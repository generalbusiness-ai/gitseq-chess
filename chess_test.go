package chess_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	chess "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

const (
	white = "1111111111111111111111111111111111111111111111111111111111111111"
	black = "2222222222222222222222222222222222222222222222222222222222222222"
	third = "3333333333333333333333333333333333333333333333333333333333333333"
)

type logBuilder struct {
	records []host.Record
	next    int64
}

func (b *logBuilder) add(id, actor, schema string, body any, restsOn ...string) host.Record {
	b.next++
	payload, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	record := host.Record{ID: id, Actor: actor, Schema: schema, Payload: payload, RestsOn: restsOn, Timestamp: b.next}
	b.records = append(b.records, record)
	return record
}

func (b *logBuilder) raw(id, actor, schema string, payload []byte, restsOn ...string) {
	b.next++
	b.records = append(b.records, host.Record{ID: id, Actor: actor, Schema: schema, Payload: payload, RestsOn: restsOn, Timestamp: b.next})
}

func (b *logBuilder) fold() chess.Projection {
	return chess.Fold(host.Log{Genesis: "genesis", Head: "head", Depth: len(b.records), Records: b.records})
}

func joinedGame(t *testing.T) (*logBuilder, chess.Game) {
	t.Helper()
	b := &logBuilder{}
	b.add("game", white, chess.SchemaCreate, chess.CreatePayload{CreatorColor: "white"})
	b.add("join", black, chess.SchemaJoin, chess.JoinPayload{Game: "game"}, "game")
	projection := b.fold()
	game, ok := projection.GameByID("game")
	if !ok {
		t.Fatal("fold did not create game")
	}
	return b, game
}

func TestFoldSeatsTheFirstQualifiedJoinAndRefusesTheRest(t *testing.T) {
	b := &logBuilder{}
	b.add("game", white, chess.SchemaCreate, chess.CreatePayload{CreatorColor: "black"})
	b.add("join-1", black, chess.SchemaJoin, chess.JoinPayload{Game: "game"}, "game")
	b.add("join-2", third, chess.SchemaJoin, chess.JoinPayload{Game: "game"}, "game")

	projection := b.fold()
	game, ok := projection.GameByID("game")
	if !ok || game.White != black || game.Black != white || game.Status != "playing" {
		t.Fatalf("game = %+v, want first joiner white and creator black", game)
	}
	if len(projection.Refused) != 1 || projection.Refused[0].Record != "join-2" {
		t.Fatalf("refusals = %+v, want only the second join", projection.Refused)
	}
}

func TestInvitationByKeyAndSecretAreEnforced(t *testing.T) {
	digest := sha256.Sum256([]byte("correct horse"))
	b := &logBuilder{}
	b.add("key-game", white, chess.SchemaCreate, chess.CreatePayload{
		CreatorColor: "white", Invitation: &chess.Invitation{OpponentKey: black},
	})
	b.add("wrong-key", third, chess.SchemaJoin, chess.JoinPayload{Game: "key-game"}, "key-game")
	b.add("right-key", black, chess.SchemaJoin, chess.JoinPayload{Game: "key-game"}, "key-game")
	b.add("secret-game", white, chess.SchemaCreate, chess.CreatePayload{
		CreatorColor: "white", Invitation: &chess.Invitation{SecretHash: hex.EncodeToString(digest[:])},
	})
	b.add("wrong-secret", black, chess.SchemaJoin, chess.JoinPayload{Game: "secret-game", Secret: "wrong"}, "secret-game")
	b.add("right-secret", third, chess.SchemaJoin, chess.JoinPayload{Game: "secret-game", Secret: "correct horse"}, "secret-game")

	projection := b.fold()
	keyGame, _ := projection.GameByID("key-game")
	secretGame, _ := projection.GameByID("secret-game")
	if keyGame.Black != black || secretGame.Black != third {
		t.Fatalf("qualified seats = %q and %q, want %q and %q", keyGame.Black, secretGame.Black, black, third)
	}
	if len(projection.Refused) != 2 {
		t.Fatalf("refused %d acts, want the two failed invitations", len(projection.Refused))
	}
}

