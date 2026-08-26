// Package chess is the deterministic application profile for a chess log.
//
// The gitseq host proves who signed each record and where it stands. This
// package alone assigns chess meaning to those records. Fold has no network,
// clock, filesystem, or randomness: the same verified log always produces the
// same projection.
package chess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
	rules "github.com/notnil/chess"
)

const (
	SchemaCreate     = "chess/create@0"
	SchemaJoin       = "chess/join@0"
	SchemaMove       = "chess/move@0"
	SchemaResign     = "chess/resign@0"
	SchemaDrawOffer  = "chess/draw-offer@0"
	SchemaDrawAccept = "chess/draw-accept@0"
	// SchemaAnchor is host vocabulary shared by every application. Chess
	// recognizes its effects through host/identity instead of defining a
	// second, incompatible identity record.
	SchemaAnchor = identity.AnchorSchema

	FoldVersion = "chess-fold@0"
	maxPayload  = 8 << 10
	maxText     = 256
	maxRefusals = 256
	maxPage     = 100
)

// Application is the identity recorded in a chess repository's binding.
var Application = host.Application{
	Name:        "chess",
	FoldVersion: FoldVersion,
	SourceURL:   "https://github.com/generalbusiness-ai/gitseq-chess.git",
}

// Invitation limits who may take the second seat. Exactly one field may be
// set. An absent invitation deliberately leaves the seat open to the first
// valid join in log order.
type Invitation struct {
	OpponentKey string `json:"opponent_key,omitempty"`
	SecretHash  string `json:"secret_hash,omitempty"`
}

type CreatePayload struct {
	CreatorColor string      `json:"creator_color"`
	Invitation   *Invitation `json:"invitation,omitempty"`
}

type JoinPayload struct {
	Game   string `json:"game"`
	Secret string `json:"secret,omitempty"`
}

type MovePayload struct {
	Game string `json:"game"`
	Move string `json:"move"`
}

type GamePayload struct {
	Game string `json:"game"`
}

type DrawAcceptPayload struct {
	Game  string `json:"game"`
	Offer string `json:"offer"`
}

// Projection is the bounded application view at one verified log frontier.
type Projection struct {
	Genesis string    `json:"genesis"`
	Head    string    `json:"head"`
	Depth   int       `json:"depth"`
	Games   []Game    `json:"games"`
	Refused []Refusal `json:"refused,omitempty"`
	// RefusedTotal includes refusals older than the bounded Refused tail.
	RefusedTotal int            `json:"refused_total"`
	ByID         map[string]int `json:"-"`

	identities   *identity.Resolution
	lastRecordID string
}

// Game is one game's complete current state. Actor fingerprints and record
// identifiers are public log data; no private key material is retained here.
type Game struct {
	ID          string `json:"id"`
	CreatedAt   int64  `json:"created_at"`
	Creator     string `json:"creator"`
	White       string `json:"white,omitempty"`
	Black       string `json:"black,omitempty"`
	Status      string `json:"status"`
	Turn        string `json:"turn,omitempty"`
	FEN         string `json:"fen"`
	LastMove    string `json:"last_move,omitempty"`
	LastMoveUCI string `json:"last_move_uci,omitempty"`
	Moves       int    `json:"moves"`
	Outcome     string `json:"outcome,omitempty"`
	Method      string `json:"method,omitempty"`
	DrawOffer   string `json:"draw_offer,omitempty"`
	OfferedBy   string `json:"offered_by,omitempty"`

	invitation *Invitation
	join       string
	engine     *rules.Game
	whiteSeat  seat
	blackSeat  seat
	offeredBy  string
}

// seat is the authority captured when a player sits down. An unanchored seat
// belongs to its exact session key. Once that key acts under a chess-scoped
// anchor, the seat is upgraded to the persistent identity and any currently
// anchored key for that identity may act for it.
type seat struct {
	actor    string
	identity identity.Identity
	anchored bool
}

// Refusal is an application act that was recorded but had no force.
type Refusal struct {
	Record string `json:"record"`
	Game   string `json:"game,omitempty"`
	Reason string `json:"reason"`
}

