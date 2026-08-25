package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	application "github.com/generalbusiness-ai/gitseq-chess"
)

func requireWritableKeyCustody(t *testing.T) {
	t.Helper()
	if err := requireKeyCustodyPlatform(); err != nil {
		t.Skip(err)
	}
}

func TestCommandsInitializeAndPlayThroughThePublicHost(t *testing.T) {
	requireWritableKeyCustody(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "data")
	call := func(arguments ...string) map[string]any {
		t.Helper()
		var output bytes.Buffer
		if err := run(ctx, arguments, &output, bytes.NewReader(nil)); err != nil {
			t.Fatalf("chess %v: %v", arguments, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			t.Fatalf("decode output %q: %v", output.String(), err)
		}
		return decoded
	}

	initialized := call("init", "--repo", repo)
	if initialized["genesis"] == "" || initialized["player_key"] == "" {
		t.Fatalf("init output = %+v", initialized)
	}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("one use\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created := call("create", "--repo", repo, "--color", "white", "--join-secret-file", secretFile)
	game, ok := created["game"].(string)
	if !ok || game == "" || created["effective"] != true || created["invitation"] == "" {
		t.Fatalf("create output = %+v", created)
	}
	invitation, err := url.Parse(created["invitation"].(string))
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(invitation.Fragment)
	if err != nil || invitation.Query().Get("game") != game || fragment.Get("secret") != "one use" {
		t.Fatalf("invitation %q round-trips as game %q secret %q: %v", invitation, invitation.Query().Get("game"), fragment.Get("secret"), err)
	}
	bob := filepath.Join(t.TempDir(), "bob.key")
	joined := call("join", "--repo", repo, "--key", bob, "--game", game, "--secret-file", secretFile)
	if joined["effective"] != true {
		t.Fatalf("join output = %+v", joined)
	}
	wrongTurn := call("move", "--repo", repo, "--key", bob, "--game", game, "--move", "e7e5")
	if wrongTurn["effective"] != false || wrongTurn["reason"] != "actor does not hold the side to move" {
		t.Fatalf("wrong-turn output = %+v", wrongTurn)
	}
	whiteMove := call("move", "--repo", repo, "--game", game, "--move", "e2e4")
	if whiteMove["effective"] != true {
		t.Fatalf("white move output = %+v", whiteMove)
	}
	blackMove := call("move", "--repo", repo, "--key", bob, "--game", game, "--move", "e7e5")
	if blackMove["effective"] != true {
		t.Fatalf("black move output = %+v", blackMove)
	}
	board := call("board", "--repo", repo, "--game", game)
	if board["moves"] != float64(2) || board["turn"] != "w" {
		t.Fatalf("board output = %+v", board)
	}
	resigned := call("resign", "--repo", repo, "--key", bob, "--game", game)
	if resigned["effective"] != true {
		t.Fatalf("resign output = %+v", resigned)
	}
	finished := call("board", "--repo", repo, "--game", game)
	if finished["status"] != "finished" || finished["outcome"] != "white-wins" || finished["method"] != "resignation" {
		t.Fatalf("finished board = %+v", finished)
	}
	if err := run(ctx, []string{"board", "--repo", repo, "--game", game, "ignored"}, io.Discard, bytes.NewReader(nil)); err == nil {
		t.Fatal("board silently accepted an extra positional argument")
	}
}

func TestMCPInitializeAndListsTheWholeDurableAndQuerySurface(t *testing.T) {
	initialized, respond := handleRPC(context.Background(), &commonFlags{}, rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize",
	})
	if !respond || initialized.Error != nil {
		t.Fatalf("initialize = %+v, respond %v", initialized, respond)
	}
	initResult, ok := initialized.Result.(map[string]any)
	if !ok || initResult["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize result = %+v", initialized.Result)
	}

	response, respond := handleRPC(context.Background(), &commonFlags{}, rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/list",
	})
	if !respond || response.Error != nil {
		t.Fatalf("tools/list = %+v, respond %v", response, respond)
	}
	result, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list result = %T", response.Result)
	}
	tools, ok := result["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools = %T", result["tools"])
	}
	want := map[string]bool{
		"list_games": false, "show_board": false, "legal_destinations": false,
		"create": false, "join": false, "move": false, "resign": false,
		"draw_offer": false, "draw_accept": false, "anchor": false,
	}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if _, expected := want[name]; expected {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("MCP does not expose %s", name)
		}
	}
}

