package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	application "github.com/generalbusiness-ai/gitseq-chess"
)

func TestAgentDestinationRefusesFallbackAndUnsafeOrigins(t *testing.T) {
	for _, value := range []string{"https://127.0.0.1:8080", "http://localhost:8080", "http://example.com", "http://192.0.2.1", "http://127.0.0.1/", "http://127.0.0.1/prefix", "http://127.0.0.1?", "http://127.0.0.1#", "http://user@127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://[::ffff:192.0.2.1]:8080"} {
		if _, err := agentServerURL(value); err == nil {
			t.Errorf("unsafe origin %q accepted", value)
		}
	}
	for _, value := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://127.0.0.2"} {
		if _, err := agentServerURL(value); err != nil {
			t.Errorf("loopback %q: %v", value, err)
		}
	}
	ctx := context.Background()
	repo, key, ws := newIdentityTestRepository(t, ctx)
	created, err := application.Create(ctx, ws, key, "white", "", "", "fallback-fixture")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := ws.Records(ctx)
	genesis := canonicalAgentObject(before.Genesis)
	keys := t.TempDir()
	keyPath := agentKey(t, keys, "agent.key")
	dead := httptest.NewServer(http.NotFoundHandler())
	origin := dead.URL
	dead.Close()
	// A real local repository exists in the process cwd. A missing routing guard
	// would make these commands succeed locally; the signed frontier must not move.
	for _, name := range []string{"create", "show_board"} {
		args := map[string]string{"game": created.ID}
		if name == "create" {
			args = map[string]string{"color": "white"}
		}
		_, err, out := agentMCP(t, repo, origin, genesis, keyPath, name, args)
		if err == nil || !strings.Contains(out, "unavailable") {
			t.Fatalf("%s fell back from unavailable service: %v %s", name, err, out)
		}
	}
	after, _ := ws.Records(ctx)
	if before.Head != after.Head || before.Depth != after.Depth {
		t.Fatal("server mode changed local repository")
	}
	if _, err = os.Stat(filepath.Join(repo, ".git", "chess-writer.lock")); !os.IsNotExist(err) {
		t.Fatal("server mode opened local writer ownership")
	}
	out, err := agentCommand(t, repo, "", "create", "--server", origin, "--repo", repo, "--genesis", genesis, "--key", keyPath)
	if err == nil || !strings.Contains(string(out), "mutually exclusive") {
		t.Fatalf("ambiguous destination: %v %s", err, out)
	}
	for _, args := range [][]string{{"mcp", "--server="}, {"mcp", "--server", origin, "--genesis", genesis}, {"mcp", "--server", origin, "--genesis", genesis, "--key", filepath.Join(keys, "missing")}} {
		if out, err := agentCommand(t, repo, "", args...); err == nil {
			t.Fatalf("invalid server setup accepted: %v %s", args, out)
		}
	}
}

