package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host/live"
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
	created := call("create", "--repo", repo, "--name", "First game", "--color", "white", "--join-secret-file", secretFile)
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
	emptySource := call("move", "--repo", repo, "--game", game, "--move", "e3e4")
	if emptySource["effective"] != false || !strings.Contains(emptySource["reason"].(string), "the square is empty") {
		t.Fatalf("empty-source output = %+v", emptySource)
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
	if board["name"] != "First game" || board["moves"] != float64(2) || board["turn"] != "w" {
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

func TestActionResultSurfacesDurableRecordWhenDecisionReadFails(t *testing.T) {
	requireWritableKeyCustody(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "decision-read-failure")
	if err := run(ctx, []string{"init", "--repo", repo}, io.Discard, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	workspace, signer, err := openWriter(ctx, &commonFlags{repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	record, err := application.CreateNamed(ctx, workspace, signer, "Durable name", "white", "", "", "read-failure")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := actionResult(canceled, workspace, record); err == nil || !strings.Contains(err.Error(), record.ID) || !strings.Contains(err.Error(), "durably appended") {
		t.Fatalf("decision read error = %v, want durable record %s", err, record.ID)
	}
	_, projection, err := application.OpenProjection(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	game, ok := projection.GameByID(record.ID)
	if !ok || game.Name != "Durable name" || !game.AdmissionOpen {
		t.Fatalf("durable game after read failure = %+v, found %v", game, ok)
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

	create := call(alice, "create", map[string]any{"name": "MCP match", "color": "white", "join_secret": "mcp invite"})
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
	if !ok || shown.Name != "MCP match" || shown.LastMoveUCI != "e2e4" {
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

func TestEmbeddedUIRendersTheExactFoldedPositionAndSeats(t *testing.T) {
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
			t.Fatalf("decode %q: %v", output.String(), err)
		}
		return decoded
	}
	call("init", "--repo", repo)
	created := call("create", "--repo", repo, "--name", "Opening lesson", "--color", "white")
	gameID := created["game"].(string)
	bob := filepath.Join(t.TempDir(), "bob.key")
	call("join", "--repo", repo, "--key", bob, "--game", gameID)
	call("move", "--repo", repo, "--game", gameID, "--move", "e2e4")
	refused := call("move", "--repo", repo, "--game", gameID, "--move", "e4e5")
	if refused["effective"] != false {
		t.Fatalf("wrong-turn move unexpectedly effective: %+v", refused)
	}
	openID := call("create", "--repo", repo, "--name", "Agent match", "--color", "white")["game"].(string)
	restrictedSecret := filepath.Join(t.TempDir(), "restricted.secret")
	if err := os.WriteFile(restrictedSecret, []byte("not in the projection\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretID := call("create", "--repo", repo, "--name", "Secret match", "--color", "white", "--join-secret-file", restrictedSecret)["game"].(string)
	invitePublic, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	inviteFingerprint, err := live.ActorFingerprint(invitePublic)
	if err != nil {
		t.Fatal(err)
	}
	inviteID := call("create", "--repo", repo, "--name", "Invited match", "--color", "white", "--invite-key", inviteFingerprint)["game"].(string)

	_, projection, err := application.OpenProjection(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	game, ok := projection.GameByID(gameID)
	if !ok || game.White == "" || game.Black == "" {
		t.Fatalf("folded game = %+v, found %v", game, ok)
	}
	handler := newReadHandler(ctx, repo)

	lobbyRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	lobbyResponse := httptest.NewRecorder()
	handler.ServeHTTP(lobbyResponse, lobbyRequest)
	if lobbyResponse.Code != http.StatusOK {
		t.Fatalf("lobby status = %d: %s", lobbyResponse.Code, lobbyResponse.Body.String())
	}
	lobby := lobbyResponse.Body.String()
	for _, want := range []string{projection.Head, url.QueryEscape(gameID), "playing", "e2e4", "Opening lesson", "Agent match", "durable games: chess", ">Lobby<", "GitHub repository"} {
		if !strings.Contains(lobby, want) {
			t.Errorf("lobby does not contain %q", want)
		}
	}
	for _, forbidden := range []string{"Verified frontier", "Every position comes from the signed log", "MCP <code>join</code>", "For an open game"} {
		if strings.Contains(lobby, forbidden) {
			t.Errorf("lobby still contains retired copy %q", forbidden)
		}
	}
	cardFor := func(game string) string {
		t.Helper()
		start := strings.Index(lobby, `href="/game?game=`+url.QueryEscape(game)+`"`)
		if start < 0 {
			t.Fatalf("lobby does not contain card for %s", game)
		}
		end := strings.Index(lobby[start:], "</a>")
		if end < 0 {
			t.Fatalf("lobby card for %s has no end", game)
		}
		return lobby[start : start+end]
	}
	if card := cardFor(openID); !strings.Contains(card, "White seated · Black open") || strings.Contains(card, "Black restricted") {
		t.Errorf("open-game card has wrong admission copy: %s", card)
	}
	for _, restrictedID := range []string{secretID, inviteID} {
		if card := cardFor(restrictedID); !strings.Contains(card, "White seated · Black restricted") || strings.Contains(card, "Black open") {
			t.Errorf("restricted-game card %s has wrong admission copy: %s", restrictedID, card)
		}
	}
	if csp := lobbyResponse.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}

	gameRequest := httptest.NewRequest(http.MethodGet, "/game?game="+url.QueryEscape(gameID), nil)
	gameResponse := httptest.NewRecorder()
	handler.ServeHTTP(gameResponse, gameRequest)
	if gameResponse.Code != http.StatusOK {
		t.Fatalf("game status = %d: %s", gameResponse.Code, gameResponse.Body.String())
	}
	page := gameResponse.Body.String()
	for _, want := range []string{
		projection.Head, game.ID, game.White, game.Black, game.FEN,
		`data-square="e4" title="e4" aria-label="white pawn at e4"><span aria-hidden="true">♙</span>`,
		`data-square="e2" title="e2" aria-label="empty at e2"><span aria-hidden="true"></span>`,
		"read-only board", "Opening lesson", ">Lobby</a>", "No chat messages yet.",
		"Refused recorded acts", "actor does not hold the side to move",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("game page does not contain %q", want)
		}
	}
	if strings.Contains(page, "Verified frontier") || strings.Contains(page, "No messages in this process-local room.") {
		t.Error("game page still contains retired frontier or chat copy")
	}

	openRequest := httptest.NewRequest(http.MethodGet, "/game?game="+url.QueryEscape(openID), nil)
	openResponse := httptest.NewRecorder()
	handler.ServeHTTP(openResponse, openRequest)
	openPage := openResponse.Body.String()
	for _, want := range []string{"This game has an open seat", "gitseq-chess MCP", "join", openID} {
		if !strings.Contains(openPage, want) {
			t.Errorf("open-game help does not contain %q", want)
		}
	}
	openGame, _ := projection.GameByID(openID)
	if !openGame.AdmissionOpen {
		t.Fatal("open game does not expose its non-secret admission predicate")
	}
	for _, restrictedID := range []string{secretID, inviteID} {
		restrictedGame, _ := projection.GameByID(restrictedID)
		if restrictedGame.AdmissionOpen {
			t.Errorf("restricted game %s claims open admission", restrictedID)
		}
		restrictedRequest := httptest.NewRequest(http.MethodGet, "/game?game="+url.QueryEscape(restrictedID), nil)
		restrictedResponse := httptest.NewRecorder()
		handler.ServeHTTP(restrictedResponse, restrictedRequest)
		if restrictedResponse.Code != http.StatusOK {
			t.Fatalf("restricted game status = %d", restrictedResponse.Code)
		}
		restrictedPage := restrictedResponse.Body.String()
		if strings.Contains(restrictedPage, "This game has an open seat") || strings.Contains(restrictedPage, "MCP <code>join</code>") {
			t.Errorf("restricted game %s advertises an open MCP join", restrictedID)
		}

		boardRequest := httptest.NewRequest(http.MethodGet, "/v1/board?game="+url.QueryEscape(restrictedID), nil)
		boardResponse := httptest.NewRecorder()
		handler.ServeHTTP(boardResponse, boardRequest)
		body := boardResponse.Body.String()
		if !strings.Contains(body, `"admission_open":false`) || strings.Contains(body, "secret_hash") || strings.Contains(body, "opponent_key") {
			t.Errorf("restricted board exposed more than its admission predicate: %s", body)
		}
	}

	legalRequest := httptest.NewRequest(http.MethodGet, "/v1/legal?game="+url.QueryEscape(gameID)+"&from=e7", nil)
	legalResponse := httptest.NewRecorder()
	handler.ServeHTTP(legalResponse, legalRequest)
	var legal struct {
		Destinations []string `json:"destinations"`
		Head         string   `json:"head"`
	}
	if err := json.Unmarshal(legalResponse.Body.Bytes(), &legal); err != nil || legalResponse.Code != http.StatusOK {
		t.Fatalf("legal response %q: %+v, %v", legalResponse.Body.String(), legal, err)
	}
	if !reflect.DeepEqual(legal.Destinations, []string{"e5", "e6"}) || legal.Head != projection.Head {
		t.Fatalf("legal projection = %+v", legal)
	}
}

func TestHTTPBoundaryRejectsMalformedBrowserInputBeforeReadingTheRepository(t *testing.T) {
	ctx := context.Background()
	handler := newReadHandler(ctx, filepath.Join(t.TempDir(), "missing"))
	eventID := "git:sha1:" + strings.Repeat("a", 40) + "#git:sha1:" + strings.Repeat("b", 40)
	escaped := url.QueryEscape(eventID)
	tests := []string{
		"/?unexpected=true",
		"/game",
		"/game?game=missing",
		"/game?game=" + escaped + "&game=" + escaped,
		"/game?game=" + escaped + "&extra=true",
		"/v1/games?limit=01",
		"/v1/games?after=missing",
		"/v1/board?game=missing",
		"/v1/legal?game=" + escaped,
		"/v1/legal?game=" + escaped + "&from=E7",
		"/v1/legal?game=" + escaped + "&from=e9",
		"/v1/legal?game=" + escaped + "&from=e7&from=e6",
		"/v1/legal?game=" + escaped + "&from=e7%3Bignored=true",
		"/assets/app.js?version=1",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("game="+escaped))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet+", HEAD" {
		t.Fatalf("POST status = %d, Allow = %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestEmbeddedBrowserAssetsHaveNoSigningKeySurface(t *testing.T) {
	handler := newReadHandler(context.Background(), filepath.Join(t.TempDir(), "missing"))
	for _, target := range []string{"/assets/app.css", "/assets/app.js"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s status = %d headers %v", target, response.Code, response.Header())
		}
		if response.Body.Len() == 0 {
			t.Fatalf("%s is empty", target)
		}
		if target == "/assets/app.js" {
			body := response.Body.String()
			for _, forbidden := range []string{"localStorage", "indexedDB", "exportKey(\"pkcs8\"", "exportKey(\"jwk\""} {
				if strings.Contains(body, forbidden) {
					t.Errorf("browser JavaScript persists or exports private-key material through %q", forbidden)
				}
			}
			if !strings.Contains(body, "/v1/legal") {
				t.Error("browser JavaScript does not query fold-owned legal destinations")
			}
			if !strings.Contains(body, `generateKey({name: "Ed25519"}, false`) || !strings.Contains(body, "/v1/live/observe") {
				t.Error("browser JavaScript does not use an in-memory non-exportable live key and separate live observation")
			}
			for _, want := range []string{"No chat messages yet.", "No valid move is available from", "showModal"} {
				if !strings.Contains(body, want) {
					t.Errorf("browser JavaScript does not contain %q", want)
				}
			}
			for _, forbidden := range []string{"The process-local live room restarted. Its transient history was cleared.", "The fold allows no move from that square."} {
				if strings.Contains(body, forbidden) {
					t.Errorf("browser JavaScript still contains retired copy %q", forbidden)
				}
			}
		} else {
			body := response.Body.String()
			for _, want := range []string{"grid-template-rows: repeat(8", "aspect-ratio: 1", "resize: both"} {
				if !strings.Contains(body, want) {
					t.Errorf("browser CSS does not enforce %q", want)
				}
			}
		}
	}
}

func TestREADMEWalkthroughCreatesKeysNamesAJoinedGameAndPlays(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(source)
	for _, want := range []string{
		"There is no separate\nkey-generation command",
		`--name "First game"`,
		"--key bob.key --game '<game-id>'",
		"--move e2e4",
		"--key bob.key --game '<game-id>' --move e7e5",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("first-game walkthrough does not contain %q", want)
		}
	}
}

func postJSON(t *testing.T, handler http.Handler, target string, value, decoded any, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("POST %s status = %d, want %d: %s", target, response.Code, wantStatus, response.Body.String())
	}
	if decoded != nil && response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), decoded); err != nil {
			t.Fatalf("decode POST %s response %q: %v", target, response.Body.String(), err)
		}
	}
	return response
}

func openBrowserLiveSession(t *testing.T, handler http.Handler, runtime *chessLive, game string, private ed25519.PrivateKey) (string, string) {
	t.Helper()
	public := private.Public().(ed25519.PublicKey)
	before := runtime.hub.Snapshot()
	var prepared struct {
		Challenge    live.SessionChallenge `json:"challenge"`
		SigningBytes []byte                `json:"signing_bytes"`
		Role         string                `json:"role"`
	}
	postJSON(t, handler, "/v1/live/session/prepare", liveSessionPrepareRequest{Game: game, ActorKey: public}, &prepared, http.StatusOK)
	afterPrepare := runtime.hub.Snapshot()
	if afterPrepare.Cursor != before.Cursor || len(afterPrepare.Presence) != len(before.Presence) {
		t.Fatal("unproved browser challenge published presence")
	}
	var opened struct {
		Credential string `json:"credential"`
		Role       string `json:"role"`
	}
	postJSON(t, handler, "/v1/live/session/open", liveSessionOpenRequest{
		Challenge: prepared.Challenge, Signature: ed25519.Sign(private, prepared.SigningBytes),
	}, &opened, http.StatusOK)
	postJSON(t, handler, "/v1/live/session/open", liveSessionOpenRequest{
		Challenge: prepared.Challenge, Signature: ed25519.Sign(private, prepared.SigningBytes),
	}, nil, http.StatusBadRequest)
	if opened.Credential == "" || opened.Role != prepared.Role {
		t.Fatalf("opened session = %+v, prepared role %q", opened, prepared.Role)
	}
	return opened.Credential, opened.Role
}

func TestLiveRoomProvesBrowserKeysAndKeepsTheFoldAuthoritative(t *testing.T) {
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
			t.Fatal(err)
		}
		return decoded
	}
	call("init", "--repo", repo)
	gameID := call("create", "--repo", repo, "--color", "white")["game"].(string)
	otherGameID := call("create", "--repo", repo, "--color", "white")["game"].(string)
	bobPath := filepath.Join(t.TempDir(), "bob.key")
	call("join", "--repo", repo, "--key", bobPath, "--game", gameID)

	aliceStore, err := openKeyStore(ctx, "", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := readKey(aliceStore)
	aliceStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := newReadHandlerWithLive(ctx, repo, runtime)
	failedPublic, failedPrivate, _ := ed25519.GenerateKey(nil)
	_, wrongPrivate, _ := ed25519.GenerateKey(nil)
	var failedProof struct {
		Challenge    live.SessionChallenge `json:"challenge"`
		SigningBytes []byte                `json:"signing_bytes"`
	}
	postJSON(t, handler, "/v1/live/session/prepare", liveSessionPrepareRequest{
		Game: gameID, ActorKey: failedPublic,
	}, &failedProof, http.StatusOK)
	postJSON(t, handler, "/v1/live/session/open", liveSessionOpenRequest{
		Challenge: failedProof.Challenge, Signature: ed25519.Sign(wrongPrivate, failedProof.SigningBytes),
	}, nil, http.StatusBadRequest)
	postJSON(t, handler, "/v1/live/session/open", liveSessionOpenRequest{
		Challenge: failedProof.Challenge, Signature: ed25519.Sign(failedPrivate, failedProof.SigningBytes),
	}, nil, http.StatusBadRequest)
	aliceCredential, aliceRole := openBrowserLiveSession(t, handler, runtime, gameID, alice)
	if aliceRole != "white" {
		t.Fatalf("fold-derived Alice role = %q, want white", aliceRole)
	}
	_, watcher, _ := ed25519.GenerateKey(nil)
	watcherCredential, watcherRole := openBrowserLiveSession(t, handler, runtime, gameID, watcher)
	if watcherRole != "watcher" {
		t.Fatalf("fold-derived stranger role = %q, want watcher", watcherRole)
	}
	var liveBaseline liveObserveResponse
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: gameID, WaitMS: 1}, &liveBaseline, http.StatusOK)

	public := alice.Public().(ed25519.PublicKey)
	postJSON(t, handler, "/v1/live/motion", liveMotionRequest{
		Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "dragged", From: "e2",
	}, nil, http.StatusOK)
	if conversations := runtime.hub.Snapshot().Conversations; len(conversations) != 0 {
		t.Fatalf("leased motion opened retained conversations: %v", conversations)
	}
	for _, invalid := range []liveMotionRequest{
		{Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "hovered", From: "e2"},
		{Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "dragged", From: "z9"},
		{Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "dragged", From: "e2", To: "e4"},
		{Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "submitting", From: "e2", To: "e5"},
		{Credential: watcherCredential, Game: gameID, ActorKey: watcher.Public().(ed25519.PublicKey), Phase: "submitting", From: "e2", To: "e4"},
	} {
		postJSON(t, handler, "/v1/live/motion", invalid, nil, http.StatusBadRequest)
	}
	postJSON(t, handler, "/v1/live/motion", liveMotionRequest{
		Credential: aliceCredential, Game: gameID, ActorKey: public, Phase: "submitting", From: "e2", To: "e4",
	}, nil, http.StatusOK)
	postJSON(t, handler, "/v1/live/motion", liveMotionRequest{
		Credential: aliceCredential, Game: otherGameID, ActorKey: public, Phase: "submitting", From: "e2", To: "e4",
	}, nil, http.StatusBadRequest)

	postJSON(t, handler, "/v1/live/chat/prepare", liveChatPrepareRequest{
		Credential: watcherCredential, Game: gameID, ActorKey: public, Text: "key mismatch",
	}, nil, http.StatusBadRequest)
	postJSON(t, handler, "/v1/live/chat/prepare", liveChatPrepareRequest{
		Credential: aliceCredential, Game: otherGameID, ActorKey: public, Text: "wrong game",
	}, nil, http.StatusBadRequest)
	var chatPrepared struct {
		Draft        live.Draft `json:"draft"`
		SigningBytes []byte     `json:"signing_bytes"`
	}
	postJSON(t, handler, "/v1/live/chat/prepare", liveChatPrepareRequest{
		Credential: aliceCredential, Game: gameID, ActorKey: public, Text: "good luck",
	}, &chatPrepared, http.StatusOK)
	watcherPublic := watcher.Public().(ed25519.PublicKey)
	var competingChat struct {
		Draft        live.Draft `json:"draft"`
		SigningBytes []byte     `json:"signing_bytes"`
	}
	postJSON(t, handler, "/v1/live/chat/prepare", liveChatPrepareRequest{
		Credential: watcherCredential, Game: gameID, ActorKey: watcherPublic, Text: "stale first draft",
	}, &competingChat, http.StatusOK)
	var chatResult struct {
		Conversation string `json:"conversation"`
	}
	postJSON(t, handler, "/v1/live/chat/submit", liveSubmitRequest{
		Credential: aliceCredential, Game: gameID,
		Submission: live.Submission{
			Draft: chatPrepared.Draft, ActorKey: public,
			ActorSignature: ed25519.Sign(alice, chatPrepared.SigningBytes),
		},
	}, &chatResult, http.StatusOK)
	postJSON(t, handler, "/v1/live/chat/submit", liveSubmitRequest{
		Credential: watcherCredential, Game: gameID,
		Submission: live.Submission{
			Draft: competingChat.Draft, ActorKey: watcherPublic,
			ActorSignature: ed25519.Sign(watcher, competingChat.SigningBytes),
		},
	}, nil, http.StatusConflict)
	if chatResult.Conversation == "" || len(runtime.hub.Snapshot().Conversations) != 1 {
		t.Fatalf("chat did not reserve exactly one per-game conversation: %+v", runtime.hub.Snapshot().Conversations)
	}

	var observed liveObserveResponse
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{
		Game: gameID, Cursor: liveBaseline.Cursor, WaitMS: 1,
	}, &observed, http.StatusOK)
	if len(observed.Participants) != 2 || len(observed.Motions) != 1 || len(observed.Chat) != 1 {
		t.Fatalf("live view = %+v", observed)
	}
	if observed.Motions[0].From != "e2" || observed.Motions[0].To != "e4" || observed.Motions[0].Role != "white" || observed.Motions[0].Phase != "submitting" {
		t.Fatalf("projected motion = %+v", observed.Motions[0])
	}
	if observed.Chat[0].Text != "good luck" || observed.Cursor.Live.Generation == "" || observed.Cursor.Durable.Head == "" || observed.Reset {
		t.Fatalf("composite observation = %+v", observed)
	}
	if observed.Cursor.Durable != liveBaseline.Cursor.Durable || observed.Cursor.Live.Position <= liveBaseline.Cursor.Live.Position {
		t.Fatalf("live changes did not move only the live cursor: before=%+v after=%+v", liveBaseline.Cursor, observed.Cursor)
	}
	if observed.Game.LastMoveUCI != "" || observed.Game.Moves != 0 {
		t.Fatalf("live motion changed durable game = %+v", observed.Game)
	}

	// An ordinary renewal clears the leased motion without touching chat, and
	// the watcher who did not need a conversation participant grant still sees
	// the one game transcript.
	postJSON(t, handler, "/v1/live/session/renew", liveSessionRenewRequest{
		Credential: aliceCredential, Game: gameID, ActorKey: public,
	}, nil, http.StatusOK)
	var watcherObserved liveObserveResponse
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{
		Credential: watcherCredential, Game: gameID, WaitMS: 1,
	}, &watcherObserved, http.StatusOK)
	if len(watcherObserved.Motions) != 0 || len(watcherObserved.Chat) != 1 || watcherObserved.Chat[0].Text != "good luck" {
		t.Fatalf("renewed motion/chat watcher projection = %+v", watcherObserved)
	}
	var otherObserved liveObserveResponse
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: otherGameID, WaitMS: 1}, &otherObserved, http.StatusOK)
	if len(otherObserved.Participants) != 0 || len(otherObserved.Motions) != 0 || len(otherObserved.Chat) != 0 {
		t.Fatalf("game-scoped live state leaked into another game: %+v", otherObserved)
	}

	restarted, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	restartedHandler := newReadHandlerWithLive(ctx, repo, restarted)
	var afterRestart liveObserveResponse
	postJSON(t, restartedHandler, "/v1/live/observe", liveObserveRequest{
		Game: gameID, Cursor: watcherObserved.Cursor, WaitMS: 1,
	}, &afterRestart, http.StatusOK)
	if !afterRestart.Reset || len(afterRestart.Participants) != 0 || len(afterRestart.Motions) != 0 || len(afterRestart.Chat) != 0 {
		t.Fatalf("process restart did not reset transient room: %+v", afterRestart)
	}

	_, projection, err := application.OpenProjection(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	game, _ := projection.GameByID(gameID)
	if got := application.LegalDestinations(game, "e2"); !reflect.DeepEqual(got, []string{"e3", "e4"}) {
		t.Fatalf("durable fold changed after live motion: %v", got)
	}
}