// Fold interprets records in their verified order. Malformed and unauthorized
// chess acts are retained as bounded refusals; unknown schemas stay opaque.
func Fold(log host.Log) Projection {
	p := Projection{
		Genesis: log.Genesis, Head: log.Head, Depth: log.Depth,
		ByID: map[string]int{}, identities: identity.Resolve(log),
	}
	if len(log.Records) != 0 {
		p.lastRecordID = log.Records[len(log.Records)-1].ID
	}
	for _, record := range log.Records {
		switch record.Schema {
		case SchemaCreate:
			p.foldCreate(record)
		case SchemaJoin:
			p.foldJoin(record)
		case SchemaMove:
			p.foldMove(record)
		case SchemaResign:
			p.foldResign(record)
		case SchemaDrawOffer:
			p.foldDrawOffer(record)
		case SchemaDrawAccept:
			p.foldDrawAccept(record)
		}
	}
	for i := range p.Games {
		p.refresh(&p.Games[i])
	}
	return p
}

func (p *Projection) foldCreate(record host.Record) {
	var body CreatePayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	if body.CreatorColor != "white" && body.CreatorColor != "black" {
		p.refuse(record, "", "creator_color must be white or black")
		return
	}
	if len(record.RestsOn) != 0 {
		p.refuse(record, "", "create must not rest on another record")
		return
	}
	if body.Invitation != nil {
		key, secret := body.Invitation.OpponentKey, body.Invitation.SecretHash
		if (key == "") == (secret == "") {
			p.refuse(record, "", "invitation must name exactly one opponent key or secret hash")
			return
		}
		if key != "" && !validActorFingerprint(key) {
			p.refuse(record, "", "opponent key must be a lowercase SHA-256 fingerprint")
			return
		}
		if secret != "" {
			raw, err := hex.DecodeString(secret)
			if err != nil || len(raw) != sha256.Size || secret != strings.ToLower(secret) {
				p.refuse(record, "", "secret hash must be lowercase SHA-256 hex")
				return
			}
		}
	}
	game := Game{
		ID: record.ID, CreatedAt: record.Timestamp, Creator: record.Actor,
		Status: "open", engine: rules.NewGame(rules.UseNotation(rules.UCINotation{})),
	}
	if body.CreatorColor == "white" {
		game.White = record.Actor
		game.whiteSeat = p.seatAt(record, game.ID)
	} else {
		game.Black = record.Actor
		game.blackSeat = p.seatAt(record, game.ID)
	}
	if body.Invitation != nil {
		copy := *body.Invitation
		game.invitation = &copy
	}
	p.ByID[game.ID] = len(p.Games)
	p.Games = append(p.Games, game)
}

func (p *Projection) foldJoin(record host.Record) {
	var body JoinPayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	game := p.game(body.Game)
	if game == nil {
		p.refuse(record, body.Game, "game does not exist")
		return
	}
	if len(record.RestsOn) != 1 || record.RestsOn[0] != game.ID {
		p.refuse(record, body.Game, "join must rest on the game create record")
		return
	}
	if game.join != "" {
		p.refuse(record, body.Game, "opponent seat is already taken")
		return
	}
	if record.Actor == game.Creator {
		p.refuse(record, body.Game, "creator cannot take both seats")
		return
	}
	if game.invitation != nil {
		switch {
		case game.invitation.OpponentKey != "":
			if record.Actor != game.invitation.OpponentKey {
				p.refuse(record, body.Game, "joiner is not the invited key")
				return
			}
		case game.invitation.SecretHash != "":
			digest := sha256.Sum256([]byte(body.Secret))
			expected, _ := hex.DecodeString(game.invitation.SecretHash)
			if subtle.ConstantTimeCompare(digest[:], expected) != 1 {
				p.refuse(record, body.Game, "join secret does not match the invitation")
				return
			}
		}
	}
	joining := p.seatAt(record, game.ID)
	if game.White == "" {
		if sameAnchoredSeat(joining, game.blackSeat) {
			p.refuse(record, body.Game, "one persistent identity cannot hold both seats")
			return
		}
		game.White = record.Actor
		game.whiteSeat = joining
	} else {
		if sameAnchoredSeat(joining, game.whiteSeat) {
			p.refuse(record, body.Game, "one persistent identity cannot hold both seats")
			return
		}
		game.Black = record.Actor
		game.blackSeat = joining
	}
	game.join = record.ID
	game.LastMove = record.ID
	game.Status = "playing"
	p.refresh(game)
}