func TestImpossibleInvitedFingerprintIsRefusedAtCreation(t *testing.T) {
	b := &logBuilder{}
	b.add("bad", white, chess.SchemaCreate, chess.CreatePayload{
		CreatorColor: "white", Invitation: &chess.Invitation{OpponentKey: "not-a-fingerprint"},
	})
	projection := b.fold()
	if len(projection.Games) != 0 || projection.RefusedTotal != 1 {
		t.Fatalf("projection = %+v, want impossible invitation refused", projection)
	}
}

// This is a mutation witness for both turn authority and the causal chain. If
// either guard is removed, one of the two illegal moves changes the position.
func TestMoveRequiresTheRightTurnAndExactPriorMove(t *testing.T) {
	b, _ := joinedGame(t)
	b.add("wrong-turn", black, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "e2e4"}, "join")
	b.add("wrong-chain", white, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "d2d4"}, "game")
	b.add("white-1", white, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "e2e4"}, "join")
	b.add("replay-chain", black, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "d7d5"}, "join")
	b.add("black-1", black, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "e7e5"}, "white-1")

	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Moves != 2 || game.LastMove != "black-1" || game.LastMoveUCI != "e7e5" {
		t.Fatalf("accepted chain = %+v, want e2e4/e7e5 only", game)
	}
	if len(projection.Refused) != 3 {
		t.Fatalf("refusals = %+v, want wrong turn and both wrong bases", projection.Refused)
	}
}

// This is a mutation witness for the source-square filter: returning every
// move, pseudo-legal moves, or the wrong side's moves cannot satisfy it.
func TestLegalDestinationsComeFromTheFoldEngine(t *testing.T) {
	_, game := joinedGame(t)
	want := []string{"e3", "e4"}
	if got := chess.LegalDestinations(game, "e2"); !reflect.DeepEqual(got, want) {
		t.Fatalf("destinations = %v, want %v", got, want)
	}
	if got := chess.LegalDestinations(game, "e7"); len(got) != 0 {
		t.Fatalf("black destinations on white's turn = %v, want none", got)
	}
	if got := chess.LegalDestinations(game, "E2"); len(got) != 0 {
		t.Fatalf("non-canonical source = %v, want none", got)
	}
}

func TestFoldComputesCheckmateWithoutAResultAct(t *testing.T) {
	b, _ := joinedGame(t)
	chain := "join"
	for index, play := range []struct {
		actor, move string
	}{{white, "f2f3"}, {black, "e7e5"}, {white, "g2g4"}, {black, "d8h4"}} {
		id := []string{"w1", "b1", "w2", "mate"}[index]
		b.add(id, play.actor, chess.SchemaMove, chess.MovePayload{Game: "game", Move: play.move}, chain)
		chain = id
	}

	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Status != "finished" || game.Outcome != "black-wins" || game.Method != "Checkmate" {
		t.Fatalf("mate = %+v, want fold-computed black checkmate", game)
	}
	if len(projection.Refused) != 0 {
		t.Fatalf("mate sequence was refused: %+v", projection.Refused)
	}
}

func TestFoldComputesStalemateWithoutAResultAct(t *testing.T) {
	b, _ := joinedGame(t)
	chain := "join"
	sequence := []string{
		"e2e3", "a7a5", "d1h5", "a8a6", "h5a5", "h7h5", "a5c7", "a6h6", "h2h4", "f7f6",
		"c7d7", "e8f7", "d7b7", "d8d3", "b7b8", "d3h7", "b8c8", "f7g6", "c8e6",
	}
	for index, move := range sequence {
		actor := white
		if index%2 == 1 {
			actor = black
		}
		id := fmt.Sprintf("move-%02d", index+1)
		b.add(id, actor, chess.SchemaMove, chess.MovePayload{Game: "game", Move: move}, chain)
		chain = id
	}
	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Status != "finished" || game.Outcome != "draw" || game.Method != "Stalemate" || projection.RefusedTotal != 0 {
		t.Fatalf("stalemate = %+v, refusals %+v", game, projection.Refused)
	}
}