func TestLiveHandlersDelegateSeatAuthorityToProjection(t *testing.T) {
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
			t.Fatal(err)
		}
		return decoded
	}
	call("init", "--repo", repo)
	game := call("create", "--repo", repo, "--color", "white")["game"].(string)
	call("join", "--repo", repo, "--key", filepath.Join(t.TempDir(), "black.key"), "--game", game)
	store, err := openKeyStore(ctx, "", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	private, err := readKey(store)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	realSeatFor := runtime.seatFor
	seatLookups := 0
	runtime.seatFor = func(projection application.Projection, game, actor string) (string, bool) {
		seatLookups++
		return realSeatFor(projection, game, actor)
	}
	handler := newReadHandlerWithLive(ctx, repo, runtime)
	credential, _ := openBrowserLiveSession(t, handler, runtime, game, private)
	if seatLookups != 2 {
		t.Fatalf("prepare and open made %d seat lookups, want 2", seatLookups)
	}
	public := private.Public().(ed25519.PublicKey)
	postJSON(t, handler, "/v1/live/session/renew", liveSessionRenewRequest{
		Credential: credential, Game: game, ActorKey: public,
	}, nil, http.StatusOK)
	if seatLookups != 3 {
		t.Fatalf("renew made %d cumulative seat lookups, want 3", seatLookups)
	}
	postJSON(t, handler, "/v1/live/motion", liveMotionRequest{
		Credential: credential, Game: game, ActorKey: public, Phase: "dragged", From: "e2",
	}, nil, http.StatusOK)
	if seatLookups != 4 {
		t.Fatalf("motion validation made %d cumulative seat lookups, want 4", seatLookups)
	}
}

