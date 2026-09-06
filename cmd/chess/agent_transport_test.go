package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
)

// Run the actual main/CLI/stdin-MCP entry points in independent processes.
func TestAgentCommandProcess(t *testing.T) {
	if os.Getenv("CHESS_AGENT_COMMAND_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"chess"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}
func agentProcess(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestAgentCommandProcess$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHESS_AGENT_COMMAND_HELPER=1")
	return cmd
}
func agentCommand(t *testing.T, dir, input string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := agentProcess(ctx, dir, args...)
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}
func agentSuccess(t *testing.T, dir string, args ...string) agentResult {
	t.Helper()
	out, err := agentCommand(t, dir, "", args...)
	if err != nil {
		t.Fatalf("agent %s: %v: %s", args[0], err, out)
	}
	var result agentResult
	if err = json.Unmarshal(out, &result); err != nil || result.Effective == nil || !*result.Effective {
		t.Fatalf("agent confirmation: %s (%v)", out, err)
	}
	return result
}
func agentKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	out, err := agentCommand(t, dir, "", "keygen", "--key", path)
	if err != nil {
		t.Fatalf("keygen: %v: %s", err, out)
	}
	if strings.Contains(string(out), "private") {
		t.Fatal("keygen printed private data")
	}
	return path
}
func agentServing(t *testing.T, repo string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := agentProcess(ctx, repo, "serve", "--repo", repo, "--listen", address)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); _ = cmd.Wait() }) }
	t.Cleanup(stop)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get("http://" + address + "/v1/service")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return "http://" + address, stop
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	t.Fatalf("service did not start: %s", logs.String())
	return "", stop
}
func agentBoard(t *testing.T, dir, server, genesis, key, game string) application.Game {
	t.Helper()
	out, err := agentCommand(t, dir, "", "board", "--server", server, "--genesis", genesis, "--key", key, "--game", game)
	if err != nil {
		t.Fatalf("board: %v: %s", err, out)
	}
	var result struct {
		Game application.Game `json:"game"`
	}
	if err = json.Unmarshal(out, &result); err != nil || result.Game.ID != game {
		t.Fatalf("board result: %s", out)
	}
	return result.Game
}
func agentMCP(t *testing.T, dir, server, genesis, key, name string, args any) (agentResult, error, string) {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	out, err := agentCommand(t, dir, string(input)+"\n", "mcp", "--server", server, "--genesis", genesis, "--key", key)
	if err != nil {
		return agentResult{}, err, string(out)
	}
	var rpc struct {
		Result struct {
			IsError    bool        `json:"isError"`
			Structured agentResult `json:"structuredContent"`
		}
	}
	if err = json.Unmarshal(out, &rpc); err != nil {
		return agentResult{}, err, string(out)
	}
	if rpc.Result.IsError {
		return agentResult{}, fmt.Errorf("tool refused"), string(out)
	}
	return rpc.Result.Structured, nil, string(out)
}

