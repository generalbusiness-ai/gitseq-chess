package chess

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
	rules "github.com/notnil/chess"
)

func TestSeatForAndFoldBothRefuseAnAmbiguousPersistentSeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "ambiguous-seat-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	_, rootKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	witnessPublic, witnessKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, candidateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := host.Init(ctx, repo, Application, rootKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, rootKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, candidateKey, host.Act{Schema: "test/candidate@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "alice"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: probe.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(MovePayload{Game: "game", Move: "e2e4"})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := workspace.Append(ctx, candidateKey, host.Act{
		Schema: SchemaMove, Payload: payload, RestsOn: []string{"join"},
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := Projection{
		Genesis: log.Genesis, Head: log.Head, Depth: log.Depth,
		ByID: map[string]int{"game": 0}, identities: identity.Resolve(log), lastRecordID: actual.ID,
		Games: []Game{{
			ID: "game", Status: "playing", LastMove: "join",
			engine:    rules.NewGame(rules.UseNotation(rules.UCINotation{})),
			whiteSeat: seat{actor: "white-key", identity: alice, anchored: true},
			blackSeat: seat{actor: "black-key", identity: alice, anchored: true},
		}},
	}
	projection.foldMove(actual)
	if projection.RefusedTotal != 1 || projection.Refused[0].Record != actual.ID {
		t.Fatalf("ambiguous actor fold verdict = %+v", projection.Refused)
	}
	if side, ok := projection.SeatFor("game", actual.Actor); ok || side != "" {
		t.Fatalf("ambiguous actor SeatFor = %q, %v", side, ok)
	}
}

func TestSeatForResolvesTheQueriedFingerprintAtTheFrontier(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "queried-fingerprint-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	_, rootKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	witnessPublic, witnessKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, seatedKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, strangerKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := host.Init(ctx, repo, Application, rootKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, rootKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	stranger, err := workspace.Append(ctx, strangerKey, host.Act{Schema: "test/stranger@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	seated, err := workspace.Append(ctx, seatedKey, host.Act{Schema: "test/seated@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "alice"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: seated.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	frontier, err := workspace.Append(ctx, seatedKey, host.Act{Schema: "test/frontier@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolution := identity.Resolve(log)
	if got := resolution.LookupAt(frontier.ID); !got.Anchored || got.Identity != alice {
		t.Fatalf("frontier author resolution = %+v, want anchored Alice", got)
	}
	if got := resolution.LookupActorAt(stranger.Actor, frontier.ID); got.Anchored {
		t.Fatalf("stranger resolution at frontier = %+v, want unanchored", got)
	}
	projection := Projection{
		Genesis: log.Genesis, Head: log.Head, Depth: log.Depth,
		ByID: map[string]int{"game": 0}, identities: resolution, lastRecordID: frontier.ID,
		Games: []Game{{
			ID: "game", Status: "playing", LastMove: "join",
			engine:    rules.NewGame(rules.UseNotation(rules.UCINotation{})),
			whiteSeat: seat{actor: seated.Actor, identity: alice, anchored: true},
			blackSeat: seat{actor: "black-key"},
		}},
	}
	if side, ok := projection.SeatFor("game", stranger.Actor); ok || side != "" {
		t.Fatalf("stranger SeatFor at seated actor frontier = %q, %v; want no seat", side, ok)
	}
}