func (p *Projection) foldMove(record host.Record) {
	var body MovePayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	game := p.game(body.Game)
	if game == nil {
		p.refuse(record, body.Game, "game does not exist")
		return
	}
	if game.Status != "playing" {
		p.refuse(record, body.Game, "game is not in play")
		return
	}
	if len(record.RestsOn) != 1 || record.RestsOn[0] != game.LastMove {
		p.refuse(record, body.Game, "move does not continue the accepted move chain")
		return
	}
	side := "white"
	if game.engine.Position().Turn() == rules.Black {
		side = "black"
	}
	matched := p.seatSide(game, record.Actor, p.identities.LookupAt(record.ID))
	if !matched.allowed() || matched.side != side {
		p.refuse(record, body.Game, "actor does not hold the side to move")
		return
	}
	if len(body.Move) < 4 || len(body.Move) > 5 || body.Move != strings.ToLower(body.Move) {
		p.refuse(record, body.Game, "move must be lowercase UCI notation")
		return
	}
	if err := game.engine.MoveStr(body.Move); err != nil {
		p.refuse(record, body.Game, "move is illegal in the current position")
		return
	}
	matched.commit()
	// Moving instead of accepting is a refusal of the other player's offer.
	if game.DrawOffer != "" && side != game.offeredBy {
		game.DrawOffer, game.OfferedBy = "", ""
		game.offeredBy = ""
	}
	game.LastMove = record.ID
	game.LastMoveUCI = body.Move
	game.Moves++
	p.refresh(game)
}

func (p *Projection) foldResign(record host.Record) {
	var body GamePayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	game := p.game(body.Game)
	if game == nil || game.Status != "playing" {
		p.refuse(record, body.Game, "game is not in play")
		return
	}
	if len(record.RestsOn) != 1 || record.RestsOn[0] != game.LastMove {
		p.refuse(record, body.Game, "resignation does not name the current move chain")
		return
	}
	matched := p.seatSide(game, record.Actor, p.identities.LookupAt(record.ID))
	if !matched.allowed() {
		p.refuse(record, body.Game, "actor holds no seat")
		return
	}
	matched.commit()
	game.Status, game.Method = "finished", "resignation"
	if matched.side == "white" {
		game.Outcome = "black-wins"
	} else {
		game.Outcome = "white-wins"
	}
}

func (p *Projection) foldDrawOffer(record host.Record) {
	var body GamePayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	game := p.game(body.Game)
	if game == nil || game.Status != "playing" {
		p.refuse(record, body.Game, "game is not in play")
		return
	}
	if len(record.RestsOn) != 1 || record.RestsOn[0] != game.LastMove {
		p.refuse(record, body.Game, "draw offer does not name the current move chain")
		return
	}
	matched := p.seatSide(game, record.Actor, p.identities.LookupAt(record.ID))
	if !matched.allowed() {
		p.refuse(record, body.Game, "actor holds no seat")
		return
	}
	if game.DrawOffer != "" {
		p.refuse(record, body.Game, "a draw offer is already pending")
		return
	}
	matched.commit()
	game.DrawOffer, game.OfferedBy = record.ID, record.Actor
	game.offeredBy = matched.side
}

func (p *Projection) foldDrawAccept(record host.Record) {
	var body DrawAcceptPayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	game := p.game(body.Game)
	if game == nil || game.Status != "playing" {
		p.refuse(record, body.Game, "game is not in play")
		return
	}
	if body.Offer == "" || body.Offer != game.DrawOffer || len(record.RestsOn) != 1 || record.RestsOn[0] != body.Offer {
		p.refuse(record, body.Game, "draw acceptance does not answer the pending offer")
		return
	}
	matched := p.seatSide(game, record.Actor, p.identities.LookupAt(record.ID))
	if !matched.allowed() || matched.side == game.offeredBy {
		p.refuse(record, body.Game, "only the other seated player may accept the draw")
		return
	}
	matched.commit()
	game.Status, game.Outcome, game.Method = "finished", "draw", "agreement"
}

func (p *Projection) refresh(game *Game) {
	if game.engine == nil {
		return
	}
	game.FEN = game.engine.FEN()
	if game.Status == "playing" {
		game.Turn = game.engine.Position().Turn().String()
	} else {
		game.Turn = ""
	}
	if game.engine.Outcome() == rules.NoOutcome || game.Status != "playing" {
		return
	}
	game.Status = "finished"
	game.Method = game.engine.Method().String()
	switch game.engine.Outcome() {
	case rules.WhiteWon:
		game.Outcome = "white-wins"
	case rules.BlackWon:
		game.Outcome = "black-wins"
	case rules.Draw:
		game.Outcome = "draw"
	}
	game.Turn = ""
}