func TestLiveBoundaryKeepsCursorsSeparateAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	missingRepo := filepath.Join(t.TempDir(), "missing")
	handler := newReadHandlerWithLive(ctx, missingRepo, runtime)
	eventID := "git:sha1:" + strings.Repeat("a", 40) + "#git:sha1:" + strings.Repeat("b", 40)

	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: eventID, WaitMS: 30001}, nil, http.StatusBadRequest)
	postJSON(t, handler, "/v1/live/observe?unexpected=true", liveObserveRequest{Game: eventID, WaitMS: 1}, nil, http.StatusBadRequest)
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: eventID, WaitMS: 1, Credential: "credential:not-valid"}, nil, http.StatusBadRequest)
	postJSON(t, handler, "/v1/live/session/prepare", map[string]any{
		"game": eventID, "actor_key": []byte("short"), "role": "white",
	}, nil, http.StatusBadRequest)

	request := httptest.NewRequest(http.MethodPost, "/v1/live/observe", strings.NewReader(`{"game":"`+eventID+`","wait_ms":1}`))
	request.Host = "127.0.0.1:8080"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing JSON content type status = %d", response.Code)
	}

	oversized := strings.Repeat("x", maxLiveRequestBytes+1)
	request = httptest.NewRequest(http.MethodPost, "/v1/live/session/revoke", strings.NewReader(`{"credential":"`+oversized+`"}`))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized live body status = %d", response.Code)
	}
}

