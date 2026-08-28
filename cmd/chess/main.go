package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

const maxMCPMessage = 1 << 20

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "chess:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], stdout)
	case "rebind":
		return runRebind(ctx, args[1:], stdout, stdin)
	case "serve":
		return runServe(ctx, args[1:], stdout)
	case "create":
		return runCreate(ctx, args[1:], stdout, stdin)
	case "join":
		return runJoin(ctx, args[1:], stdout, stdin)
	case "move":
		return runMove(ctx, args[1:], stdout)
	case "board":
		return runBoard(ctx, args[1:], stdout)
	case "resign":
		return runResign(ctx, args[1:], stdout)
	case "mcp":
		return runMCP(ctx, args[1:], stdin, stdout)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: chess <init|rebind|serve|create|join|move|board|resign|mcp> [options]")
}

func runInit(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	repo := set.String("repo", ".", "repository directory")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	if err := requireKeyCustodyPlatform(); err != nil {
		return err
	}
	if output, err := exec.CommandContext(ctx, "git", "init", "-q", *repo).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize Git repository: %w: %s", err, strings.TrimSpace(string(output)))
	}
	store, err := openKeyStore(ctx, "", *repo, false)
	if err != nil {
		return err
	}
	defer store.Close()
	key, err := ensureKey(store)
	if err != nil {
		return err
	}
	keyPath := store.path()
	workspace, err := host.Init(ctx, *repo, application.Application, key, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		return err
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"repository": *repo, "genesis": log.Genesis, "player_key": keyPath})
}

var replaceApplicationBinding = host.ReplaceBinding