func (p *Projection) game(id string) *Game {
	index, ok := p.ByID[id]
	if !ok {
		return nil
	}
	return &p.Games[index]
}

func (p *Projection) refuse(record host.Record, game, reason string) {
	// Reasons are fixed application strings. Keep a bounded diagnostic tail so
	// a hostile stream of ineffective records cannot make the projection grow
	// beyond the game state it actually represents.
	p.RefusedTotal++
	if len(p.Refused) == maxRefusals {
		copy(p.Refused, p.Refused[1:])
		p.Refused = p.Refused[:maxRefusals-1]
	}
	p.Refused = append(p.Refused, Refusal{Record: record.ID, Game: game, Reason: reason})
}

func decode(payload []byte, target any) error {
	if len(payload) == 0 || len(payload) > maxPayload {
		return errors.New("payload is empty or too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("payload is not canonical JSON for this act")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("payload has trailing data")
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(canonical, payload) {
		return errors.New("payload is not canonically encoded")
	}
	return nil
}

func invalidText(value string) bool {
	return value == "" || len(value) > maxText || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00")
}

func validActorFingerprint(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && value == strings.ToLower(value)
}

type seatMatch struct {
	matched   bool
	collision bool
	upgrade   *identity.Identity
}

func (m seatMatch) allowed() bool { return m.matched && !m.collision }

func (m seatMatch) commit(owner *seat) {
	if owner == nil || m.upgrade == nil {
		return
	}
	owner.identity, owner.anchored = *m.upgrade, true
}

type sideMatch struct {
	seatMatch
	side  string
	owner *seat
}

func (m sideMatch) commit() { m.seatMatch.commit(m.owner) }

func (p *Projection) seatSide(game *Game, actor string, resolved identity.Resolved) sideMatch {
	white := matchSeat(game.whiteSeat, game.blackSeat, actor, resolved, game.ID)
	black := matchSeat(game.blackSeat, game.whiteSeat, actor, resolved, game.ID)
	if white.collision || black.collision || white.matched == black.matched {
		return sideMatch{}
	}
	if white.matched {
		return sideMatch{seatMatch: white, side: "white", owner: &game.whiteSeat}
	}
	return sideMatch{seatMatch: black, side: "black", owner: &game.blackSeat}
}

func (p *Projection) seatAt(record host.Record, game string) seat {
	owner := seat{actor: record.Actor}
	if resolved := p.identities.LookupAt(record.ID); resolved.Anchored && chessScope(resolved.Scope, game) {
		owner.identity, owner.anchored = resolved.Identity, true
	}
	return owner
}

func matchSeat(owner, other seat, actor string, resolved identity.Resolved, game string) seatMatch {
	if owner.actor == "" {
		return seatMatch{}
	}
	// An exact seated key belongs to that seat even when its current identity
	// resolution changes. It must never borrow the opposing seat merely because
	// it now resolves to that seat's persistent identity.
	if actor == other.actor {
		return seatMatch{}
	}
	if owner.anchored {
		return seatMatch{
			matched: resolved.Anchored && chessScope(resolved.Scope, game) && sameIdentity(resolved.Identity, owner.identity),
		}
	}
	if actor != owner.actor {
		return seatMatch{}
	}
	matched := seatMatch{matched: true}
	if resolved.Anchored && chessScope(resolved.Scope, game) {
		matched.collision = other.anchored && sameIdentity(resolved.Identity, other.identity)
		identity := resolved.Identity
		matched.upgrade = &identity
	}
	return matched
}

// SeatFor previews which seat a fingerprint holds in one game. It is judged
// at the last record instant, position-exact, timestamp-optimistic; the query
// is a preview, the append is the judgment; it may say yes where a later
// append refuses on expiry, never the reverse.
func (p Projection) SeatFor(gameID, fingerprint string) (side string, ok bool) {
	game, found := p.GameByID(gameID)
	if !found || p.identities == nil || p.lastRecordID == "" {
		return "", false
	}
	matched := p.seatSide(&game, fingerprint, p.identities.LookupActorAt(fingerprint, p.lastRecordID))
	if !matched.allowed() {
		return "", false
	}
	return matched.side, true
}

// SideToMove reports the fold engine's side-to-move vocabulary.
func SideToMove(game Game) string {
	if game.Status != "playing" || game.engine == nil {
		return ""
	}
	if game.engine.Position().Turn() == rules.Black {
		return "black"
	}
	return "white"
}

func chessScope(scope, game string) bool {
	return scope == "chess" || scope == "chess:"+game
}

func sameIdentity(left, right identity.Identity) bool {
	// Handle is display text and may change between otherwise equivalent
	// endorsements. The host identity contract defines sameness by provider
	// scheme and stable subject only.
	return left.Scheme == right.Scheme && left.Subject == right.Subject
}

func sameAnchoredSeat(left, right seat) bool {
	return left.anchored && right.anchored && sameIdentity(left.identity, right.identity)
}

// LegalDestinations returns the fold engine's legal destinations from one
// square. It never trusts a client-side rules implementation.
func LegalDestinations(game Game, from string) []string {
	if game.Status != "playing" || game.engine == nil || len(from) != 2 || from != strings.ToLower(from) {
		return []string{}
	}
	var result []string
	for _, move := range game.engine.ValidMoves() {
		if move.S1().String() == from {
			to := move.S2().String()
			if move.Promo() != rules.NoPieceType {
				to += move.Promo().String()
			}
			result = append(result, to)
		}
	}
	slices.Sort(result)
	return result
}

// GameByID returns a copy suitable for display plus whether it exists.
func (p Projection) GameByID(id string) (Game, bool) {
	index, ok := p.ByID[id]
	if !ok {
		return Game{}, false
	}
	return p.Games[index], true
}

// GamesPage returns a stable page in create order. Limit is clamped to the
// adapter-wide maximum; after is the last game identifier from the preceding
// page. The returned next value is empty at the end.
func (p Projection) GamesPage(after string, limit int) ([]Game, string) {
	if limit <= 0 || limit > maxPage {
		limit = maxPage
	}
	start := 0
	if after != "" {
		index, ok := p.ByID[after]
		if !ok {
			return []Game{}, ""
		}
		start = index + 1
	}
	end := min(start+limit, len(p.Games))
	page := append([]Game(nil), p.Games[start:end]...)
	next := ""
	if end < len(p.Games) && len(page) != 0 {
		next = page[len(page)-1].ID
	}
	return page, next
}

// Decision reports whether one accepted chess act changed the projection.
// Unknown and host records are not chess decisions and return found=false.
func Decision(ctx context.Context, ws *host.Workspace, record string) (effective, found bool, reason string, err error) {
	projection, err := readProjection(ctx, ws)
	if err != nil {
		return false, false, "", err
	}
	for _, refusal := range projection.Refused {
		if refusal.Record == record {
			return false, true, refusal.Reason, nil
		}
	}
	// If it is not in the bounded tail, inspect the verified log to distinguish
	// an old effective act from an old refusal without guessing.
	log, err := ws.Records(ctx)
	if err != nil {
		return false, false, "", err
	}
	for _, candidate := range log.Records {
		if candidate.ID == record {
			switch candidate.Schema {
			case SchemaCreate, SchemaJoin, SchemaMove, SchemaResign, SchemaDrawOffer, SchemaDrawAccept:
				// Re-folding the prefix bounds diagnostics and yields the exact
				// decision at the record's position.
				prefix := log
				for index := range log.Records {
					if log.Records[index].ID == record {
						prefix.Records = log.Records[:index+1]
						prefix.Depth = index + 1
						prefix.Head = ""
						break
					}
				}
				atRecord := Fold(prefix)
				for _, refused := range atRecord.Refused {
					if refused.Record == record {
						return false, true, refused.Reason, nil
					}
				}
				return true, true, "", nil
			default:
				return false, false, "", nil
			}
		}
	}
	return false, false, "", nil
}

func encode(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayload {
		return nil, errors.New("payload is too large")
	}
	return payload, nil
}

// Create records a new game. creatorColor is white or black. inviteKey and
// joinSecret are mutually exclusive; leaving both empty creates an open game.
func Create(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, creatorColor, inviteKey, joinSecret, idempotencyKey string) (host.Record, error) {
	if creatorColor != "white" && creatorColor != "black" {
		return host.Record{}, errors.New("creator color must be white or black")
	}
	if inviteKey != "" && joinSecret != "" {
		return host.Record{}, errors.New("invite key and join secret are mutually exclusive")
	}
	if inviteKey != "" && !validActorFingerprint(inviteKey) {
		return host.Record{}, errors.New("invite key must be a lowercase SHA-256 fingerprint")
	}
	body := CreatePayload{CreatorColor: creatorColor}
	if inviteKey != "" {
		body.Invitation = &Invitation{OpponentKey: inviteKey}
	}
	if joinSecret != "" {
		digest := sha256.Sum256([]byte(joinSecret))
		body.Invitation = &Invitation{SecretHash: hex.EncodeToString(digest[:])}
	}
	payload, err := encode(body)
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: SchemaCreate, Payload: payload, IdempotencyKey: idempotencyKey})
}