func TestDrawRequiresTheOtherPlayerAndExactOffer(t *testing.T) {
	b, _ := joinedGame(t)
	b.add("offer", white, chess.SchemaDrawOffer, chess.GamePayload{Game: "game"}, "join")
	b.add("self-accept", white, chess.SchemaDrawAccept, chess.DrawAcceptPayload{Game: "game", Offer: "offer"}, "offer")
	b.add("wrong-offer", black, chess.SchemaDrawAccept, chess.DrawAcceptPayload{Game: "game", Offer: "missing"}, "missing")
	b.add("accept", black, chess.SchemaDrawAccept, chess.DrawAcceptPayload{Game: "game", Offer: "offer"}, "offer")

	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Status != "finished" || game.Outcome != "draw" || game.Method != "agreement" {
		t.Fatalf("game = %+v, want agreed draw", game)
	}
	if len(projection.Refused) != 2 {
		t.Fatalf("refusals = %+v, want self-accept and wrong offer", projection.Refused)
	}
}

func TestResignationIsLimitedToASeatAtTheCurrentChain(t *testing.T) {
	b, _ := joinedGame(t)
	b.add("stranger", third, chess.SchemaResign, chess.GamePayload{Game: "game"}, "join")
	b.add("stale", white, chess.SchemaResign, chess.GamePayload{Game: "game"}, "game")
	b.add("resign", white, chess.SchemaResign, chess.GamePayload{Game: "game"}, "join")
	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Outcome != "black-wins" || game.Method != "resignation" {
		t.Fatalf("game = %+v, want white resignation", game)
	}
	if len(projection.Refused) != 2 {
		t.Fatalf("refusals = %+v, want stranger and stale resignation", projection.Refused)
	}
}

func TestMalformedAndUnknownRecordsCannotBreakReplay(t *testing.T) {
	b, _ := joinedGame(t)
	b.raw("spaced", white, chess.SchemaMove, []byte(`{ "game":"game","move":"e2e4"}`), "join")
	b.raw("unknown-field", white, chess.SchemaMove, []byte(`{"game":"game","move":"e2e4","x":1}`), "join")
	b.raw("oversize", white, chess.SchemaMove, make([]byte, 9<<10), "join")
	b.raw("opaque", white, "another/application@0", []byte("anything"))

	first := b.fold()
	second := b.fold()
	game, _ := first.GameByID("game")
	if game.Moves != 0 || len(first.Refused) != 3 {
		t.Fatalf("projection = %+v, want three malformed chess refusals and opaque unknown act", first)
	}
	first.ByID, second.ByID = nil, nil
	if !reflect.DeepEqual(first, second) {
		t.Fatal("folding the same malformed log twice gave different projections")
	}
}

func TestSharedAnchorVocabularyIsOpaqueAndNonDisruptiveToChess(t *testing.T) {
	b, _ := joinedGame(t)
	// Identity implementation belongs to the host. Even a malformed anchor
	// remains opaque here: it is neither a chess refusal nor a link in the move
	// chain, and cannot make the next valid chess act unreadable.
	b.raw("anchor", white, chess.SchemaAnchor, []byte(`{"not":"chess"}`))
	b.add("move", white, chess.SchemaMove, chess.MovePayload{Game: "game", Move: "e2e4"}, "join")
	projection := b.fold()
	game, _ := projection.GameByID("game")
	if game.Moves != 1 || game.LastMove != "move" || projection.RefusedTotal != 0 {
		t.Fatalf("anchor disrupted chess projection: game %+v, refusals %+v", game, projection.Refused)
	}
}