func TestAgentNativeTypedBoundaryAndIneffectiveDecisions(t *testing.T) {
	ctx := context.Background()
	repo, white, ws := newIdentityTestRepository(t, ctx)
	owner, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	live, _ := newChessLive()
	handler := newReadHandlerWithIdentity(ctx, repo, live, identityHTTPConfig{}, owner)
	before, _ := ws.Records(ctx)
	genesis := canonicalAgentObject(before.Genesis)
	pub, black := generateIdentityKey(t)
	input := agentAction{Action: "create", Genesis: genesis, ActorKey: white.Public().(ed25519.PublicKey), Color: "white", Name: "Typed", IdempotencyKey: "typed-create"}
	var prepared agentPreparation
	postJSON(t, handler, "/v1/actions/native/prepare", input, &prepared, 200)
	submission := agentSubmission{Action: input, Signature: ed25519.Sign(white, prepared.SigningBytes)}
	for _, change := range []func(*agentSubmission){
		func(s *agentSubmission) { s.Action.Genesis = "git:sha1:" + strings.Repeat("a", 40) },
		func(s *agentSubmission) { s.Action.ActorKey = pub },
		func(s *agentSubmission) { s.Action.Name = "Altered" },
		func(s *agentSubmission) { s.Action.IdempotencyKey = "altered" },
		func(s *agentSubmission) { s.Signature = bytes.Repeat([]byte{1}, 64) },
		func(s *agentSubmission) { s.Action.Action = "anchor" },
		func(s *agentSubmission) {
			s.Action.Predecessor = "git:sha1:" + strings.Repeat("a", 40) + "#git:sha1:" + strings.Repeat("b", 40)
		},
	} {
		changed := submission
		change(&changed)
		status := 400
		if changed.Action.Genesis != genesis {
			status = 409
		}
		postJSON(t, handler, "/v1/actions/native/submit", changed, nil, status)
	}
	for _, body := range []any{
		map[string]any{"action": input, "signature": submission.Signature, "draft": "browser-draft"},
		map[string]any{"prepared": prepared.Prepared, "signature": submission.Signature},
		map[string]any{"action": input, "signature": submission.Signature, "padding": strings.Repeat("x", 33<<10)},
	} {
		postJSON(t, handler, "/v1/actions/native/submit", body, nil, 400)
	}
	postJSON(t, handler, "/v1/actions/native/prepare?secret=bad", input, nil, 400)
	huge := input
	huge.Secret = strings.Repeat("x", 4097)
	postJSON(t, handler, "/v1/actions/native/prepare", huge, nil, 400)
	after, _ := ws.Records(ctx)
	if after.Head != before.Head {
		t.Fatal("invalid replay appended")
	}
	var created agentResult
	postJSON(t, handler, "/v1/actions/native/submit", submission, &created, 200)
	join := agentAction{Action: "join", Genesis: genesis, ActorKey: pub, Game: created.Record, IdempotencyKey: "typed-join"}
	postJSON(t, handler, "/v1/actions/native/prepare", join, &prepared, 200)
	var joined agentResult
	postJSON(t, handler, "/v1/actions/native/submit", agentSubmission{Action: join, Signature: ed25519.Sign(black, prepared.SigningBytes)}, &joined, 200)
	_, p, _ := owner.openView(ctx)
	game, _ := p.GameByID(created.Record)
	// A valid signature does not grant the wrong seat a turn. The actual fold
	// refusal remains a confirmed record, and the exact retry returns that record.
	wrong := agentAction{Action: "move", Genesis: genesis, ActorKey: pub, Game: created.Record, Move: "e7e5", Predecessor: game.LastMove, IdempotencyKey: "wrong-turn"}
	postJSON(t, handler, "/v1/actions/native/prepare", wrong, &prepared, 200)
	signed := agentSubmission{Action: wrong, Signature: ed25519.Sign(black, prepared.SigningBytes)}
	var refusal agentResult
	postJSON(t, handler, "/v1/actions/native/submit", signed, &refusal, 200)
	if refusal.Effective == nil || *refusal.Effective || refusal.Reason == "" {
		t.Fatalf("wrong seat confirmation: %#v", refusal)
	}
	var retry agentResult
	postJSON(t, handler, "/v1/actions/native/submit", signed, &retry, 200)
	if retry.Record != refusal.Record || retry.Depth != refusal.Depth {
		t.Fatal("ineffective retry was not exact")
	}
}

