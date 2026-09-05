package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
)

func forgeGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func forgeFixture(t *testing.T) (string, string, ed25519.PrivateKey, *host.Workspace) {
	t.Helper()
	ctx := context.Background()
	repo, key, ws := newIdentityTestRepository(t, ctx)
	remote := filepath.Join(t.TempDir(), "forge.git")
	forgeGit(t, repo, "init", "--bare", "-q", remote)
	forgeGit(t, repo, "remote", "add", "forge", remote)
	log, err := ws.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/seq/" + log.Genesis
	forgeGit(t, repo, "push", "forge", ref)
	dir, err := gitCommonDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"chess.forgeRemote": "forge", "chess.forgeRef": ref, "chess.forgeGenesis": log.Genesis, "chess.sequencerKey": filepath.Join(dir, "gitseq", "sequencer")} {
		forgeGit(t, repo, "config", "--local", name, value)
	}
	return repo, remote, key, ws
}

func forgeCreate(t *testing.T, s *chessRepository, key ed25519.PrivateKey, idem string) host.SignedAct {
	t.Helper()
	payload, _ := json.Marshal(application.CreatePayload{CreatorColor: "white"})
	prepared, err := s.workspace.Prepare(host.Act{Schema: application.SchemaCreate, Payload: payload, IdempotencyKey: idem})
	if err != nil {
		t.Fatal(err)
	}
	data, err := host.ActorSigningBytes(prepared)
	if err != nil {
		t.Fatal(err)
	}
	return host.SignedAct{Prepared: prepared, ActorKey: key.Public().(ed25519.PublicKey), ActorSignature: ed25519.Sign(key, data)}
}

func TestForgeAppendWaitsForExactRemoteConfirmationAndRetriesOnce(t *testing.T) {
	ctx := context.Background()
	repo, remote, key, _ := forgeFixture(t)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	initial := s.confirmed
	actual := s.git
	pushes := 0
	s.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "push" {
			pushes++
			v, _ := s.view(ctx)
			if v.log.Head != initial.Head {
				t.Fatal("pending head was published before push")
			}
			_, err := actual(ctx, args...)
			if err != nil {
				return "", err
			}
			return "", errors.New("lost response after successful push")
		}
		return actual(ctx, args...)
	}
	act := forgeCreate(t, s, key, "lost-response")
	record, err := s.appendSigned(ctx, act)
	if err != nil {
		t.Fatal(err)
	}
	if pushes != 1 {
		t.Fatalf("pushes=%d", pushes)
	}
	v, err := s.view(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.log.Head == initial.Head || v.log.Depth != initial.Depth+1 || forgeGit(t, remote, "rev-parse", s.forge.ref) != v.log.Head {
		t.Fatal("success did not name the exact confirmed prefix")
	}
	retry, err := s.appendSigned(ctx, act)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.workspace.Records(ctx)
	if retry.ID != record.ID || after.Depth != v.log.Depth {
		t.Fatal("exact retry appended again")
	}
}

func TestForgeOutageFreezesIntakeAndEveryReadSurface(t *testing.T) {
	ctx := context.Background()
	repo, _, key, ws := forgeFixture(t)
	// An existing game makes a pending join observable on board and role reads.
	game, err := application.Create(ctx, ws, key, "white", "", "", "existing-game")
	if err != nil {
		t.Fatal(err)
	}
	log, _ := ws.Records(ctx)
	forgeGit(t, repo, "push", "forge", "refs/seq/"+log.Genesis)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.confirmed
	actual := s.git
	s.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "push" {
			return "", errors.New("forge outage")
		}
		return actual(ctx, args...)
	}
	_, black := generateIdentityKey(t)
	join, _ := application.JoinAct(game.ID, "", "pending-join")
	prepared, _ := s.workspace.Prepare(join)
	data, _ := host.ActorSigningBytes(prepared)
	act := host.SignedAct{Prepared: prepared, ActorKey: black.Public().(ed25519.PublicKey), ActorSignature: ed25519.Sign(black, data)}
	record, err := s.appendSigned(ctx, act)
	if !errors.Is(err, errDeliveryPending) || record.ID == "" {
		t.Fatalf("pending append=%s,%v", record.ID, err)
	}
	local, _ := s.workspace.Records(ctx)
	if local.Depth != before.Depth+1 {
		t.Fatal("fixture did not create pending history")
	}
	if _, err = s.appendSigned(ctx, forgeCreate(t, s, key, "must-not-append")); !errors.Is(err, errDeliveryPending) {
		t.Fatalf("new intake=%v", err)
	}
	localAfter, _ := s.workspace.Records(ctx)
	if localAfter.Depth != local.Depth {
		t.Fatal("new record admitted while pending")
	}
	runtime, _ := newChessLive()
	handler := newReadHandlerWithIdentity(ctx, repo, runtime, identityHTTPConfig{}, s)
	for _, path := range []string{"/v1/games", "/v1/board?game=" + game.ID, "/game?game=" + game.ID, "/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), before.Head) {
			t.Fatalf("%s did not serve confirmed head: %d %s", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), record.ID) {
			t.Fatalf("%s leaked pending join", path)
		}
	}
	view, projection, err := s.openView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := projection.SeatFor(game.ID, liveFingerprint(black.Public().(ed25519.PublicKey))); ok {
		t.Fatal("live role granted by pending join")
	}
	var identityView map[string]any
	postJSON(t, handler, "/v1/identity/status", identityActorRequest{ActorKey: black.Public().(ed25519.PublicKey)}, &identityView, 200)
	if identityView["head"] != before.Head {
		t.Fatalf("identity read leaked pending head: %#v", identityView)
	}
	if _, err = view.Prepare(join); !errors.Is(err, errDeliveryPending) {
		t.Fatal("prepared new work while pending")
	}
	_, cliProjection, err := readConfirmed(ctx, repo)
	if err != nil || cliProjection.Head != before.Head {
		t.Fatalf("CLI read=%s,%v", cliProjection.Head, err)
	}
	// Restoring transport confirms the same local history, without another act.
	s.git = actual
	if err = s.confirm(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := s.view(ctx)
	if v.log.Head != local.Head || v.log.Depth != local.Depth {
		t.Fatal("recovery changed the pending history")
	}
}