func TestAnchoredSeatRecoveryUsesExactRecordOrder(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "recovery-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, recoveryKey := key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, recoveryKey, host.Act{Schema: "test/recovery-key@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242", Handle: "alice"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: actorOf(whiteKey), Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "recovery-create")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "recovery-join")
	if err != nil {
		t.Fatal(err)
	}

	// The recovered key first acts without authority, then receives a scoped
	// delegation and acts again. All timestamps are tied below while retaining
	// the real host-generated record identifiers, actors and identity payloads.
	beforeAnchor, err := chess.Move(ctx, workspace, recoveryKey, created.ID, "e2e4", "before-anchor")
	if err != nil {
		t.Fatal(err)
	}
	// A provider handle is display text, not identity. Re-anchoring the seated
	// key with an updated handle must not split Alice into two seat owners.
	alice.Handle = "alice-now"
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{
		Subject: probe.Actor, Scope: "chess:" + created.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	afterAnchor, err := chess.Move(ctx, workspace, recoveryKey, created.ID, "e2e4", "after-anchor")
	if err != nil {
		t.Fatal(err)
	}
	blackMove, err := chess.Move(ctx, workspace, blackKey, created.ID, "e7e5", "black-one")
	if err != nil {
		t.Fatal(err)
	}
	beforeRevoke, err := chess.Move(ctx, workspace, recoveryKey, created.ID, "g1f3", "before-revoke")
	if err != nil {
		t.Fatal(err)
	}
	blackTwo, err := chess.Move(ctx, workspace, blackKey, created.ID, "b8c6", "black-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Revoke(ctx, workspace, whiteKey, credential.ID); err != nil {
		t.Fatal(err)
	}
	afterRevoke, err := chess.Move(ctx, workspace, recoveryKey, created.ID, "f1b5", "after-revoke")
	if err != nil {
		t.Fatal(err)
	}

	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := range log.Records {
		log.Records[index].Timestamp = 1_000
	}
	projection := chess.Fold(log)
	game, ok := projection.GameByID(created.ID)
	if !ok || game.Moves != 4 || game.LastMove != blackTwo.ID || game.LastMoveUCI != "b8c6" {
		t.Fatalf("recovered game = %+v, want four moves ending at black's second move", game)
	}
	for _, want := range []string{beforeAnchor.ID, afterRevoke.ID} {
		found := false
		for _, refusal := range projection.Refused {
			found = found || refusal.Record == want
		}
		if !found {
			t.Fatalf("record %s was not refused at its exact identity position: %+v", want, projection.Refused)
		}
	}
	for _, want := range []string{afterAnchor.ID, blackMove.ID, beforeRevoke.ID, blackTwo.ID, joined.ID} {
		for _, refusal := range projection.Refused {
			if refusal.Record == want {
				t.Fatalf("record %s was retroactively refused by a same-second identity boundary: %+v", want, refusal)
			}
		}
	}
}

func TestSeatCanUpgradeAfterPlayingUnanchored(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "late-anchor-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, recoveredKey := key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "late-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "late-join"); err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	// The exact seated key remains valid for its first post-anchor move; that
	// move upgrades the seat to Alice's persistent identity.
	if _, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e4", "late-white"); err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Move(ctx, workspace, blackKey, created.ID, "e7e5", "late-black"); err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, recoveredKey, host.Act{Schema: "test/recovered@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{Subject: probe.Actor, Scope: "chess"}); err != nil {
		t.Fatal(err)
	}
	move, err := chess.Move(ctx, workspace, recoveredKey, created.ID, "g1f3", "late-recovered")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 3 || game.LastMove != move.ID || projection.RefusedTotal != 0 {
		t.Fatalf("late anchor recovery = game %+v refusals %+v", game, projection.Refused)
	}
}