func TestReadOnlySurfacesDoNotRequireKeyCustody(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "missing-repository")

	err := run(ctx, []string{"board", "--repo", repo, "--game", "missing"}, io.Discard, bytes.NewReader(nil))
	if err == nil || strings.Contains(err.Error(), "player-key custody") {
		t.Fatalf("board error = %v", err)
	}

	params, err := json.Marshal(callParams{Name: "list_games", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	response, respond := handleRPC(ctx, &commonFlags{repo: repo}, rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params,
	})
	if !respond || response.Error != nil {
		t.Fatalf("list_games = %+v, respond %v", response, respond)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"isError":true`) ||
		strings.Contains(string(encoded), "player-key custody") {
		t.Fatalf("list_games result = %s", encoded)
	}

	handler := newReadHandler(ctx, repo)
	request := httptest.NewRequest(http.MethodGet, "/v1/games", nil)
	httpResponse := httptest.NewRecorder()
	handler.ServeHTTP(httpResponse, request)
	if httpResponse.Code != http.StatusServiceUnavailable ||
		httpResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read service status %d headers %v", httpResponse.Code, httpResponse.Header())
	}
}

func TestMCPToolsCallEveryActAndBoundedQueries(t *testing.T) {
	requireWritableKeyCustody(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "data")
	var discarded bytes.Buffer
	if err := run(ctx, []string{"init", "--repo", repo}, &discarded, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	alice := &commonFlags{repo: repo}
	bob := &commonFlags{repo: repo, key: filepath.Join(t.TempDir(), "bob.key")}
	call := func(actor *commonFlags, name string, arguments map[string]any) map[string]any {
		t.Helper()
		encodedArguments, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		params, err := json.Marshal(callParams{Name: name, Arguments: encodedArguments})
		if err != nil {
			t.Fatal(err)
		}
		response, respond := handleRPC(ctx, actor, rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
		if !respond || response.Error != nil {
			t.Fatalf("%s response = %+v, respond %v", name, response, respond)
		}
		result, ok := response.Result.(map[string]any)
		if !ok || result["isError"] == true {
			t.Fatalf("%s result = %+v", name, response.Result)
		}
		structured, ok := result["structuredContent"].(map[string]any)
		if !ok {
			t.Fatalf("%s structured result = %T", name, result["structuredContent"])
		}
		return structured
	}

	create := call(alice, "create", map[string]any{"color": "white", "join_secret": "mcp invite"})
	game, _ := create["game"].(string)
	if game == "" {
		// Tool create returns record; a create record is the game identifier.
		game, _ = create["record"].(string)
	}
	if game == "" || create["effective"] != true {
		t.Fatalf("create = %+v", create)
	}
	if joined := call(bob, "join", map[string]any{"game": game, "secret": "mcp invite"}); joined["effective"] != true {
		t.Fatalf("join = %+v", joined)
	}
	if moved := call(alice, "move", map[string]any{"game": game, "move": "e2e4"}); moved["effective"] != true {
		t.Fatalf("move = %+v", moved)
	}
	legal := call(alice, "legal_destinations", map[string]any{"game": game, "from": "e7"})
	destinations, ok := legal["destinations"].([]string)
	if !ok || !reflect.DeepEqual(destinations, []string{"e5", "e6"}) {
		t.Fatalf("legal destinations = %#v", legal["destinations"])
	}
	board := call(alice, "show_board", map[string]any{"game": game})
	shown, ok := board["game"].(application.Game)
	if !ok || shown.LastMoveUCI != "e2e4" {
		t.Fatalf("board = %+v", board)
	}
	if offered := call(alice, "draw_offer", map[string]any{"game": game}); offered["effective"] != true {
		t.Fatalf("draw offer = %+v", offered)
	}
	if accepted := call(bob, "draw_accept", map[string]any{"game": game}); accepted["effective"] != true {
		t.Fatalf("draw accept = %+v", accepted)
	}

	second := call(alice, "create", map[string]any{"color": "black"})
	secondGame, _ := second["record"].(string)
	if joined := call(bob, "join", map[string]any{"game": secondGame}); joined["effective"] != true {
		t.Fatalf("second join = %+v", joined)
	}
	if resigned := call(bob, "resign", map[string]any{"game": secondGame}); resigned["effective"] != true {
		t.Fatalf("resign = %+v", resigned)
	}
	anchored := call(alice, "anchor", map[string]any{"subject": "agent-fingerprint", "scope": "chess"})
	if anchored["record"] == "" || anchored["effective"] != nil {
		t.Fatalf("host anchor = %+v", anchored)
	}

	page := call(alice, "list_games", map[string]any{"limit": float64(1)})
	games, ok := page["games"].([]application.Game)
	if !ok || len(games) != 1 || page["next"] == "" {
		t.Fatalf("first games page = %+v", page)
	}
	next := page["next"].(string)
	lastPage := call(alice, "list_games", map[string]any{"limit": float64(1), "after": next})
	lastGames, ok := lastPage["games"].([]application.Game)
	if !ok || len(lastGames) != 1 || lastPage["next"] != "" {
		t.Fatalf("last games page = %+v", lastPage)
	}
}

func TestMCPToolArgumentsCannotSilentlyWidenAnInvitation(t *testing.T) {
	ctx := context.Background()
	t.Run("read tool schema", func(t *testing.T) {
		showArgs, err := json.Marshal(callParams{
			Name: "show_board", Arguments: json.RawMessage(`{"game":"missing","from":"e2"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		response, _ := handleRPC(ctx, &commonFlags{}, rpcRequest{
			JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: showArgs,
		})
		result, ok := response.Result.(map[string]any)
		if !ok || result["isError"] != true {
			t.Fatalf("show_board accepted legal_destinations-only field: %+v", response)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "player-key custody") {
			t.Fatalf("read tool schema crossed the custody boundary: %s", encoded)
		}
	})

	t.Run("write tool schema", func(t *testing.T) {
		requireWritableKeyCustody(t)
		repo := filepath.Join(t.TempDir(), "data")
		if err := run(ctx, []string{"init", "--repo", repo}, io.Discard, bytes.NewReader(nil)); err != nil {
			t.Fatal(err)
		}
		common := &commonFlags{repo: repo}
		for name, arguments := range map[string]string{
			"wrong type":          `{"color":"white","invite_key":42}`,
			"unknown field":       `{"color":"white","invited_key":"someone"}`,
			"wrong secret":        `{"color":"white","join_secret":false}`,
			"invalid fingerprint": `{"color":"white","invite_key":"someone"}`,
			"null arguments":      `null`,
		} {
			params, err := json.Marshal(callParams{Name: "create", Arguments: json.RawMessage(arguments)})
			if err != nil {
				t.Fatal(err)
			}
			response, _ := handleRPC(ctx, common, rpcRequest{
				JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params,
			})
			result, ok := response.Result.(map[string]any)
			if !ok || result["isError"] != true {
				t.Errorf("%s arguments produced %+v, want tool error", name, response)
			}
		}
		_, projection, err := application.OpenProjection(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(projection.Games) != 0 {
			t.Fatalf("malformed MCP arguments created %d open games", len(projection.Games))
		}
	})
}

func TestMCPStrictlyRefusesMalformedAndOversizedRequests(t *testing.T) {
	var output bytes.Buffer
	input := strings.NewReader("{not json}\n")
	if err := runMCP(context.Background(), nil, input, &output); err != nil {
		t.Fatal(err)
	}
	var malformed rpcResponse
	if err := json.Unmarshal(output.Bytes(), &malformed); err != nil {
		t.Fatal(err)
	}
	if malformed.Error == nil || malformed.Error.Code != -32700 {
		t.Fatalf("malformed response = %+v", malformed)
	}

	output.Reset()
	unknownField := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","extra":true}` + "\n")
	if err := runMCP(context.Background(), nil, unknownField, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &malformed); err != nil || malformed.Error == nil || malformed.Error.Code != -32700 {
		t.Fatalf("unknown-field response = %+v, error %v", malformed, err)
	}

	tooLarge := strings.NewReader(strings.Repeat("x", maxMCPMessage+1) + "\n")
	if err := runMCP(context.Background(), nil, tooLarge, io.Discard); err == nil {
		t.Fatal("oversized MCP request was accepted")
	}
}

func TestHTTPReadProjectionIsBoundedAndCarriesTheVerifiedHead(t *testing.T) {
	requireWritableKeyCustody(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "data")
	var output bytes.Buffer
	if err := run(ctx, []string{"init", "--repo", repo}, &output, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(ctx, []string{"create", "--repo", repo, "--color", "white"}, &output, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.Unmarshal(output.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	game := created["game"].(string)
	handler := newReadHandler(ctx, repo)

	request := httptest.NewRequest(http.MethodGet, "/v1/games?limit=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("games status %d headers %v", response.Code, response.Header())
	}
	var page struct {
		Games []application.Game `json:"games"`
		Head  string             `json:"head"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Games) != 1 || page.Head == "" {
		t.Fatalf("games response %q: %+v, %v", response.Body.String(), page, err)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/board?game="+url.QueryEscape(game), nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var board struct {
		Game application.Game `json:"game"`
		Head string           `json:"head"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &board); err != nil || response.Code != http.StatusOK || board.Game.ID != game || board.Head != page.Head {
		t.Fatalf("board response %q: %+v, %v", response.Body.String(), board, err)
	}

	for _, target := range []string{"/v1/games?limit=0", "/v1/games?limit=101", "/v1/games?limit=words"} {
		request = httptest.NewRequest(http.MethodGet, target, nil)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, response.Code)
		}
	}
}