func TestAgentClientRefusesWrongServiceAndMalformedResponsesBeforeSigning(t *testing.T) {
	ctx := context.Background()
	repo, _, ws := newIdentityTestRepository(t, ctx)
	log, _ := ws.Records(ctx)
	genesis := canonicalAgentObject(log.Genesis)
	owner, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	live, _ := newChessLive()
	real := newReadHandlerWithIdentity(ctx, repo, live, identityHTTPConfig{}, owner)
	for _, name := range []string{"wrong-genesis", "wrong-protocol", "redirect", "oversize", "malformed", "altered-echo", "altered-signing-bytes"} {
		t.Run(name, func(t *testing.T) {
			var submits atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/actions/native/submit" {
					submits.Add(1)
				}
				if r.URL.Path == "/v1/service" {
					switch name {
					case "wrong-genesis":
						serveJSON(w, agentService{Genesis: "git:sha1:" + strings.Repeat("a", 40), Version: agentTransportVersion, Operations: agentOperations})
						return
					case "wrong-protocol":
						serveJSON(w, agentService{Genesis: genesis, Version: "unknown", Operations: agentOperations})
						return
					case "redirect":
						http.Redirect(w, r, "http://127.0.0.1:1", 302)
						return
					case "oversize":
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(strings.Repeat(" ", maxAgentResponse+1)))
						return
					case "malformed":
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"genesis":`))
						return
					}
				}
				if r.URL.Path == "/v1/actions/native/prepare" {
					recorder := httptest.NewRecorder()
					real.ServeHTTP(recorder, r)
					var p agentPreparation
					if err := json.Unmarshal(recorder.Body.Bytes(), &p); err != nil {
						http.Error(w, "fixture failed", 500)
						return
					}
					if name == "altered-echo" {
						p.Echo.Predecessor = "changed"
					} else {
						p.SigningBytes = []byte("changed")
					}
					serveJSON(w, p)
					return
				}
				real.ServeHTTP(w, r)
			}))
			defer fake.Close()
			keys := t.TempDir()
			path := agentKey(t, keys, "agent.key")
			if out, err := agentCommand(t, keys, "", "create", "--server", fake.URL, "--genesis", genesis, "--key", path); err == nil {
				t.Fatalf("bad service accepted: %s", out)
			}
			if submits.Load() != 0 {
				t.Fatal("bad service received a signature")
			}
			if _, err := os.Stat(filepath.Join(keys, ".agent.key.agent", "pending.json")); !os.IsNotExist(err) {
				t.Fatal("bad preparation retained a signed attempt")
			}
		})
	}
	after, _ := ws.Records(ctx)
	if after.Head != log.Head {
		t.Fatal("bad service appended")
	}
}

func TestAgentRetryWaitsForForgeConfirmation(t *testing.T) {
	ctx := context.Background()
	repo, remote, _, ws := forgeFixture(t)
	before, _ := ws.Records(ctx)
	genesis := canonicalAgentObject(before.Genesis)
	owner, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	actual := owner.git
	var outage atomic.Bool
	outage.Store(true)
	owner.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "push" && outage.Load() {
			return "", errors.New("test forge unavailable")
		}
		return actual(ctx, args...)
	}
	live, _ := newChessLive()
	server := httptest.NewServer(newReadHandlerWithIdentity(ctx, repo, live, identityHTTPConfig{}, owner))
	defer server.Close()
	keys := t.TempDir()
	path := agentKey(t, keys, "agent.key")
	out, err := agentCommand(t, keys, "", "create", "--name", "Pending forge", "--server", server.URL, "--genesis", genesis, "--key", path)
	if err == nil || !strings.Contains(string(out), "outcome not confirmed") {
		t.Fatalf("unconfirmed append acknowledged: %v %s", err, out)
	}
	pending, _ := ws.Records(ctx)
	if pending.Depth != before.Depth+1 {
		t.Fatal("fixture did not retain exactly one local append")
	}
	if head := forgeGit(t, remote, "rev-parse", "refs/seq/"+before.Genesis); head != before.Head {
		t.Fatal("outage fixture reached forge")
	}
	view, p, _ := owner.openView(ctx)
	if p.Head != before.Head {
		t.Fatal("pending game leaked to readers")
	}
	_ = view
	outage.Store(false)
	confirmed := agentSuccess(t, keys, "retry", "--server", server.URL, "--genesis", genesis, "--key", path)
	if confirmed.Depth != pending.Depth || confirmed.Head != pending.Head {
		t.Fatal("forge retry changed original history")
	}
	if head := forgeGit(t, remote, "rev-parse", "refs/seq/"+before.Genesis); head != confirmed.Head {
		t.Fatal("retry returned before exact forge confirmation")
	}
}
