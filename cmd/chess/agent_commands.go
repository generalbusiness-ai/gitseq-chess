package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"strconv"
	"strings"
)

func newAgentFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}
func hasServerFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--server" || arg == "-server" || strings.HasPrefix(arg, "--server=") || strings.HasPrefix(arg, "-server=") {
			return true
		}
	}
	return false
}
func runAgentCLI(ctx context.Context, command string, args []string, stdout io.Writer, stdin io.Reader) error {
	set := newAgentFlagSet(command)
	common := addCommon(set, true)
	action := agentAction{Action: command}
	var secretFile, after, from string
	var limit int
	switch command {
	case "create":
		set.StringVar(&action.Name, "name", "", "game display name")
		set.StringVar(&action.Color, "color", "white", "creator color")
		set.StringVar(&action.InviteKey, "invite-key", "", "opponent fingerprint")
		set.StringVar(&secretFile, "join-secret-file", "", "invitation secret file, or -")
	case "join":
		set.StringVar(&secretFile, "secret-file", "", "invitation secret file, or -")
	case "move":
		set.StringVar(&action.Move, "move", "", "UCI move")
	case "resign", "draw_offer", "draw_accept", "retry", "board", "games", "legal":
	default:
		return errors.New("this command is unsupported in server mode")
	}
	switch command {
	case "join", "move", "resign", "draw_offer", "draw_accept", "board", "legal":
		set.StringVar(&action.Game, "game", "", "game identifier")
	}
	switch command {
	case "move", "resign", "draw_offer":
		set.StringVar(&action.Predecessor, "predecessor", "", "last_move from the position examined")
	case "draw_accept":
		set.StringVar(&action.Offer, "offer", "", "draw_offer record examined")
	case "games":
		set.StringVar(&after, "after", "", "preceding page game")
		set.IntVar(&limit, "limit", 20, "page size, 1..100")
	case "legal":
		set.StringVar(&from, "from", "", "source square")
	}
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	secret, err := readSecret(secretFile, stdin)
	if err != nil {
		return err
	}
	action.Secret = secret
	action.IdempotencyKey = common.idem
	if command == "retry" && common.idem != "" {
		return errors.New("retry uses the retained idempotency key; do not supply another")
	}
	client, err := newAgentClient(common)
	if err != nil {
		return err
	}
	defer client.close()
	custody, err := openAgentCustody(ctx, common.key)
	if err != nil {
		return err
	}
	defer custody.Close()
	var result any
	switch command {
	case "board":
		result, err = agentReadTool(ctx, client, "show_board", action.Game, "", 0, "")
	case "games":
		result, err = agentReadTool(ctx, client, "list_games", "", after, limit, "")
	case "legal":
		result, err = agentReadTool(ctx, client, "legal_destinations", action.Game, "", 0, from)
	default:
		result, err = client.mutate(ctx, custody, action, command == "retry")
	}
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func agentReadTool(ctx context.Context, client *agentClient, name, game, after string, limit int, from string) (any, error) {
	query := url.Values{}
	path := ""
	switch name {
	case "list_games":
		if limit < 1 || limit > 100 || after != "" && !validEventID(after) {
			return nil, errors.New("invalid games cursor or page size")
		}
		query.Set("limit", strconv.Itoa(limit))
		if after != "" {
			query.Set("after", after)
		}
		path = "/v1/games"
	case "show_board", "legal_destinations":
		if !validEventID(game) {
			return nil, errors.New("game identifier is required")
		}
		query.Set("game", game)
		path = "/v1/board"
		if name == "legal_destinations" {
			if len(from) != 2 || from[0] < 'a' || from[0] > 'h' || from[1] < '1' || from[1] > '8' {
				return nil, errors.New("from must be a square such as e2")
			}
			query.Set("from", from)
			path = "/v1/legal"
		}
	}
	return client.read(ctx, path+"?"+query.Encode())
}

func callAgentTool(ctx context.Context, common *commonFlags, params callParams) (any, error) {
	// One decoder for the native game vocabulary. act() enforces the per-action
	// field set; read commands use their own closed forms below.
	client, err := newAgentClient(common)
	if err != nil {
		return nil, err
	}
	defer client.close()
	custody, err := openAgentCustody(ctx, common.key)
	if err != nil {
		return nil, err
	}
	defer custody.Close()
	switch params.Name {
	case "list_games":
		var args struct {
			After string `json:"after,omitempty"`
			Limit *int   `json:"limit,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &args); err != nil {
			return nil, err
		}
		limit := 20
		if args.Limit != nil {
			limit = *args.Limit
		}
		return agentReadTool(ctx, client, params.Name, "", args.After, limit, "")
	case "show_board":
		var args struct {
			Game string `json:"game"`
		}
		if err = decodeArguments(params.Arguments, &args); err != nil {
			return nil, err
		}
		return agentReadTool(ctx, client, params.Name, args.Game, "", 0, "")
	case "legal_destinations":
		var args struct {
			Game string `json:"game"`
			From string `json:"from"`
		}
		if err = decodeArguments(params.Arguments, &args); err != nil {
			return nil, err
		}
		return agentReadTool(ctx, client, params.Name, args.Game, "", 0, args.From)
	case "retry":
		var args struct{}
		if err = decodeArguments(params.Arguments, &args); err != nil {
			return nil, err
		}
		return client.mutate(ctx, custody, agentAction{}, true)
	case "create", "join", "move", "resign", "draw_offer", "draw_accept":
		var args struct {
			Game           string `json:"game,omitempty"`
			Name           string `json:"name,omitempty"`
			Color          string `json:"color,omitempty"`
			InviteKey      string `json:"invite_key,omitempty"`
			JoinSecret     string `json:"join_secret,omitempty"`
			Secret         string `json:"secret,omitempty"`
			Move           string `json:"move,omitempty"`
			Predecessor    string `json:"predecessor,omitempty"`
			Offer          string `json:"offer,omitempty"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if err = decodeArguments(params.Arguments, &args); err != nil {
			return nil, err
		}
		if params.Name == "create" {
			if args.Secret != "" {
				return nil, errors.New("create uses join_secret")
			}
			args.Secret = args.JoinSecret
		} else if args.JoinSecret != "" {
			return nil, errors.New("join_secret belongs to create")
		}
		action := agentAction{Action: params.Name, Game: args.Game, Name: args.Name, Color: args.Color, InviteKey: args.InviteKey, Secret: args.Secret, Move: args.Move, Predecessor: args.Predecessor, Offer: args.Offer, IdempotencyKey: args.IdempotencyKey}
		return client.mutate(ctx, custody, action, false)
	default:
		return nil, errors.New("tool is unsupported in server mode; no local repository was opened")
	}
}

func agentMCPTools() []map[string]any {
	var result []map[string]any
	for _, tool := range mcpTools() {
		name := tool["name"].(string)
		if name == "anchor" || name == "list_anchors" || name == "revoke_anchor" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]string)
		if name == "move" || name == "resign" || name == "draw_offer" {
			properties["predecessor"] = map[string]any{"type": "string", "description": "Exact last_move from the board examined; never refreshed on retry"}
			required = append(required, "predecessor")
		}
		if name == "draw_accept" {
			properties["offer"] = map[string]any{"type": "string", "description": "Exact draw_offer record examined"}
			required = append(required, "offer")
		}
		schema["required"] = required
		result = append(result, tool)
	}
	result = append(result, map[string]any{"name": "retry", "description": "Resubmit this key's retained signed action after an unconfirmed outcome", "inputSchema": map[string]any{"type": "object", "properties": map[string]json.RawMessage{}, "additionalProperties": false}})
	return result
}