func TestExpiredOrWrongScopeAnchorCannotRecoverASeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "bounded-anchor-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, expiredKey, watcherKey := key(t), key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: actorOf(whiteKey), Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "bounded-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "bounded-join"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		key      ed25519.PrivateKey
		scope    string
		notAfter int64
		move     string
	}{
		{expiredKey, "chess", 1, "e2e4"},
		{watcherKey, "watch", 0, "d2d4"},
	} {
		probe, err := workspace.Append(ctx, candidate.key, host.Act{Schema: "test/candidate@0", Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{
			Subject: probe.Actor, Scope: candidate.scope, NotAfter: candidate.notAfter,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := chess.Move(ctx, workspace, candidate.key, created.ID, candidate.move, "bounded-"+candidate.scope); err != nil {
			t.Fatal(err)
		}
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 0 || projection.RefusedTotal != 2 {
		t.Fatalf("bounded recovery = game %+v refusals %+v", game, projection.Refused)
	}
}

func TestSamePersistentIdentityCannotTakeBothSeats(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "identity-collision-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey := key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	blackProbe, err := workspace.Append(ctx, blackKey, host.Act{Schema: "test/black-key@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242", Handle: "alice"}
	for _, actor := range []string{actorOf(whiteKey), blackProbe.Actor} {
		if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
			Subject: actor, Identity: &alice, Scope: "chess",
		}); err != nil {
			t.Fatal(err)
		}
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "collision-create")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "collision-join")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Status != "open" || game.Black != "" || game.LastMove != "" {
		t.Fatalf("same-identity join occupied black: %+v", game)
	}
	if projection.RefusedTotal != 1 || projection.Refused[0].Record != joined.ID ||
		projection.Refused[0].Reason != "one persistent identity cannot hold both seats" {
		t.Fatalf("same-identity join refusals = %+v", projection.Refused)
	}
}

// Mutation witness for the runtime guard: join collision is not enough when
// two exact-key seats acquire the same persistent identity only after seating.
func TestSecondSeatCannotUpgradeToTheFirstSeatsIdentity(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "late-collision-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey := key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "late-collision-create")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "late-collision-join")
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	for _, actor := range []string{created.Actor, joined.Actor} {
		if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
			Subject: actor, Identity: &alice, Scope: "chess",
		}); err != nil {
			t.Fatal(err)
		}
	}
	whiteMove, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e4", "late-collision-white")
	if err != nil {
		t.Fatal(err)
	}
	blackMove, err := chess.Move(ctx, workspace, blackKey, created.ID, "e7e5", "late-collision-black")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 1 || game.LastMove != whiteMove.ID || game.LastMoveUCI != "e2e4" {
		t.Fatalf("late identity collision moved both colors: %+v", game)
	}
	if projection.RefusedTotal != 1 || projection.Refused[0].Record != blackMove.ID {
		t.Fatalf("late identity collision refusals = %+v", projection.Refused)
	}
}