// runRebind moves a repository from an older chess fold understood by this
// build to the exact fold carried by this build. It deliberately reads an
// existing key instead of calling ensureKey: losing the initializing key is
// not authority to invent its replacement.
func runRebind(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	set := flag.NewFlagSet("rebind", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	repo := set.String("repo", ".", "chess repository")
	keyPath := set.String("key", "", "initializing player private-key file")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	if err := requireKeyCustodyPlatform(); err != nil {
		return err
	}

	outgoing, log, err := readableOlderChessLog(ctx, *repo)
	if err != nil {
		return err
	}
	store, err := openKeyStore(ctx, *keyPath, *repo, *keyPath != "")
	if err != nil {
		return err
	}
	defer store.Close()
	key, err := readKey(store)
	if err != nil {
		return fmt.Errorf("read initializing player key: %w", err)
	}
	if len(log.Records) == 0 || !bytes.Equal(key.Public().(ed25519.PublicKey), log.Records[0].ActorKey) {
		return errors.New("binding replacement requires the initializing actor's key")
	}

	if err := confirmRebind(stdin, stdout, outgoing, application.FoldVersion); err != nil {
		return err
	}
	replacement, err := replaceApplicationBinding(ctx, *repo, application.Application, key)
	if err != nil {
		return err
	}
	if _, err := host.Open(ctx, *repo, application.Application); err != nil {
		return fmt.Errorf("binding was replaced but this build cannot open the repository: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "Rebound chess repository from %s to %s.\n", replacement.OutgoingFoldVersion, replacement.IncomingFoldVersion)
	return err
}

// readableOlderChessLog accepts only historical folds whose record vocabulary
// the current fold still implements. Opening under the outgoing identity first
// also verifies the sequence without weakening host.Open's exact binding rule.
func readableOlderChessLog(ctx context.Context, repo string) (string, host.Log, error) {
	if _, err := host.Open(ctx, repo, application.Application); err == nil {
		return "", host.Log{}, fmt.Errorf("repository is already bound to %s", application.FoldVersion)
	} else if !errors.Is(err, host.ErrUninterpretable) {
		return "", host.Log{}, err
	}
	for _, version := range []string{"chess-fold@1", "chess-fold@0"} {
		older := application.Application
		older.FoldVersion = version
		workspace, err := host.Open(ctx, repo, older)
		if err == nil {
			log, err := workspace.Records(ctx)
			if err != nil {
				return "", host.Log{}, err
			}
			// Folding before the replacement proves this build can read the target
			// record stream. Fold is pure and represents malformed acts as refusals.
			application.Fold(log)
			return version, log, nil
		}
		if !errors.Is(err, host.ErrUninterpretable) {
			return "", host.Log{}, err
		}
	}
	return "", host.Log{}, errors.New("this build cannot interpret the repository binding as a supported older chess fold")
}

func confirmRebind(stdin io.Reader, stdout io.Writer, outgoing, incoming string) error {
	if _, err := fmt.Fprintf(stdout, "Warning: records in this repository were folded under %s; after rebinding to %s, some records may be interpreted differently.\nType rebind to continue: ", outgoing, incoming); err != nil {
		return err
	}
	answer, tooLong, err := bufio.NewReaderSize(stdin, 33).ReadLine()
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read rebind confirmation: %w", err)
	}
	if tooLong || strings.TrimSuffix(string(answer), "\r") != "rebind" {
		return errors.New("binding replacement was not confirmed; type rebind exactly")
	}
	return nil
}

type commonFlags struct {
	repo string
	key  string
	idem string
}

func addCommon(set *flag.FlagSet, write bool) *commonFlags {
	flags := &commonFlags{}
	set.StringVar(&flags.repo, "repo", ".", "chess repository")
	if write {
		set.StringVar(&flags.key, "key", "", "player private-key file")
		set.StringVar(&flags.idem, "idempotency-key", "", "stable retry key")
	}
	return flags
}

func openWriter(ctx context.Context, flags *commonFlags) (*host.Workspace, ed25519.PrivateKey, error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return nil, nil, err
	}
	workspace, err := host.Open(ctx, flags.repo, application.Application)
	if err != nil {
		return nil, nil, err
	}
	store, err := openKeyStore(ctx, flags.key, flags.repo, flags.key != "")
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	key, err := ensureKey(store)
	if err != nil {
		return nil, nil, err
	}
	return workspace, key, nil
}

func runCreate(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	set := flag.NewFlagSet("create", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	name := set.String("name", "", "short display name for the game")
	color := set.String("color", "white", "creator color: white or black")
	inviteKey := set.String("invite-key", "", "invited opponent fingerprint")
	// The secret is read from a file or from standard input, never taken as a
	// flag value: an argument is visible in the process table to every account
	// on the machine for as long as the command runs.
	joinSecretFile := set.String("join-secret-file", "", "file holding the invitation secret, or - for standard input")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	joinSecret, err := readSecret(*joinSecretFile, stdin)
	if err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.CreateNamed(ctx, workspace, key, *name, *color, *inviteKey, joinSecret, common.idem)
	if err != nil {
		return err
	}
	result, err := actionResult(ctx, workspace, record)
	if err != nil {
		return err
	}
	result["game"] = record.ID
	if joinSecret != "" {
		// The invitation is the caller's to pass on out of band. It carries the
		// secret in a fragment, which no correct client sends to a server.
		query := url.Values{"game": []string{record.ID}}
		result["invitation"] = "chess://join?" + query.Encode() + "#secret=" + url.QueryEscape(joinSecret)
	}
	return writeJSON(stdout, result)
}

func runJoin(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	set := flag.NewFlagSet("join", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	game := set.String("game", "", "create record identifier")
	secretFile := set.String("secret-file", "", "file holding the invitation secret, or - for standard input")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	secret, err := readSecret(*secretFile, stdin)
	if err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.Join(ctx, workspace, key, *game, secret, common.idem)
	if err != nil {
		return err
	}
	return writeActionResult(ctx, stdout, workspace, record)
}

func runMove(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("move", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	game := set.String("game", "", "game identifier")
	move := set.String("move", "", "move in UCI notation, for example e2e4")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.Move(ctx, workspace, key, *game, *move, common.idem)
	if err != nil {
		return err
	}
	return writeActionResult(ctx, stdout, workspace, record)
}

func runResign(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("resign", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	game := set.String("game", "", "game identifier")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.Resign(ctx, workspace, key, *game, common.idem)
	if err != nil {
		return err
	}
	return writeActionResult(ctx, stdout, workspace, record)
}

func runBoard(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("board", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, false)
	gameID := set.String("game", "", "game identifier")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	_, projection, err := application.OpenProjection(ctx, common.repo)
	if err != nil {
		return err
	}
	game, ok := projection.GameByID(*gameID)
	if !ok {
		return errors.New("game does not exist")
	}
	return writeJSON(stdout, game)
}

func runServe(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, false)
	listen := set.String("listen", "127.0.0.1:8080", "HTTP listen address")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	if !validLoopbackListen(*listen) {
		return errors.New("serve listen address must use localhost or a loopback IP")
	}
	if _, _, err := application.OpenProjection(ctx, common.repo); err != nil {
		return fmt.Errorf("open chess repository %q: %w", common.repo, err)
	}
	identityConfig, err := identityHTTPConfigFromEnvironment(ctx, common.repo)
	if err != nil {
		return err
	}
	runtime, err := newChessLive()
	if err != nil {
		return errors.New("live runtime is unavailable")
	}
	server := newChessHTTPServer(*listen, newReadHandlerWithIdentity(ctx, common.repo, runtime, identityConfig))
	fmt.Fprintln(stdout, "http://"+*listen)
	return server.ListenAndServe()
}

func newChessHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: liveWriteTimeout, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10,
	}
}

func validLoopbackListen(value string) bool {
	host, _, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	set := flag.NewFlagSet("mcp", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 4096), maxMCPMessage)
	encoder := json.NewEncoder(stdout)
	for scanner.Scan() {
		var request rpcRequest
		if err := strictJSON(scanner.Bytes(), &request); err != nil {
			if err := encoder.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "invalid JSON-RPC request"}}); err != nil {
				return err
			}
			continue
		}
		response, respond := handleRPC(ctx, common, request)
		if respond {
			if err := encoder.Encode(response); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func parseNoPositionals(set *flag.FlagSet, arguments []string) error {
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	return nil
}

func handleRPC(ctx context.Context, common *commonFlags, request rpcRequest) (rpcResponse, bool) {
	respond := len(request.ID) != 0 && string(request.ID) != "null"
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	if request.JSONRPC != "2.0" || request.Method == "" {
		response.Error = &rpcError{Code: -32600, Message: "invalid JSON-RPC request"}
		return response, respond
	}
	switch request.Method {
	case "notifications/initialized":
		return response, false
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "gitseq-chess", "version": application.FoldVersion},
		}
	case "tools/list":
		response.Result = map[string]any{"tools": mcpTools()}
	case "tools/call":
		var params callParams
		if err := strictJSON(request.Params, &params); err != nil {
			response.Error = &rpcError{Code: -32602, Message: "invalid tool arguments"}
			break
		}
		result, err := callTool(ctx, common, params)
		if err != nil {
			response.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}
		} else {
			encoded, _ := json.Marshal(result)
			response.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(encoded)}}, "structuredContent": result}
		}
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return response, respond
}