func TestAgentProcessesPlayAndRecoverAcrossServiceAndAdapterRestart(t *testing.T) {
	repo, remote, _, ws := forgeFixture(t)
	log, err := ws.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	genesis := canonicalAgentObject(log.Genesis)
	keys := t.TempDir()
	white := agentKey(t, keys, "white.key")
	black := agentKey(t, keys, "black.key")
	server, stop := agentServing(t, repo)
	flags := func(key string) []string { return []string{"--server", server, "--genesis", genesis, "--key", key} }
	create := agentSuccess(t, keys, append([]string{"create", "--name", "Evening game"}, flags(white)...)...)
	join, err, out := agentMCP(t, keys, server, genesis, black, "join", map[string]string{"game": create.Record})
	if err != nil || join.Effective == nil || !*join.Effective {
		t.Fatalf("MCP join: %v %s", err, out)
	}
	board := agentBoard(t, keys, server, genesis, white, create.Record)
	wrongTurn, err, out := agentMCP(t, keys, server, genesis, black, "move", map[string]string{"game": create.Record, "move": "e7e5", "predecessor": board.LastMove})
	if err != nil || wrongTurn.Effective == nil || *wrongTurn.Effective || wrongTurn.Reason == "" {
		t.Fatalf("MCP lost ineffective decision: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(keys, ".black.key.agent", "pending.json")); !os.IsNotExist(err) {
		t.Fatal("ineffective confirmed result kept pending state")
	}
	// The actual service holds writer ownership while both entry points play.
	if out, err := agentCommand(t, repo, "", "move", "--repo", repo, "--key", white, "--game", create.Record, "--move", "e2e4"); err == nil || !strings.Contains(string(out), "another Chess writer") {
		t.Fatalf("local writer was not excluded: %v %s", err, out)
	}
	// Drop both automatic submit acknowledgements after the real service has
	// replied successfully. Keep the actual accepted record for exact comparison.
	target, _ := url.Parse(server)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var acceptedMu sync.Mutex
	var accepted []agentResult
	var drop atomic.Bool
	drop.Store(true)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.URL.Path == "/v1/actions/native/submit" && response.StatusCode == 200 {
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				return err
			}
			var result agentResult
			if err = json.Unmarshal(data, &result); err != nil {
				return err
			}
			acceptedMu.Lock()
			accepted = append(accepted, result)
			acceptedMu.Unlock()
			response.Body = io.NopCloser(bytes.NewReader(data))
			if drop.Load() {
				return fmt.Errorf("deliberately dropped acknowledged submit")
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		connection, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr == nil {
			connection.Close()
		}
	}
	pendingPath := filepath.Join(keys, ".white.key.agent", "pending.json")
	var retainedBeforeSubmit atomic.Bool
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/actions/native/submit" {
			data, readErr := io.ReadAll(r.Body)
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(data))
			saved, fileErr := os.ReadFile(pendingPath)
			var sent, retained agentSubmission
			if readErr != nil || fileErr != nil || json.Unmarshal(data, &sent) != nil || json.Unmarshal(saved, &retained) != nil || !reflect.DeepEqual(sent, retained) {
				http.Error(w, "submission preceded exact pending publication", 500)
				return
			}
			retainedBeforeSubmit.Store(true)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer fault.Close()
	moveArgs := []string{"move", "--server", fault.URL, "--genesis", genesis, "--key", white, "--game", create.Record, "--move", "e2e4", "--predecessor", board.LastMove}
	outBytes, err := agentCommand(t, keys, "", moveArgs...)
	if err == nil || !strings.Contains(string(outBytes), "outcome not confirmed") {
		t.Fatalf("lost response: %v %s", err, outBytes)
	}
	acceptedMu.Lock()
	copies := append([]agentResult(nil), accepted...)
	acceptedMu.Unlock()
	if len(copies) != 2 || copies[0].Record != copies[1].Record || copies[0].Depth != copies[1].Depth {
		t.Fatalf("automatic exact replay: %#v", copies)
	}
	if !retainedBeforeSubmit.Load() {
		t.Fatal("signed action was not retained before its first submit")
	}
	pendingBytes, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	var pending agentSubmission
	if err = json.Unmarshal(pendingBytes, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Action.Move != "e2e4" || pending.Action.Predecessor != board.LastMove || len(pending.Signature) != ed25519.SignatureSize {
		t.Fatal("original signed move was not retained")
	}
	if out, err := agentCommand(t, keys, "", append([]string{"create"}, flags(white)...)...); err == nil || !strings.Contains(string(out), "retained action") {
		t.Fatalf("unknown outcome allowed another mutation: %v %s", err, out)
	}
	if after, _ := os.ReadFile(pendingPath); !bytes.Equal(after, pendingBytes) {
		t.Fatal("different command overwrote pending move")
	}
	// Black advances the board before either process is restarted.
	board = agentBoard(t, keys, server, genesis, black, create.Record)
	reply, err, out := agentMCP(t, keys, server, genesis, black, "move", map[string]string{"game": create.Record, "move": "e7e5", "predecessor": board.LastMove})
	if err != nil || reply.Effective == nil || !*reply.Effective {
		t.Fatalf("MCP answer: %v %s", err, out)
	}
	stop()
	server, stop = agentServing(t, repo)
	defer stop()
	retry, err, out := agentMCP(t, keys, server, genesis, white, "retry", map[string]any{})
	if err != nil || retry.Record != copies[0].Record || retry.Depth != reply.Depth {
		t.Fatalf("restart exact replay: %v %s", err, out)
	}
	if _, err = os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("confirmed attempt retained: %v", err)
	}
	confirmed, _ := ws.Records(context.Background())
	if confirmed.Depth != reply.Depth {
		t.Fatal("retry appended a duplicate")
	}
	if remoteHead := forgeGit(t, remote, "rev-parse", "refs/seq/"+log.Genesis); remoteHead != retry.Head {
		t.Fatal("acknowledgement was not the forge-confirmed frontier")
	}
	// A fresh stale choice is rejected before signing or publication.
	staleArgs := append([]string{"move", "--game", create.Record, "--move", "d2d4", "--predecessor", pending.Action.Predecessor}, flags(white)...)
	if out, err := agentCommand(t, keys, "", staleArgs...); err == nil || !strings.Contains(string(out), "HTTP 409") {
		t.Fatalf("fresh stale move: %v %s", err, out)
	}
	if _, err = os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatal("stale prepare created a pending action")
	}
	board = agentBoard(t, keys, server, genesis, white, create.Record)
	offer := agentSuccess(t, keys, append([]string{"draw_offer", "--game", create.Record, "--predecessor", board.LastMove}, flags(white)...)...)
	acceptedDraw, err, out := agentMCP(t, keys, server, genesis, black, "draw_accept", map[string]string{"game": create.Record, "offer": offer.Record})
	if err != nil || acceptedDraw.Effective == nil || !*acceptedDraw.Effective {
		t.Fatalf("draw accept: %v %s", err, out)
	}
	if game := agentBoard(t, keys, server, genesis, white, create.Record); game.Status != "finished" {
		t.Fatalf("terminal game: %#v", game)
	}
	rematch, err, out := agentMCP(t, keys, server, genesis, black, "create", map[string]string{"name": "Rematch", "color": "black"})
	if err != nil || rematch.Effective == nil || !*rematch.Effective || rematch.Record == create.Record {
		t.Fatalf("rematch: %v %s", err, out)
	}
	agentSuccess(t, keys, append([]string{"join", "--game", rematch.Record}, flags(white)...)...)
	board = agentBoard(t, keys, server, genesis, black, rematch.Record)
	agentSuccess(t, keys, append([]string{"resign", "--game", rematch.Record, "--predecessor", board.LastMove}, flags(black)...)...)
}