// Mutation witness for every act using the shared two-seat authority check:
// an exact seated key cannot borrow the other seat's persistent identity while
// its own identity resolves to the same owner.
func TestExactSeatedKeyCannotBorrowTheOtherSeatsIdentity(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "cross-seat-collision-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey := key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "cross-seat-create")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "cross-seat-join")
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: actorOf(whiteKey), Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	blackAnchor, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: joined.Actor, Identity: &alice, Scope: "chess:" + created.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	whiteMove, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e4", "cross-seat-white")
	if err != nil {
		t.Fatal(err)
	}
	project := func() chess.Projection {
		t.Helper()
		log, err := workspace.Records(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return chess.Fold(log)
	}
	projection := project()
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 1 || game.LastMove != whiteMove.ID || game.LastMoveUCI != "e2e4" || projection.RefusedTotal != 0 {
		t.Fatalf("white did not upgrade on its first effective act: game %+v refusals %+v", game, projection.Refused)
	}
	if _, err := identity.Revoke(ctx, workspace, witnessKey, blackAnchor.ID); err != nil {
		t.Fatal(err)
	}
	blackMove, err := chess.Move(ctx, workspace, blackKey, created.ID, "e7e5", "cross-seat-black")
	if err != nil {
		t.Fatal(err)
	}
	drawOffer, err := chess.OfferDraw(ctx, workspace, blackKey, created.ID, "cross-seat-draw-offer")
	if err != nil {
		t.Fatal(err)
	}
	projection = project()
	game, _ = projection.GameByID(created.ID)
	if game.Moves != 2 || game.LastMove != blackMove.ID || game.LastMoveUCI != "e7e5" ||
		game.DrawOffer != drawOffer.ID || projection.RefusedTotal != 0 {
		t.Fatalf("withdrawn black key did not act as its exact seat: game %+v refusals %+v", game, projection.Refused)
	}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: joined.Actor, Identity: &alice, Scope: "chess:" + created.ID,
	}); err != nil {
		t.Fatal(err)
	}
	projection = project()
	game, _ = projection.GameByID(created.ID)
	if game.Moves != 2 || game.LastMove != blackMove.ID || game.DrawOffer != drawOffer.ID || projection.RefusedTotal != 0 {
		t.Fatalf("re-anchoring changed the game before an act: game %+v refusals %+v", game, projection.Refused)
	}
	appendAttempt := func(schema string, body any, restsOn string) host.Record {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		record, err := workspace.Append(ctx, blackKey, host.Act{
			Schema: schema, Payload: payload, RestsOn: []string{restsOn},
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	// Append against the accepted causal references directly so the witness
	// remains stable even when a mutant incorrectly makes an earlier attempt
	// effective and changes the convenience writers' projected references.
	borrowedMove := appendAttempt(chess.SchemaMove, chess.MovePayload{Game: created.ID, Move: "g1f3"}, blackMove.ID)
	borrowedResign := appendAttempt(chess.SchemaResign, chess.GamePayload{Game: created.ID}, blackMove.ID)
	borrowedOffer := appendAttempt(chess.SchemaDrawOffer, chess.GamePayload{Game: created.ID}, blackMove.ID)
	borrowedAccept := appendAttempt(chess.SchemaDrawAccept, chess.DrawAcceptPayload{Game: created.ID, Offer: drawOffer.ID}, drawOffer.ID)
	projection = project()
	game, _ = projection.GameByID(created.ID)
	if game.Status != "playing" || game.Moves != 2 || game.LastMove != blackMove.ID ||
		game.LastMoveUCI != "e7e5" || game.DrawOffer != drawOffer.ID {
		t.Fatalf("other seat's exact key borrowed white: %+v", game)
	}
	wantRefused := []string{borrowedMove.ID, borrowedResign.ID, borrowedOffer.ID, borrowedAccept.ID}
	if projection.RefusedTotal != len(wantRefused) || len(projection.Refused) != len(wantRefused) {
		t.Fatalf("cross-seat collision refusals = %+v", projection.Refused)
	}
	for index, want := range wantRefused {
		if projection.Refused[index].Record != want {
			t.Fatalf("cross-seat collision refusal %d = %+v, want %s", index, projection.Refused[index], want)
		}
	}
}

// An opposing key remains the opposing key even after its own seat has
// upgraded to one identity and the key later resolves to the other seat's
// identity. Without the exact-key guard in matchSeat, identity matching alone
// lets the black key recover white here.
func TestOpposingExactKeyCannotRecoverAnAnchoredSeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "anchored-cross-seat-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey := key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "anchored-cross-create")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "anchored-cross-join")
	if err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "alice"}
	bob := identity.Identity{Scheme: identity.GitHubScheme, Subject: "bob"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess:" + created.ID,
	}); err != nil {
		t.Fatal(err)
	}
	blackAnchor, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: joined.Actor, Identity: &bob, Scope: "chess:" + created.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e4", "anchored-cross-white"); err != nil {
		t.Fatal(err)
	}
	blackMove, err := chess.Move(ctx, workspace, blackKey, created.ID, "e7e5", "anchored-cross-black")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Revoke(ctx, workspace, witnessKey, blackAnchor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: joined.Actor, Identity: &alice, Scope: "chess:" + created.ID,
	}); err != nil {
		t.Fatal(err)
	}
	borrowed, err := chess.Move(ctx, workspace, blackKey, created.ID, "g1f3", "anchored-cross-borrow")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 2 || game.LastMove != blackMove.ID || game.LastMoveUCI != "e7e5" {
		t.Fatalf("opposing exact key recovered white: %+v", game)
	}
	if projection.RefusedTotal != 1 || len(projection.Refused) != 1 || projection.Refused[0].Record != borrowed.ID {
		t.Fatalf("opposing exact-key refusals = %+v", projection.Refused)
	}
}