func mcpTools() []map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	stringField := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	game := map[string]any{"game": stringField("The create record identifier")}
	return []map[string]any{
		{"name": "list_games", "description": "List a bounded page of games at the verified frontier", "inputSchema": object(map[string]any{"after": stringField("Last game identifier from the preceding page"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}})},
		{"name": "show_board", "description": "Show one folded game", "inputSchema": object(game, "game")},
		{"name": "legal_destinations", "description": "List legal destinations from a square", "inputSchema": object(map[string]any{"game": game["game"], "from": stringField("Source square such as e2")}, "game", "from")},
		{"name": "create", "description": "Create a named game", "inputSchema": object(map[string]any{"name": stringField("Optional short display name"), "color": stringField("white or black"), "invite_key": stringField("Optional opponent fingerprint"), "join_secret": stringField("Optional invitation secret"), "idempotency_key": stringField("Stable retry key")}, "color")},
		{"name": "join", "description": "Join a game", "inputSchema": object(map[string]any{"game": game["game"], "secret": stringField("Invitation secret"), "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "move", "description": "Play a move", "inputSchema": object(map[string]any{"game": game["game"], "move": stringField("UCI move such as e2e4"), "idempotency_key": stringField("Stable retry key")}, "game", "move")},
		{"name": "resign", "description": "Resign a game", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "draw_offer", "description": "Offer a draw", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "draw_accept", "description": "Accept the pending draw", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "anchor", "description": "Endorse a session or agent key through host identity", "inputSchema": object(map[string]any{"subject": stringField("Key fingerprint to endorse"), "scope": stringField("chess or chess:<game>"), "not_after": map[string]any{"type": "integer"}}, "subject", "scope")},
		{"name": "list_anchors", "description": "List a bounded set of standing host identity anchors", "inputSchema": object(map[string]any{"subject": stringField("Exact endorsed key fingerprint"), "scope": stringField("Exact chess or chess:<game> scope"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}})},
		{"name": "revoke_anchor", "description": "Withdraw an identity anchor or delegated credential", "inputSchema": object(map[string]any{"record": stringField("Anchor or credential record identifier")}, "record")},
	}
}

func callTool(ctx context.Context, common *commonFlags, params callParams) (any, error) {
	switch params.Name {
	case "list_games":
		var arguments struct {
			After string `json:"after,omitempty"`
			Limit *int   `json:"limit,omitempty"`
		}
		if err := decodeArguments(params.Arguments, &arguments); err != nil {
			return nil, err
		}
		_, projection, err := application.OpenProjection(ctx, common.repo)
		if err != nil {
			return nil, err
		}
		limit := 100
		if arguments.Limit != nil {
			if *arguments.Limit < 1 || *arguments.Limit > 100 {
				return nil, errors.New("limit must be between 1 and 100")
			}
			limit = *arguments.Limit
		}
		games, next := projection.GamesPage(arguments.After, limit)
		return map[string]any{"games": games, "next": next, "head": projection.Head}, nil
	case "show_board":
		var arguments struct {
			Game string `json:"game"`
		}
		if err := decodeArguments(params.Arguments, &arguments); err != nil {
			return nil, err
		}
		if arguments.Game == "" {
			return nil, errors.New("game must be a non-empty string")
		}
		_, projection, err := application.OpenProjection(ctx, common.repo)
		if err != nil {
			return nil, err
		}
		game, ok := projection.GameByID(arguments.Game)
		if !ok {
			return nil, errors.New("game does not exist")
		}
		return map[string]any{"game": game, "head": projection.Head}, nil
	case "legal_destinations":
		var arguments struct {
			Game string `json:"game"`
			From string `json:"from"`
		}
		if err := decodeArguments(params.Arguments, &arguments); err != nil {
			return nil, err
		}
		if arguments.Game == "" || arguments.From == "" {
			return nil, errors.New("game and from must be non-empty strings")
		}
		_, projection, err := application.OpenProjection(ctx, common.repo)
		if err != nil {
			return nil, err
		}
		game, ok := projection.GameByID(arguments.Game)
		if !ok {
			return nil, errors.New("game does not exist")
		}
		selection := application.LegalSelection(game, arguments.From)
		return map[string]any{"destinations": selection.Destinations, "reason": selection.Reason, "head": projection.Head}, nil
	case "list_anchors":
		var arguments struct {
			Subject string `json:"subject,omitempty"`
			Scope   string `json:"scope,omitempty"`
			Limit   *int   `json:"limit,omitempty"`
		}
		if err := decodeArguments(params.Arguments, &arguments); err != nil {
			return nil, err
		}
		limit := 100
		if arguments.Limit != nil {
			limit = *arguments.Limit
		}
		workspace, err := host.Open(ctx, common.repo, application.Application)
		if err != nil {
			return nil, err
		}
		page, err := application.ListAnchors(ctx, workspace, arguments.Subject, arguments.Scope, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"anchors": page.Anchors, "head": page.Head, "truncated": page.Truncated}, nil
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return nil, err
	}
	var record host.Record
	identityMutation := false
	switch params.Name {
	case "create":
		var arguments struct {
			Name           string `json:"name,omitempty"`
			Color          string `json:"color"`
			InviteKey      string `json:"invite_key,omitempty"`
			JoinSecret     string `json:"join_secret,omitempty"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			record, err = application.CreateNamed(ctx, workspace, key, arguments.Name, arguments.Color, arguments.InviteKey, arguments.JoinSecret, arguments.IdempotencyKey)
		}
	case "join":
		var arguments struct {
			Game           string `json:"game"`
			Secret         string `json:"secret,omitempty"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			record, err = application.Join(ctx, workspace, key, arguments.Game, arguments.Secret, arguments.IdempotencyKey)
		}
	case "move":
		var arguments struct {
			Game           string `json:"game"`
			Move           string `json:"move"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			record, err = application.Move(ctx, workspace, key, arguments.Game, arguments.Move, arguments.IdempotencyKey)
		}
	case "resign", "draw_offer", "draw_accept":
		var arguments struct {
			Game           string `json:"game"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			switch params.Name {
			case "resign":
				record, err = application.Resign(ctx, workspace, key, arguments.Game, arguments.IdempotencyKey)
			case "draw_offer":
				record, err = application.OfferDraw(ctx, workspace, key, arguments.Game, arguments.IdempotencyKey)
			case "draw_accept":
				record, err = application.AcceptDraw(ctx, workspace, key, arguments.Game, arguments.IdempotencyKey)
			}
		}
	case "anchor":
		var arguments struct {
			Subject  string `json:"subject"`
			Scope    string `json:"scope"`
			NotAfter int64  `json:"not_after,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			if arguments.NotAfter < 0 {
				err = errors.New("not_after must be a non-negative integer")
			} else if arguments.Subject == "" || arguments.Scope == "" {
				err = errors.New("subject and scope are required")
			} else {
				record, err = application.Anchor(ctx, workspace, key, identity.Anchor{Subject: arguments.Subject, Scope: arguments.Scope, NotAfter: arguments.NotAfter})
				identityMutation = true
			}
		}
	case "revoke_anchor":
		var arguments struct {
			Record string `json:"record"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			if arguments.Record == "" {
				err = errors.New("record is required")
			} else {
				record, err = application.RevokeAnchor(ctx, workspace, key, arguments.Record)
				identityMutation = true
			}
		}
	default:
		return nil, errors.New("unknown tool")
	}
	if err != nil {
		return nil, err
	}
	if identityMutation {
		return identityActionResult(application.IdentityOutcome(ctx, workspace, record)), nil
	}
	return actionResult(ctx, workspace, record)
}

func identityActionResult(outcome application.IdentityMutation) map[string]any {
	result := map[string]any{"record": outcome.Record, "outcome": outcome.Outcome}
	switch outcome.Outcome {
	case "created", "revoked":
		result["effective"] = true
	case "refused":
		result["effective"] = false
	default:
		result["effective"] = nil
	}
	if outcome.Reason != "" {
		result["reason"] = outcome.Reason
	}
	return result
}

func decodeArguments(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("arguments do not match the tool schema")
	}
	if err := strictJSON(data, target); err != nil {
		return errors.New("arguments do not match the tool schema")
	}
	return nil
}

func actionResult(ctx context.Context, workspace *host.Workspace, record host.Record) (map[string]any, error) {
	effective, found, reason, err := application.Decision(ctx, workspace, record.ID)
	if err != nil {
		return nil, fmt.Errorf("record %s was durably appended, but its decision could not be read: %w", record.ID, err)
	}
	result := map[string]any{"record": record.ID, "effective": effective}
	if !found {
		result["effective"] = nil
	}
	if reason != "" {
		result["reason"] = reason
	}
	return result, nil
}

func writeActionResult(ctx context.Context, output io.Writer, workspace *host.Workspace, record host.Record) error {
	result, err := actionResult(ctx, workspace, record)
	if err != nil {
		return err
	}
	return writeJSON(output, result)
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func gitCommonDir(ctx context.Context, repo string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("locate Git directory: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// keyFileMode is the exact mode used for every key this program publishes.
// Existing operator-managed keys may instead be read-only mode 0400.
const keyFileMode = 0o600

// maxSecretBytes bounds a secret read from a file or from standard input, so
// a wrong path cannot make the process read something enormous.
const maxSecretBytes = 4 << 10

// keyStore is the directory holding a player key, kept open for the whole
// operation.
//
// Checking a path and then using it by name is two different questions asked
// at two different moments, and anything under the attacker's control can move
// in between. os.Root keeps a descriptor on the directory and resolves every
// name beneath it, so a component swapped after the check does not change what
// is opened, and a symbolic link cannot lead outside the root at all.
//
// Refusing a link that stays inside the root is separate work, because os.Root
// follows those by contract. Each such refusal is made by naming the thing,
// opening it, and proving the two answers describe the same file: a static
// link is refused outright, and a link swapped in mid-operation can only pass
// if it resolves to the very inode that was named.
type keyStore struct {
	root *os.Root
	name string
	// link publishes the finished temporary file under the key's real name.
	// It is a field so a test can interrupt at exactly that boundary; nothing
	// in the program replaces it.
	link func(temporary, name string) error
}

// openKeyStore decides which directory bounds the key.
//
// A path this program chose is bounded by the Git common directory, so no
// component of it may be a link out. A path the operator named with --key is
// bounded only by its own parent: they chose where it lives, and the checks
// that remain are the ones protecting the key file, not its location.
func openKeyStore(ctx context.Context, path, repo string, named bool) (*keyStore, error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return nil, err
	}
	if named && path == "" {
		return nil, errors.New("key path is required")
	}
	if named {
		root, err := os.OpenRoot(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("open key directory: %w", err)
		}
		return newKeyStore(root, filepath.Base(path)), nil
	}
	common, err := gitCommonDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	outer, err := os.OpenRoot(common)
	if err != nil {
		return nil, fmt.Errorf("open Git directory: %w", err)
	}
	defer outer.Close()
	inner, err := openDirectory(outer, "chess")
	if err != nil {
		return nil, err
	}
	return newKeyStore(inner, "player.key"), nil
}

// openDirectory pins one directory beneath root, refusing a symbolic link
// standing in for it.
//
// os.Root refuses a link that leaves the root and follows one that does not,
// so it cannot make this refusal on its own; measured on Go 1.26, both
// Root.OpenFile and Root.OpenRoot follow an in-root link. The refusal is made
// by asking what the name is, opening it, and proving the two answers describe
// the same directory. A swap in between makes them disagree.
func openDirectory(root *os.Root, name string) (*os.Root, error) {
	return openNamedDirectory(root, name, root.Mkdir)
}

// openNamedDirectory takes the mkdir it should use so a test can lose the
// creation race deterministically rather than hoping two goroutines collide.
func openNamedDirectory(root *os.Root, name string, mkdir func(string, os.FileMode) error) (*os.Root, error) {
	named, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		// Mkdir fails if anything already holds the name, so winning it means
		// no link can be in the way. Losing it to another process is ordinary,
		// not an error: fall through and check what they made the same way.
		if err := mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create player key directory: %w", err)
		}
		named, err = root.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	return adoptDirectory(root, name, named)
}

// adoptDirectory finishes what openNamedDirectory started, taking the answer
// Lstat already gave. It is separate so a test can hold that answer, replace
// what the name refers to, and prove the mismatch is refused; the race is
// otherwise unreachable on purpose.
func adoptDirectory(root *os.Root, name string, named os.FileInfo) (*os.Root, error) {
	shown := filepath.Join(root.Name(), name)
	if named.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; refusing to keep a private key beneath it", shown)
	}
	if !named.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", shown)
	}
	opened, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	current, err := opened.Stat(".")
	if err != nil {
		opened.Close()
		return nil, err
	}
	if !os.SameFile(named, current) {
		opened.Close()
		return nil, fmt.Errorf("%s changed while it was being opened; refusing to use it", shown)
	}
	// A directory anyone else may write to is a directory whose names anyone
	// else controls. Pinning it stops it being swapped for another; it does
	// nothing about a neighbour creating, removing or replacing the key inside
	// the very directory we pinned.
	//
	// The mode is read from the open descriptor, not from the Lstat above.
	// Chmod does not change an inode's identity, so a directory made writable
	// after that snapshot still satisfies SameFile; only the descriptor says
	// what the directory permits now.
	if permission := current.Mode().Perm(); permission&0o022 != 0 {
		opened.Close()
		return nil, fmt.Errorf("%s is mode %04o; a private key must not be kept in a directory that group or others can write", shown, permission)
	}
	return opened, nil
}