func TestLiveTransportAndWaitBoundariesFailClosed(t *testing.T) {
	server := newChessHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if server.WriteTimeout <= live.MaxCompositeWait || server.ReadTimeout <= 0 || server.ReadHeaderTimeout <= 0 {
		t.Fatalf("HTTP timeouts do not bound reads and cover live waits: read=%s header=%s write=%s wait=%s", server.ReadTimeout, server.ReadHeaderTimeout, server.WriteTimeout, live.MaxCompositeWait)
	}
	for _, address := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		if !validLoopbackListen(address) {
			t.Errorf("loopback listen %q refused", address)
		}
	}
	for _, address := range []string{"0.0.0.0:8080", "example.com:8080", ":8080", "127.0.0.1"} {
		if validLoopbackListen(address) {
			t.Errorf("unsafe listen %q accepted", address)
		}
	}
	if err := runServe(context.Background(), []string{"--repo", t.TempDir(), "--listen", "0.0.0.0:8080"}, io.Discard); err == nil {
		t.Fatal("serve accepted a non-loopback listener")
	}

	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := newReadHandlerWithLive(context.Background(), filepath.Join(t.TempDir(), "missing"), runtime)
	eventID := "git:sha1:" + strings.Repeat("a", 40) + "#git:sha1:" + strings.Repeat("b", 40)
	body, _ := json.Marshal(liveObserveRequest{Game: eventID, WaitMS: 1})
	tests := []struct {
		name, host, origin, site, reason string
	}{
		{name: "host", host: "attacker.example:8080", reason: "loopback"},
		{name: "origin", host: "127.0.0.1:8080", origin: "http://attacker.example:8080", reason: "cross-origin"},
		{name: "site", host: "127.0.0.1:8080", site: "cross-site", reason: "cross-site"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/live/observe", bytes.NewReader(body))
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.site != "" {
				request.Header.Set("Sec-Fetch-Site", test.site)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.reason) {
				t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestLiveWaitBudgetReleasesOnCompletionAndCancellation(t *testing.T) {
	eventID := "git:sha1:" + strings.Repeat("c", 40) + "#git:sha1:" + strings.Repeat("d", 40)
	projection := application.Projection{
		Genesis: strings.Split(eventID, "#")[0], Head: strings.Split(eventID, "#")[1], Depth: 1,
		Games: []application.Game{{ID: eventID, Status: "playing", Turn: "w"}}, ByID: map[string]int{eventID: 0},
	}
	makeHandler := func(runtime *chessLive, read projectionReader) http.Handler {
		mux := http.NewServeMux()
		runtime.register(mux, read)
		return securityHeaders(rejectLiveQueries(trustedLiveHost(mux)))
	}
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := makeHandler(runtime, func(context.Context) (application.Projection, error) { return projection, nil })
	for range maxLiveWaiters {
		runtime.waitSlots <- struct{}{}
	}
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: eventID, WaitMS: 1}, nil, http.StatusTooManyRequests)
	<-runtime.waitSlots
	postJSON(t, handler, "/v1/live/observe", liveObserveRequest{Game: eventID, WaitMS: 1}, nil, http.StatusOK)
	if len(runtime.waitSlots) != maxLiveWaiters-1 {
		t.Fatalf("completed observe leaked a wait slot: %d occupied", len(runtime.waitSlots))
	}
	for len(runtime.waitSlots) != 0 {
		<-runtime.waitSlots
	}

	cancelRuntime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	readerEntered := make(chan struct{})
	var once sync.Once
	cancelHandler := makeHandler(cancelRuntime, func(ctx context.Context) (application.Projection, error) {
		once.Do(func() { close(readerEntered) })
		<-ctx.Done()
		return application.Projection{}, ctx.Err()
	})
	requestContext, cancel := context.WithCancel(context.Background())
	requestBody, _ := json.Marshal(liveObserveRequest{Game: eventID, WaitMS: 30000})
	request := httptest.NewRequest(http.MethodPost, "/v1/live/observe", bytes.NewReader(requestBody)).WithContext(requestContext)
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		cancelHandler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-readerEntered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled live observe did not return promptly")
	}
	if len(cancelRuntime.waitSlots) != 0 {
		t.Fatalf("cancelled observe leaked %d wait slots", len(cancelRuntime.waitSlots))
	}
}