// Mutation witness: committing the late identity upgrade inside the authority
// check, before MoveStr accepts the move, lets the recovered key play e2e4.
func TestIllegalMoveCannotUpgradeAnUnanchoredSeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "illegal-upgrade-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, recoveredKey := key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "illegal-upgrade-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "illegal-upgrade-join"); err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	illegal, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e5", "illegal-upgrade-attempt")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, recoveredKey, host.Act{Schema: "test/recovered-key@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{Subject: probe.Actor, Scope: "chess"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := chess.Move(ctx, workspace, recoveredKey, created.ID, "e2e4", "illegal-upgrade-recovered")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 0 || projection.RefusedTotal != 2 {
		t.Fatalf("illegal upgrade changed authority: game %+v refusals %+v", game, projection.Refused)
	}
	if projection.Refused[0].Record != illegal.ID || projection.Refused[1].Record != recovered.ID {
		t.Fatalf("illegal upgrade refusals = %+v", projection.Refused)
	}
}

// Mutation witness for the draw path: a second pending offer reaches the seat
// check but remains ineffective, so it cannot carry a later recovery key in.
func TestRefusedDrawOfferCannotUpgradeAnUnanchoredSeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "draw-upgrade-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, recoveredKey := key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "draw-upgrade-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "draw-upgrade-join"); err != nil {
		t.Fatal(err)
	}
	if _, err := chess.OfferDraw(ctx, workspace, blackKey, created.ID, "draw-upgrade-pending"); err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	refusedOffer, err := chess.OfferDraw(ctx, workspace, whiteKey, created.ID, "draw-upgrade-refused")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, recoveredKey, host.Act{Schema: "test/draw-recovery-key@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{Subject: probe.Actor, Scope: "chess"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := chess.Move(ctx, workspace, recoveredKey, created.ID, "e2e4", "draw-upgrade-recovered")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Moves != 0 || game.DrawOffer == "" || projection.RefusedTotal != 2 {
		t.Fatalf("refused draw upgraded authority: game %+v refusals %+v", game, projection.Refused)
	}
	if projection.Refused[0].Record != refusedOffer.ID || projection.Refused[1].Record != recovered.ID {
		t.Fatalf("draw upgrade refusals = %+v", projection.Refused)
	}
}