func TestForgeRestartReconcilesPendingHistoryAndRefusesDivergence(t *testing.T) {
	ctx := context.Background()
	repo, remote, key, ws := forgeFixture(t)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := s.confirmed
	// This is the precise crash seam: host append completed, push did not run.
	record, err := ws.AppendSigned(ctx, forgeCreate(t, s, key, "crash"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	restarted, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(record.ID, ":"+restarted.confirmed.Head) {
		t.Fatal("restart did not confirm original record")
	}
	restarted.Close()
	// A forge rollback is divergence from the retained confirmed frontier.
	forgeGit(t, remote, "update-ref", "refs/seq/"+confirmed.Genesis, confirmed.Head)
	if reopened, err := openChessRepository(ctx, repo); err == nil {
		reopened.Close()
		t.Fatal("restart accepted forge rollback")
	}
}

func TestWriterOwnershipStopsOtherEntryPointsAndDetectsReplacement(t *testing.T) {
	ctx := context.Background()
	repo, _, _, ws := forgeFixture(t)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, _ := ws.Records(ctx)
	for _, args := range [][]string{{"create", "--repo", repo}, {"rebind", "--repo", repo}, {"init", "--repo", repo}} {
		var out bytes.Buffer
		if err := run(ctx, args, &out, strings.NewReader("rebind\n")); err == nil {
			t.Fatalf("second writer accepted %v", args)
		}
	}
	if another, err := acquireWriter(ctx, repo); err == nil {
		another.Close()
		t.Fatal("second owner acquired")
	}
	after, _ := ws.Records(ctx)
	if after.Depth != before.Depth {
		t.Fatal("second writer changed sequence")
	}
	// A lock-file replacement must stop intake even while the old fd is open.
	dir, _ := gitCommonDir(ctx, repo)
	path := filepath.Join(dir, "chess-writer.lock")
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ready(ctx); err == nil {
		t.Fatal("replaced ownership did not stop intake")
	}
}

func TestForgeProcessHelper(t *testing.T) {
	repo := os.Getenv("CHESS_TEST_FORGE_REPO")
	if repo == "" {
		return
	}
	s, err := openChessRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("CHESS_TEST_FORGE_ACT"))
	if err != nil {
		t.Fatal(err)
	}
	var act host.SignedAct
	if err = json.Unmarshal(data, &act); err != nil {
		t.Fatal(err)
	}
	// Deliberately stop at the append/push crash seam. The parent kills this
	// actual process, so cleanup cannot release the lease on its behalf.
	record, err := s.workspace.AppendSigned(context.Background(), act)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stdout.WriteString(record.ID + "\n"); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	_, _ = os.Stdin.Read(one[:])
}

func TestForgeProcessDeathReleasesOwnershipAndConfirmsOriginalAppend(t *testing.T) {
	repo, remote, key, ws := forgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	initial := s.confirmed
	act := forgeCreate(t, s, key, "actual-process-crash")
	s.Close()
	data, _ := json.Marshal(act)
	actFile := filepath.Join(t.TempDir(), "signed-act.json")
	if err = os.WriteFile(actFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForgeProcessHelper$")
	command.Env = append(os.Environ(), "CHESS_TEST_FORGE_REPO="+repo, "CHESS_TEST_FORGE_ACT="+actFile)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	})
	record, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper: %v %s", err, stderr.String())
	}
	record = strings.TrimSpace(record)
	if second, err := openChessRepository(ctx, repo); err == nil {
		second.Close()
		t.Fatal("second process could become writer")
	}
	_, read, err := readConfirmed(ctx, repo)
	if err != nil || read.Head != initial.Head {
		t.Fatal("read-only CLI saw crashed pending append")
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("helper was not killed")
	}
	restarted, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	log, err := ws.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if log.Depth != initial.Depth+1 || !strings.HasSuffix(record, ":"+log.Head) || restarted.confirmed.Head != log.Head || forgeGit(t, remote, "rev-parse", restarted.forge.ref) != log.Head {
		t.Fatal("restart replaced or lost the original append")
	}
}