func newKeyStore(root *os.Root, name string) *keyStore {
	store := &keyStore{root: root, name: name}
	store.link = func(temporary, name string) error { return store.root.Link(temporary, name) }
	return store
}

func (s *keyStore) Close() error { return s.root.Close() }

// path is for messages only. Never reopen by it.
func (s *keyStore) path() string { return filepath.Join(s.root.Name(), s.name) }

// ensureKey loads the player key, or publishes a new one.
func ensureKey(store *keyStore) (ed25519.PrivateKey, error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return nil, err
	}
	private, err := readKey(store)
	if err == nil {
		return private, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// The directory is pinned by openKeyStore, and Root.Chmod is documented as
	// racy on Unix, so it is not adjusted here. A umask can only narrow the
	// mode, and a directory too narrow to enter fails closed.
	_, generated, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("generate player key")
	}
	if err := publishKey(store, generated); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Somebody else published first. Theirs is the key.
			return readKey(store)
		}
		return nil, err
	}
	return generated, nil
}

// readKey reads an existing 0400 or 0600 key, refusing one reached through a
// symbolic link or carrying any other mode.
func readKey(store *keyStore) (ed25519.PrivateKey, error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return nil, err
	}
	// os.Root follows a link that stays inside the root, so the refusal is
	// made here: ask what the name is, open it, and then prove the thing
	// opened is the thing asked about. A swap between the two answers makes
	// them disagree, and disagreement is a refusal.
	named, err := store.root.Lstat(store.name)
	if err != nil {
		return nil, err
	}
	return readNamedKey(store, named)
}