// Mutation witness for draw acceptance: matching the offering seat is not an
// effective acceptance and therefore cannot commit that seat's late upgrade.
func TestRefusedDrawAcceptanceCannotUpgradeAnUnanchoredSeat(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "accept-upgrade-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey, recoveredKey := key(t), key(t), key(t)
	witnessPublic, witnessKey := keyPair(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, whiteKey, witnessPublic, []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "", "accept-upgrade-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "", "accept-upgrade-join"); err != nil {
		t.Fatal(err)
	}
	if _, err := chess.OfferDraw(ctx, workspace, whiteKey, created.ID, "accept-upgrade-offer"); err != nil {
		t.Fatal(err)
	}
	alice := identity.Identity{Scheme: identity.GitHubScheme, Subject: "4242"}
	if _, err := identity.Endorse(ctx, workspace, witnessKey, identity.Anchor{
		Subject: created.Actor, Identity: &alice, Scope: "chess",
	}); err != nil {
		t.Fatal(err)
	}
	refusedAcceptance, err := chess.AcceptDraw(ctx, workspace, whiteKey, created.ID, "accept-upgrade-refused")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := workspace.Append(ctx, recoveredKey, host.Act{Schema: "test/accept-recovery-key@0", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Endorse(ctx, workspace, whiteKey, identity.Anchor{Subject: probe.Actor, Scope: "chess"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := chess.Move(ctx, workspace, recoveredKey, created.ID, "e2e4", "accept-upgrade-recovered")
	if err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, _ := projection.GameByID(created.ID)
	if game.Status != "playing" || game.Moves != 0 || game.DrawOffer == "" || projection.RefusedTotal != 2 {
		t.Fatalf("refused acceptance upgraded authority: game %+v refusals %+v", game, projection.Refused)
	}
	if projection.Refused[0].Record != refusedAcceptance.ID || projection.Refused[1].Record != recovered.ID {
		t.Fatalf("acceptance upgrade refusals = %+v", projection.Refused)
	}
}

func TestGamesPageIsBoundedAndStable(t *testing.T) {
	b := &logBuilder{}
	for index := range 105 {
		id := "game-" + fmt.Sprintf("%03d", index)
		b.add(id, white, chess.SchemaCreate, chess.CreatePayload{CreatorColor: "white"})
	}
	projection := b.fold()
	first, next := projection.GamesPage("", 1000)
	if len(first) != 100 || first[0].ID != "game-000" || first[99].ID != "game-099" || next != "game-099" {
		t.Fatalf("first page has %d games [%q..%q], next %q", len(first), first[0].ID, first[len(first)-1].ID, next)
	}
	second, next := projection.GamesPage(next, 100)
	if len(second) != 5 || second[0].ID != "game-100" || second[4].ID != "game-104" || next != "" {
		t.Fatalf("second page = %+v, next %q", second, next)
	}
	unknown, next := projection.GamesPage("not-a-game", 10)
	if len(unknown) != 0 || next != "" {
		t.Fatalf("unknown cursor returned %d games, next %q", len(unknown), next)
	}
}

func TestPublicActionsRunAgainstARealGitseqRepository(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "game-repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	whiteKey, blackKey := key(t), key(t)
	workspace, err := host.Init(ctx, repo, chess.Application, whiteKey, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	before, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Create(ctx, workspace, whiteKey, "white", "not-a-fingerprint", "", "bad-invite"); err == nil {
		t.Fatal("public Create accepted an impossible invited fingerprint")
	}
	after, err := workspace.Records(ctx)
	if err != nil || after.Depth != before.Depth {
		t.Fatalf("invalid create changed log depth from %d to %d: %v", before.Depth, after.Depth, err)
	}
	created, err := chess.Create(ctx, workspace, whiteKey, "white", "", "invite", "create-once")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Join(ctx, workspace, blackKey, created.ID, "invite", "join-once"); err != nil {
		t.Fatal(err)
	}
	if _, err := chess.Move(ctx, workspace, whiteKey, created.ID, "e2e4", "move-once"); err != nil {
		t.Fatal(err)
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection := chess.Fold(log)
	game, ok := projection.GameByID(created.ID)
	if !ok || game.Moves != 1 || game.LastMoveUCI != "e2e4" || len(projection.Refused) != 0 {
		t.Fatalf("real repository projection = %+v", projection)
	}

	// Reopening proves there is no reliance on process memory or a local
	// replacement of the gitseq module.
	reopened, replayed, err := chess.OpenProjection(ctx, repo)
	if err != nil || reopened == nil || replayed.Head != projection.Head {
		t.Fatalf("reopen = %v at %q: %v", reopened, replayed.Head, err)
	}
}

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private := keyPair(t)
	return private
}

func keyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func actorOf(private ed25519.PrivateKey) string {
	digest := sha256.Sum256(private.Public().(ed25519.PublicKey))
	return hex.EncodeToString(digest[:])
}