func TestLeasedMotionExpiresWithoutRetainedFrames(t *testing.T) {
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	game := "git:sha1:" + strings.Repeat("e", 40) + "#git:sha1:" + strings.Repeat("f", 40)
	fingerprint := liveFingerprint(public)
	presence := livePresence{
		Game: game, Actor: fingerprint, Role: "white",
		Motion: &liveMotion{Phase: "submitting", Head: "head", From: "e2", To: "e4", Role: "white"},
	}
	value, _ := json.Marshal(presence)
	challenge, err := runtime.hub.PrepareSession(sessionActor(game, fingerprint), public, string(value), 5*time.Millisecond, live.ActivityUpdate{})
	if err != nil {
		t.Fatal(err)
	}
	signingBytes, _ := live.SessionSigningBytes(challenge)
	if _, _, err := runtime.hub.OpenSession(challenge, ed25519.Sign(private, signingBytes)); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	_, motions, _ := runtime.projectLiveLocked(game, "head", runtime.hub.Snapshot())
	runtime.mu.Unlock()
	if len(motions) != 1 || len(runtime.hub.Snapshot().Conversations) != 0 {
		t.Fatalf("initial leased motion = %+v conversations = %v", motions, runtime.hub.Snapshot().Conversations)
	}
	time.Sleep(15 * time.Millisecond)
	runtime.mu.Lock()
	participants, motions, _ := runtime.projectLiveLocked(game, "head", runtime.hub.Snapshot())
	runtime.mu.Unlock()
	if len(participants) != 0 || len(motions) != 0 {
		t.Fatalf("expired presence remained visible: participants=%+v motions=%+v", participants, motions)
	}
}