// readNamedKey finishes what readKey started, taking the answer Lstat already
// gave. It is separate so a test can hold that answer, replace what the name
// refers to, and prove the mismatch is refused.
func readNamedKey(store *keyStore, named os.FileInfo) (ed25519.PrivateKey, error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return nil, err
	}
	if named.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; refusing to follow it to a private key", store.path())
	}
	if !named.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", store.path())
	}
	file, err := store.root.OpenFile(store.name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read player key: %w", err)
	}
	if !os.SameFile(named, info) {
		return nil, fmt.Errorf("%s changed while it was being opened; refusing to read it", store.path())
	}
	// A key this program publishes is exactly 0600. An existing key may be
	// owner-read-only 0400, but no other mode carries the custody contract.
	if permission := info.Mode().Perm(); permission != 0o400 && permission != keyFileMode {
		return nil, fmt.Errorf("player key %s is mode %04o; existing keys must be mode 0400 or 0600", store.path(), permission)
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 4<<10))
	if err != nil {
		return nil, fmt.Errorf("read player key: %w", err)
	}
	return decodeKey(encoded)
}

// publishKey writes a key beside its destination and links it into place, so
// the name never exists holding a half-written key. Link fails rather than
// replacing an existing file, so a concurrent publisher cannot be overwritten
// and the loser can simply read the winner's key.
func publishKey(store *keyStore, private ed25519.PrivateKey) (err error) {
	if err := requireKeyCustodyPlatform(); err != nil {
		return err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("create player key: %w", err)
	}
	temporary := "." + hex.EncodeToString(suffix[:]) + ".key"
	file, openErr := store.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if openErr != nil {
		return fmt.Errorf("create player key: %w", openErr)
	}
	// The temporary name holds a second copy of the private key. Failing to
	// remove it is not tidying left undone, it is a private key left on disk
	// under a name nobody will look at, so the failure is reported even when
	// everything else worked. It deliberately does not wrap os.ErrExist: the
	// caller reads that as "someone else published first" and would go on to
	// read the winner's key, swallowing this.
	defer func() {
		if removeErr := store.root.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = fmt.Errorf("remove temporary player key %s: %v", filepath.Join(store.root.Name(), temporary), removeErr)
		}
	}()
	// The mode passed to OpenFile above is filtered by the process umask, so
	// it is set again on the descriptor to be the mode this program promised.
	if err := file.Chmod(keyFileMode); err != nil {
		file.Close()
		return fmt.Errorf("secure player key: %w", err)
	}
	if _, err := fmt.Fprintln(file, hex.EncodeToString(private)); err != nil {
		file.Close()
		return fmt.Errorf("write player key: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("write player key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write player key: %w", err)
	}
	return store.link(temporary, store.name)
}

// readSecret takes an invitation secret from a file, or from standard input
// when the name is "-". Passing a secret as a flag value publishes it in the
// process table to every account on the machine, which is why there is no
// flag that carries the value itself.
//
// Naming a source and getting nothing from it is an error, not an absent
// secret. Treating it as absent would turn a typo into an open invitation that
// anyone can accept.
func readSecret(name string, stdin io.Reader) (string, error) {
	if name == "" {
		return "", nil
	}
	source := stdin
	if name != "-" {
		file, err := os.Open(name)
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		defer file.Close()
		source = file
	}
	encoded, err := io.ReadAll(io.LimitReader(source, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	if len(encoded) > maxSecretBytes {
		return "", fmt.Errorf("secret is longer than %d bytes", maxSecretBytes)
	}
	secret := strings.TrimRight(string(encoded), "\r\n")
	if secret == "" {
		return "", errors.New("secret source is empty; omit the flag to create an open invitation")
	}
	return secret, nil
}

func decodeKey(encoded []byte) (ed25519.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("player key file does not contain one Ed25519 private key")
	}
	return ed25519.PrivateKey(raw), nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