// Join records an attempt to take the open opponent seat.
func Join(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, secret, idempotencyKey string) (host.Record, error) {
	if invalidText(game) {
		return host.Record{}, errors.New("game is required")
	}
	payload, err := encode(JoinPayload{Game: game, Secret: secret})
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: SchemaJoin, Payload: payload, RestsOn: []string{game}, IdempotencyKey: idempotencyKey})
}

// Move records a UCI move on the current accepted move chain.
func Move(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, move, idempotencyKey string) (host.Record, error) {
	projection, err := readProjection(ctx, ws)
	if err != nil {
		return host.Record{}, err
	}
	current, ok := projection.GameByID(game)
	if !ok || current.LastMove == "" {
		return host.Record{}, errors.New("game is not ready for a move")
	}
	payload, err := encode(MovePayload{Game: game, Move: move})
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: SchemaMove, Payload: payload, RestsOn: []string{current.LastMove}, IdempotencyKey: idempotencyKey})
}

// Resign records a resignation on the current accepted move chain.
func Resign(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, idempotencyKey string) (host.Record, error) {
	return appendAtCurrentMove(ctx, ws, signer, SchemaResign, game, idempotencyKey)
}

// OfferDraw records a draw offer on the current accepted move chain.
func OfferDraw(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, idempotencyKey string) (host.Record, error) {
	return appendAtCurrentMove(ctx, ws, signer, SchemaDrawOffer, game, idempotencyKey)
}