func TestAgentKeyCreationAndPendingCustody(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := agentKey(t, dir, "agent.key")
	original, _ := os.ReadFile(keyPath)
	if out, err := agentCommand(t, dir, "", "keygen", "--key", keyPath); err == nil {
		t.Fatalf("keygen replaced an existing key: %s", out)
	}
	if after, _ := os.ReadFile(keyPath); !bytes.Equal(after, original) {
		t.Fatal("keygen changed existing key")
	}
	custody, err := openAgentCustody(ctx, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if concurrent, err := openAgentCustody(ctx, keyPath); err == nil {
		concurrent.Close()
		t.Fatal("concurrent agent acquired pending state")
	}
	custody.Close()
	_, replacement := generateIdentityKey(t)
	if err = os.WriteFile(keyPath, []byte(hex.EncodeToString(replacement)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := openAgentCustody(ctx, keyPath); err == nil {
		c.Close()
		t.Fatal("replaced key accepted")
	}
	if err = os.WriteFile(keyPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if c, err := openAgentCustody(ctx, keyPath); err == nil {
		c.Close()
		t.Fatal("public key file permissions accepted")
	}
	if c, err := openAgentCustody(ctx, filepath.Join(dir, "missing.key")); err == nil {
		c.Close()
		t.Fatal("missing key invented")
	}
	if _, err = os.Stat(filepath.Join(dir, "missing.key")); !os.IsNotExist(err) {
		t.Fatal("missing key was created")
	}
}

func TestAgentPendingBoundsAndExclusivePublication(t *testing.T) {
	dir := t.TempDir()
	path := agentKey(t, dir, "agent.key")
	custody, err := openAgentCustody(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer custody.Close()
	action := agentSubmission{Action: agentAction{Action: "create", Genesis: "git:sha1:" + strings.Repeat("a", 40), Color: "white", ActorKey: custody.key.Public().(ed25519.PublicKey), IdempotencyKey: "first"}, Signature: make([]byte, 64)}
	if err = custody.retain(action); err != nil {
		t.Fatal(err)
	}
	action.Action.IdempotencyKey = "different"
	if err = custody.retain(action); err == nil {
		t.Fatal("pending action overwritten")
	}
	original, err := custody.pending()
	if err != nil || original.Action.IdempotencyKey != "first" {
		t.Fatalf("original pending changed: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".agent.key.agent", "pending.json"), bytes.Repeat([]byte("x"), maxAgentPending+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = custody.pending(); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized pending accepted: %v", err)
	}
}