func TestForgeConfigurationCannotFallBackToNativeWrites(t *testing.T) {
	for _, field := range []string{"chess.forgeRef", "chess.forgeGenesis", "chess.sequencerKey"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			repo, _, _, ws := forgeFixture(t)
			before, _ := ws.Records(ctx)
			forgeGit(t, repo, "config", "--local", "--unset", field)
			var out bytes.Buffer
			if err := run(ctx, []string{"create", "--repo", repo}, &out, strings.NewReader("")); err == nil {
				t.Fatal("incomplete forge config fell back to local append")
			}
			after, _ := ws.Records(ctx)
			if after.Depth != before.Depth {
				t.Fatal("configuration refusal changed history")
			}
		})
	}
}

func TestForgeConfirmsOnLastAttemptAndRecoversTransientAncestryFailure(t *testing.T) {
	ctx := context.Background()
	repo, _, key, _ := forgeFixture(t)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actual := s.git
	pushes := 0
	s.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "push" {
			pushes++
			if pushes < 3 {
				return "", errors.New("temporary transport failure")
			}
		}
		return actual(ctx, args...)
	}
	if _, err := s.appendSigned(ctx, forgeCreate(t, s, key, "third-attempt")); err != nil {
		t.Fatal(err)
	}
	if pushes != 3 {
		t.Fatalf("pushes = %d", pushes)
	}
	before := s.confirmed.Head
	s.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "merge-base" {
			return "", context.DeadlineExceeded
		}
		return actual(ctx, args...)
	}
	r, err := s.appendSigned(ctx, forgeCreate(t, s, key, "ancestry-outage"))
	if !errors.Is(err, errDeliveryPending) || s.stopped != nil || s.confirmed.Head != before {
		t.Fatalf("temporary check failed permanently or exposed pending state: %v / %v", err, s.stopped)
	}
	s.git = actual
	if err := s.confirm(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.ID, ":"+s.confirmed.Head) {
		t.Fatal("recovery did not confirm original append")
	}
}

func TestForgeRemoteMustNameOneMatchingDestination(t *testing.T) {
	for _, kind := range []string{"missing", "multiple-fetch", "different-push"} {
		t.Run(kind, func(t *testing.T) {
			repo, remote, _, _ := forgeFixture(t)
			switch kind {
			case "missing":
				forgeGit(t, repo, "config", "chess.forgeRemote", ".")
			case "multiple-fetch":
				forgeGit(t, repo, "config", "--add", "remote.forge.url", remote+"-other")
			case "different-push":
				forgeGit(t, repo, "config", "remote.forge.pushurl", remote+"-other")
			}
			if s, err := openChessRepository(context.Background(), repo); err == nil {
				s.Close()
				t.Fatal("ambiguous forge destination accepted")
			}
		})
	}
}

func TestWriterOwnershipIsSharedByLinkedWorktrees(t *testing.T) {
	ctx := context.Background()
	repo, _, _, _ := forgeFixture(t)
	forgeGit(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "Fixture checkout")
	linked := filepath.Join(t.TempDir(), "linked")
	forgeGit(t, repo, "worktree", "add", "--detach", linked)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := openChessRepository(ctx, linked); err == nil {
		other.Close()
		t.Fatal("linked worktree acquired a second writer")
	}
}

func TestWriterOwnershipAndForgeConfigurationIgnoreAmbientGitOverrides(t *testing.T) {
	ctx := context.Background()
	repo, remote, _, _ := forgeFixture(t)
	s, err := openChessRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", remote)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "chess.forgeRemote")
	t.Setenv("GIT_CONFIG_VALUE_0", "missing")
	if other, err := openChessRepository(ctx, repo); err == nil {
		other.Close()
		t.Fatal("ambient Git directory bypassed held ownership")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openChessRepository(ctx, repo)
	if err != nil {
		t.Fatalf("ambient configuration changed explicit forge attachment: %v", err)
	}
	defer s.Close()
}

func TestForgeRejectsUnexpectedBindingBeforeCustody(t *testing.T) {
	ctx := context.Background()
	repo, remote, key, _ := forgeFixture(t)
	foreign := t.TempDir()
	forgeGit(t, foreign, "init", "-q")
	profile := application.Application
	profile.Name = "other-application"
	ws, err := host.Init(ctx, foreign, profile, key, host.Options{})
	if err != nil {
		t.Fatal(err)
	}
	log, err := ws.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forgeGit(t, foreign, "push", remote, "refs/seq/"+log.Genesis)
	forgeGit(t, repo, "config", "chess.forgeGenesis", log.Genesis)
	forgeGit(t, repo, "config", "chess.forgeRef", "refs/seq/"+log.Genesis)
	forgeGit(t, repo, "config", "chess.sequencerKey", filepath.Join(t.TempDir(), "unavailable-key"))
	if s, err := openChessRepository(ctx, repo); err == nil {
		s.Close()
		t.Fatal("foreign binding accepted")
	} else if !errors.Is(err, host.ErrUninterpretable) {
		t.Fatalf("expected binding refusal before key custody, got %v", err)
	}
}