func appendAtCurrentMove(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, schema, game, idempotencyKey string) (host.Record, error) {
	projection, err := readProjection(ctx, ws)
	if err != nil {
		return host.Record{}, err
	}
	current, ok := projection.GameByID(game)
	if !ok || current.LastMove == "" {
		return host.Record{}, errors.New("game is not in play")
	}
	payload, err := encode(GamePayload{Game: game})
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: schema, Payload: payload, RestsOn: []string{current.LastMove}, IdempotencyKey: idempotencyKey})
}

// AcceptDraw records an answer to the currently pending offer.
func AcceptDraw(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, idempotencyKey string) (host.Record, error) {
	projection, err := readProjection(ctx, ws)
	if err != nil {
		return host.Record{}, err
	}
	current, ok := projection.GameByID(game)
	if !ok || current.DrawOffer == "" {
		return host.Record{}, errors.New("game has no pending draw offer")
	}
	payload, err := encode(DrawAcceptPayload{Game: game, Offer: current.DrawOffer})
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: SchemaDrawAccept, Payload: payload, RestsOn: []string{current.DrawOffer}, IdempotencyKey: idempotencyKey})
}

// Anchor uses the shared host identity vocabulary. Its force is determined by
// host/identity; an unanchored endorser cannot mint an identity by assertion.
func Anchor(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, anchor identity.Anchor) (host.Record, error) {
	return identity.Endorse(ctx, ws, signer, anchor)
}

func readProjection(ctx context.Context, ws *host.Workspace) (Projection, error) {
	if ws == nil {
		return Projection{}, errors.New("workspace is required")
	}
	log, err := ws.Records(ctx)
	if err != nil {
		return Projection{}, err
	}
	return Fold(log), nil
}

// OpenProjection opens and folds a repository at one verified frontier.
func OpenProjection(ctx context.Context, repo string) (*host.Workspace, Projection, error) {
	ws, err := host.Open(ctx, repo, Application)
	if err != nil {
		return nil, Projection{}, err
	}
	projection, err := readProjection(ctx, ws)
	return ws, projection, err
}

func (g Game) String() string {
	return fmt.Sprintf("%s %s %s", g.ID, g.Status, g.FEN)
}
