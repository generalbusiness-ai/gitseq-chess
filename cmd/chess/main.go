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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	case "serve":
		return runServe(ctx, args[1:], stdout)
	case "create":
		return runCreate(ctx, args[1:], stdout)
	case "join":
		return runJoin(ctx, args[1:], stdout)
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
	return errors.New("usage: chess <init|serve|create|join|move|board|resign|mcp> [options]")
}

func runInit(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	repo := set.String("repo", ".", "repository directory")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	if output, err := exec.CommandContext(ctx, "git", "init", "-q", *repo).CombinedOutput(); err != nil {
		return fmt.Errorf("initialize Git repository: %w: %s", err, strings.TrimSpace(string(output)))
	}
	keyPath, err := defaultKeyPath(ctx, *repo)
	if err != nil {
		return err
	}
	key, err := ensureKey(keyPath)
	if err != nil {
		return err
	}
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
	workspace, err := host.Open(ctx, flags.repo, application.Application)
	if err != nil {
		return nil, nil, err
	}
	path := flags.key
	if path == "" {
		path, err = defaultKeyPath(ctx, flags.repo)
		if err != nil {
			return nil, nil, err
		}
	}
	key, err := ensureKey(path)
	if err != nil {
		return nil, nil, err
	}
	return workspace, key, nil
}

func runCreate(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("create", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	color := set.String("color", "white", "creator color: white or black")
	inviteKey := set.String("invite-key", "", "invited opponent fingerprint")
	joinSecret := set.String("join-secret", "", "secret carried by the invitation link")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.Create(ctx, workspace, key, *color, *inviteKey, *joinSecret, common.idem)
	if err != nil {
		return err
	}
	result, err := actionResult(ctx, workspace, record)
	if err != nil {
		return err
	}
	result["game"] = record.ID
	if *joinSecret != "" {
		query := url.Values{"game": []string{record.ID}}
		result["invitation"] = "chess://join?" + query.Encode() + "#secret=" + url.QueryEscape(*joinSecret)
	}
	return writeJSON(stdout, result)
}

func runJoin(ctx context.Context, args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("join", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	common := addCommon(set, true)
	game := set.String("game", "", "create record identifier")
	secret := set.String("secret", "", "invitation secret")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return err
	}
	record, err := application.Join(ctx, workspace, key, *game, *secret, common.idem)
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
	server := &http.Server{
		Addr: *listen, Handler: newReadHandler(ctx, common.repo), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	fmt.Fprintln(stdout, "http://"+*listen)
	return server.ListenAndServe()
}

func newReadHandler(ctx context.Context, repo string) http.Handler {
	mux := http.NewServeMux()
	read := func() (application.Projection, error) {
		_, projection, err := application.OpenProjection(ctx, repo)
		return projection, err
	}
	mux.HandleFunc("GET /v1/games", func(w http.ResponseWriter, request *http.Request) {
		projection, err := read()
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		limit := 100
		if stated := request.URL.Query().Get("limit"); stated != "" {
			parsed, parseErr := strconv.Atoi(stated)
			if parseErr != nil || parsed < 1 || parsed > 100 {
				http.Error(w, "limit must be between 1 and 100", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		games, next := projection.GamesPage(request.URL.Query().Get("after"), limit)
		serveJSON(w, map[string]any{"games": games, "next": next, "head": projection.Head})
	})
	mux.HandleFunc("GET /v1/board", func(w http.ResponseWriter, request *http.Request) {
		projection, err := read()
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		game, ok := projection.GameByID(request.URL.Query().Get("game"))
		if !ok {
			http.Error(w, "game does not exist", http.StatusNotFound)
			return
		}
		serveJSON(w, map[string]any{"game": game, "head": projection.Head})
	})
	mux.HandleFunc("GET /v1/legal", func(w http.ResponseWriter, request *http.Request) {
		projection, err := read()
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		game, ok := projection.GameByID(request.URL.Query().Get("game"))
		if !ok {
			http.Error(w, "game does not exist", http.StatusNotFound)
			return
		}
		serveJSON(w, map[string]any{"destinations": application.LegalDestinations(game, request.URL.Query().Get("from")), "head": projection.Head})
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, request)
	})
}

func serveJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
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
		{"name": "create", "description": "Create a game", "inputSchema": object(map[string]any{"color": stringField("white or black"), "invite_key": stringField("Optional opponent fingerprint"), "join_secret": stringField("Optional invitation secret"), "idempotency_key": stringField("Stable retry key")}, "color")},
		{"name": "join", "description": "Join a game", "inputSchema": object(map[string]any{"game": game["game"], "secret": stringField("Invitation secret"), "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "move", "description": "Play a move", "inputSchema": object(map[string]any{"game": game["game"], "move": stringField("UCI move such as e2e4"), "idempotency_key": stringField("Stable retry key")}, "game", "move")},
		{"name": "resign", "description": "Resign a game", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "draw_offer", "description": "Offer a draw", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "draw_accept", "description": "Accept the pending draw", "inputSchema": object(map[string]any{"game": game["game"], "idempotency_key": stringField("Stable retry key")}, "game")},
		{"name": "anchor", "description": "Endorse a session or agent key through host identity", "inputSchema": object(map[string]any{"subject": stringField("Key fingerprint to endorse"), "scope": stringField("chess or chess:<game>"), "not_after": map[string]any{"type": "integer"}}, "subject", "scope")},
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
		return map[string]any{"destinations": application.LegalDestinations(game, arguments.From), "head": projection.Head}, nil
	}
	workspace, key, err := openWriter(ctx, common)
	if err != nil {
		return nil, err
	}
	var record host.Record
	switch params.Name {
	case "create":
		var arguments struct {
			Color          string `json:"color"`
			InviteKey      string `json:"invite_key,omitempty"`
			JoinSecret     string `json:"join_secret,omitempty"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &arguments); err == nil {
			record, err = application.Create(ctx, workspace, key, arguments.Color, arguments.InviteKey, arguments.JoinSecret, arguments.IdempotencyKey)
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
			}
		}
	default:
		return nil, errors.New("unknown tool")
	}
	if err != nil {
		return nil, err
	}
	return actionResult(ctx, workspace, record)
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
		return nil, err
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

func defaultKeyPath(ctx context.Context, repo string) (string, error) {
	command := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("locate Git directory: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return filepath.Join(strings.TrimSpace(string(output)), "chess", "player.key"), nil
}

func ensureKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("key path is required")
	}
	encoded, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(encoded)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read player key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create player key directory: %w", err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("generate player key")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			encoded, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, fmt.Errorf("read concurrently created player key: %w", readErr)
			}
			return decodeKey(encoded)
		}
		return nil, fmt.Errorf("create player key: %w", err)
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, hex.EncodeToString(private)); err != nil {
		return nil, fmt.Errorf("write player key: %w", err)
	}
	return private, nil
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
