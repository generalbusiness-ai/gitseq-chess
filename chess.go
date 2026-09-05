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
	SchemaCreate      = "chess/create@0"
	SchemaCreateNamed = "chess/create-named@0"
	SchemaName        = "chess/name@0"
	SchemaJoin        = "chess/join@0"
	SchemaMove        = "chess/move@0"
	SchemaResign      = "chess/resign@0"
	SchemaDrawOffer   = "chess/draw-offer@0"
	SchemaDrawAccept  = "chess/draw-accept@0"
	// SchemaAnchor is host vocabulary shared by every application. Chess
	// recognizes its effects through host/identity instead of defining a
	// second, incompatible identity record.
	SchemaAnchor = identity.AnchorSchema

	FoldVersion = "chess-fold@2"
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

type CreateNamedPayload struct {
	CreatorColor string      `json:"creator_color"`
	Invitation   *Invitation `json:"invitation,omitempty"`
	Name         string      `json:"name"`
}

type NamePayload struct {
	Game string `json:"game"`
	Name string `json:"name"`
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
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	AdmissionOpen bool   `json:"admission_open"`
	CreatedAt     int64  `json:"created_at"`
	Creator       string `json:"creator"`
	White         string `json:"white,omitempty"`
	Black         string `json:"black,omitempty"`
	Status        string `json:"status"`
	Turn          string `json:"turn,omitempty"`
	FEN           string `json:"fen"`
	LastMove      string `json:"last_move,omitempty"`
	LastMoveUCI   string `json:"last_move_uci,omitempty"`
	Moves         int    `json:"moves"`
	Outcome       string `json:"outcome,omitempty"`
	Method        string `json:"method,omitempty"`
	DrawOffer     string `json:"draw_offer,omitempty"`
	OfferedBy     string `json:"offered_by,omitempty"`

	invitation *Invitation
	join       string
	engine     *rules.Game
	whiteSeat  seat
	blackSeat  seat
	offeredBy  string
}

// seat is the authority captured when a player sits down. An unanchored seat
// belongs to its exact session key. Once that key acts under a chess-scoped
// anchor meeting the seat strength threshold, the seat is upgraded to the
// persistent identity and any currently qualified key for that identity may
// act for it.
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

// IdentityMutation reports the durable record and the authority change the
// verified host identity fold found after it was appended. Outcome is created,
// revoked, refused, or unknown. Unknown means the record is durable but the
// post-append verified frontier could not be read or did not contain it.
type IdentityMutation struct {
	Record  string `json:"record"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// StandingAnchor is one endorsement that still answers for its subject at the
// verified frontier. Identity and strength come from identity.Resolve, never
// from claims in the anchor payload.
type StandingAnchor struct {
	Record       string            `json:"record"`
	Subject      string            `json:"subject"`
	Scope        string            `json:"scope"`
	Identity     identity.Identity `json:"identity"`
	Vouching     string            `json:"vouching"`
	Verification string            `json:"verification"`
	NotAfter     int64             `json:"not_after,omitempty"`
}

// AnchorPage is a bounded read-only view of standing identity endorsements.
type AnchorPage struct {
	Anchors   []StandingAnchor `json:"anchors"`
	Head      string           `json:"head"`
	Truncated bool             `json:"truncated,omitempty"`
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
		case SchemaCreateNamed:
			p.foldCreateNamed(record)
		case SchemaName:
			p.foldName(record)
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
	p.foldNewGame(record, body.CreatorColor, body.Invitation, "")
}

func (p *Projection) foldCreateNamed(record host.Record) {
	var body CreateNamedPayload
	if err := decode(record.Payload, &body); err != nil {
		p.refuse(record, "", err.Error())
		return
	}
	if invalidText(body.Name) {
		p.refuse(record, "", "name must be one line of at most 256 bytes")
		return
	}
	p.foldNewGame(record, body.CreatorColor, body.Invitation, body.Name)
}

func (p *Projection) foldNewGame(record host.Record, creatorColor string, invitation *Invitation, name string) {
	if creatorColor != "white" && creatorColor != "black" {
		p.refuse(record, "", "creator_color must be white or black")
		return
	}
	if len(record.RestsOn) != 0 {
		p.refuse(record, "", "create must not rest on another record")
		return
	}
	if invitation != nil {
		key, secret := invitation.OpponentKey, invitation.SecretHash
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
		AdmissionOpen: invitation == nil,
		Name:          name,
		Status:        "open", engine: rules.NewGame(rules.UseNotation(rules.UCINotation{})),
	}
	if creatorColor == "white" {
		game.White = record.Actor
		game.whiteSeat = p.seatAt(record, game.ID)
	} else {
		game.Black = record.Actor
		game.blackSeat = p.seatAt(record, game.ID)
	}
	if invitation != nil {
		copy := *invitation
		game.invitation = &copy
	}
	p.ByID[game.ID] = len(p.Games)
	p.Games = append(p.Games, game)
}

func (p *Projection) foldName(record host.Record) {
	var body NamePayload
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
		p.refuse(record, body.Game, "name must rest on the game create record")
		return
	}
	if record.Actor != game.Creator {
		p.refuse(record, body.Game, "only the creator may name the game")
		return
	}
	if invalidText(body.Name) {
		p.refuse(record, body.Game, "name must be one line of at most 256 bytes")
		return
	}
	if game.Name != "" {
		p.refuse(record, body.Game, "game already has a name")
		return
	}
	game.Name = body.Name
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
	side := SideToMove(*game)
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
		reason := "move is illegal in the current position"
		reason += ": " + invalidMoveDetail(*game, body.Move)
		p.refuse(record, body.Game, reason)
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
	if resolved := p.identities.LookupAt(record.ID); seatAnchorQualifies(resolved, game) {
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
			matched: seatAnchorQualifies(resolved, game) && sameIdentity(resolved.Identity, owner.identity),
		}
	}
	if actor != owner.actor {
		return seatMatch{}
	}
	matched := seatMatch{matched: true}
	if seatAnchorQualifies(resolved, game) {
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

// seatAnchorQualifies is the chess seat-strength policy. Every anchor rung the
// host defines today qualifies, including the weakest witnessed/live-lookup
// pair, but an unanchored resolution, the wrong scope, or any unreviewed
// strength value confers no seat authority.
func seatAnchorQualifies(resolved identity.Resolved, game string) bool {
	if !resolved.Anchored || !chessScope(resolved.Scope, game) {
		return false
	}
	switch resolved.Vouching {
	case identity.Witnessed, identity.SelfSigned:
	default:
		return false
	}
	switch resolved.Verification {
	case identity.LiveLookup, identity.InLog:
	default:
		return false
	}
	return true
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
	return LegalSelection(game, from).Destinations
}

// LegalSelection reports legal destinations and, when none exist, the reason
// the rules engine can establish from the current position.
type LegalSelectionResult struct {
	Destinations []string
	Reason       string
}

func LegalSelection(game Game, from string) LegalSelectionResult {
	if game.Status != "playing" || game.engine == nil {
		return LegalSelectionResult{Destinations: []string{}, Reason: "the game is not in play"}
	}
	if len(from) != 2 || from != strings.ToLower(from) || from[0] < 'a' || from[0] > 'h' || from[1] < '1' || from[1] > '8' {
		return LegalSelectionResult{Destinations: []string{}, Reason: "the source square is invalid"}
	}
	square := rules.NewSquare(rules.File(from[0]-'a'), rules.Rank(from[1]-'1'))
	piece := game.engine.Position().Board().Piece(square)
	if piece == rules.NoPiece {
		return LegalSelectionResult{Destinations: []string{}, Reason: "the square is empty"}
	}
	turn := game.engine.Position().Turn()
	if piece.Color() != turn {
		return LegalSelectionResult{
			Destinations: []string{},
			Reason:       fmt.Sprintf("the square holds a %s piece, but %s is to move", colorName(piece.Color()), colorName(turn)),
		}
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
	if len(result) == 0 {
		return LegalSelectionResult{
			Destinations: []string{},
			Reason:       "the piece is blocked, pinned, or moving it would leave the king in check",
		}
	}
	return LegalSelectionResult{Destinations: result}
}

func invalidMoveDetail(game Game, move string) string {
	selection := LegalSelection(game, move[:2])
	if selection.Reason != "" {
		return selection.Reason
	}
	if len(move) == 4 {
		for _, destination := range selection.Destinations {
			if len(destination) == 3 && strings.HasPrefix(destination, move[2:4]) {
				return fmt.Sprintf("a promotion piece is required; use %sq, %sr, %sb, or %sn", move, move, move, move)
			}
		}
	}
	return fmt.Sprintf("destination %s is not valid from %s", move[2:], move[:2])
}

func colorName(color rules.Color) string {
	if color == rules.Black {
		return "black"
	}
	return "white"
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
			case SchemaCreate, SchemaCreateNamed, SchemaName, SchemaJoin, SchemaMove, SchemaResign, SchemaDrawOffer, SchemaDrawAccept:
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
	body, err := createPayload(creatorColor, inviteKey, joinSecret)
	if err != nil {
		return host.Record{}, err
	}
	payload, err := encode(body)
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{Schema: SchemaCreate, Payload: payload, IdempotencyKey: idempotencyKey})
}

func createPayload(creatorColor, inviteKey, joinSecret string) (CreatePayload, error) {
	if creatorColor != "white" && creatorColor != "black" {
		return CreatePayload{}, errors.New("creator color must be white or black")
	}
	if inviteKey != "" && joinSecret != "" {
		return CreatePayload{}, errors.New("invite key and join secret are mutually exclusive")
	}
	if inviteKey != "" && !validActorFingerprint(inviteKey) {
		return CreatePayload{}, errors.New("invite key must be a lowercase SHA-256 fingerprint")
	}
	body := CreatePayload{CreatorColor: creatorColor}
	if inviteKey != "" {
		body.Invitation = &Invitation{OpponentKey: inviteKey}
	}
	if joinSecret != "" {
		digest := sha256.Sum256([]byte(joinSecret))
		body.Invitation = &Invitation{SecretHash: hex.EncodeToString(digest[:])}
	}
	return body, nil
}

// CreateNamed records a named game in one act. Keeping the combined vocabulary
// separate preserves the exact bytes and judgments of create@0 and name@0.
func CreateNamed(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, name, creatorColor, inviteKey, joinSecret, idempotencyKey string) (host.Record, error) {
	if name == "" {
		return Create(ctx, ws, signer, creatorColor, inviteKey, joinSecret, idempotencyKey)
	}
	if invalidText(name) {
		return host.Record{}, errors.New("name must be one line of at most 256 bytes")
	}
	create, err := createPayload(creatorColor, inviteKey, joinSecret)
	if err != nil {
		return host.Record{}, err
	}
	payload, err := encode(CreateNamedPayload{
		CreatorColor: create.CreatorColor,
		Invitation:   create.Invitation,
		Name:         name,
	})
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, host.Act{
		Schema: SchemaCreateNamed, Payload: payload, IdempotencyKey: idempotencyKey,
	})
}

// Join records an attempt to take the open opponent seat.
func Join(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, game, secret, idempotencyKey string) (host.Record, error) {
	act, err := JoinAct(game, secret, idempotencyKey)
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, act)
}

// JoinAct builds the same signed application act for local and browser custody.
// Preparing it reserves no seat; the fold decides admission when sequenced.
func JoinAct(game, secret, idempotencyKey string) (host.Act, error) {
	if invalidText(game) {
		return host.Act{}, errors.New("game is required")
	}
	payload, err := encode(JoinPayload{Game: game, Secret: secret})
	if err != nil {
		return host.Act{}, err
	}
	return host.Act{Schema: SchemaJoin, Payload: payload, RestsOn: []string{game}, IdempotencyKey: idempotencyKey}, nil
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
	act, err := MoveAct(game, move, current.LastMove, idempotencyKey)
	if err != nil {
		return host.Record{}, err
	}
	return ws.Append(ctx, signer, act)
}

// MoveAct binds a UCI move to the explicit accepted predecessor supplied by a
// verified projection. It does not decide legality or refresh a stale intent.
func MoveAct(game, move, predecessor, idempotencyKey string) (host.Act, error) {
	if invalidText(game) || invalidText(predecessor) {
		return host.Act{}, errors.New("game and predecessor are required")
	}
	payload, err := encode(MovePayload{Game: game, Move: move})
	if err != nil {
		return host.Act{}, err
	}
	return host.Act{Schema: SchemaMove, Payload: payload, RestsOn: []string{predecessor}, IdempotencyKey: idempotencyKey}, nil
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

// RevokeAnchor records an attempt to withdraw one identity endorsement. The
// host identity fold, not the append boundary, decides whether it has force.
func RevokeAnchor(ctx context.Context, ws *host.Workspace, signer ed25519.PrivateKey, anchorRecord string) (host.Record, error) {
	return identity.Revoke(ctx, ws, signer, anchorRecord)
}

// IdentityOutcome reads the verified host state after an identity mutation and
// reports what that exact record accomplished. It deliberately does not widen
// Decision: host identity records remain outside chess judgment. A read failure
// returns unknown with the durable record identifier rather than either losing
// the identifier or assuming that an append created authority.
func IdentityOutcome(ctx context.Context, ws *host.Workspace, record host.Record) IdentityMutation {
	result := IdentityMutation{Record: record.ID, Outcome: "unknown"}
	if ws == nil {
		result.Reason = "record was durably appended, but its identity outcome could not be read: workspace is required"
		return result
	}
	log, err := ws.Records(ctx)
	if err != nil {
		result.Reason = fmt.Sprintf("record was durably appended, but its identity outcome could not be read: %v", err)
		return result
	}
	index := -1
	for candidate := range log.Records {
		if log.Records[candidate].ID == record.ID {
			index = candidate
			record = log.Records[candidate]
			break
		}
	}
	if index < 0 {
		result.Reason = "record was durably appended, but it is absent from the verified frontier"
		return result
	}
	resolved := identity.Resolve(log)
	switch record.Schema {
	case identity.AnchorSchema:
		var anchor identity.Anchor
		if err := json.Unmarshal(record.Payload, &anchor); err != nil {
			result.Reason = "the durable anchor could not be interpreted"
			return result
		}
		standing := resolved.LookupActorAt(anchor.Subject, record.ID)
		if standing.Anchored && standing.Record == record.ID {
			result.Outcome = "created"
			return result
		}
		result.Outcome = "refused"
		result.Reason = "the endorsement did not create recovery authority"
		return result
	case identity.RevokeSchema:
		var revocation identity.Revocation
		if err := json.Unmarshal(record.Payload, &revocation); err != nil {
			result.Reason = "the durable revocation could not be interpreted"
			return result
		}
		// Compare the actual fold with the same verified position treated as an
		// opaque record. Keeping its position and timestamp in the counterfactual
		// prevents an expiry boundary from being mistaken for a successful
		// withdrawal. Comparing every endorsed subject also catches withdrawal
		// of credentials inherited from the named anchor.
		withoutRevocation := log
		withoutRevocation.Records = append([]host.Record(nil), log.Records...)
		withoutRevocation.Records[index].Schema = "chess/identity-outcome-probe@0"
		without := identity.Resolve(withoutRevocation)
		subjects := make(map[string]bool)
		for _, candidate := range log.Records {
			if candidate.Schema != identity.AnchorSchema {
				continue
			}
			var anchor identity.Anchor
			if json.Unmarshal(candidate.Payload, &anchor) == nil && anchor.Subject != "" {
				subjects[anchor.Subject] = true
			}
		}
		for subject := range subjects {
			before := without.LookupActorAt(subject, record.ID)
			after := resolved.LookupActorAt(subject, record.ID)
			if before != after {
				result.Outcome = "revoked"
				return result
			}
		}
		result.Outcome = "refused"
		result.Reason = "the revocation did not withdraw standing recovery authority"
		return result
	default:
		result.Reason = "the durable record is not an identity mutation"
		return result
	}
}

// ListAnchors returns at most limit standing endorsements matching subject or
// scope. Candidates come only from verified host records, while force,
// identity, and strength come from identity.Resolve at the verified frontier.
func ListAnchors(ctx context.Context, ws *host.Workspace, subject, scope string, limit int) (AnchorPage, error) {
	if ws == nil {
		return AnchorPage{}, errors.New("workspace is required")
	}
	if subject == "" && scope == "" {
		return AnchorPage{}, errors.New("subject or scope is required")
	}
	if subject != "" && invalidIdentityFilter(subject) {
		return AnchorPage{}, errors.New("subject must be one line of at most 128 bytes")
	}
	if scope != "" && invalidIdentityFilter(scope) {
		return AnchorPage{}, errors.New("scope must be one line of at most 128 bytes")
	}
	if limit < 1 || limit > maxPage {
		return AnchorPage{}, fmt.Errorf("limit must be between 1 and %d", maxPage)
	}
	log, err := ws.Records(ctx)
	if err != nil {
		return AnchorPage{}, err
	}
	page := AnchorPage{Anchors: []StandingAnchor{}, Head: log.Head}
	if len(log.Records) == 0 {
		return page, nil
	}
	resolution := identity.Resolve(log)
	frontier := log.Records[len(log.Records)-1].ID
	seen := make(map[string]bool)
	for _, record := range log.Records {
		if record.Schema != identity.AnchorSchema {
			continue
		}
		var anchor identity.Anchor
		if json.Unmarshal(record.Payload, &anchor) != nil {
			continue
		}
		if subject != "" && anchor.Subject != subject || scope != "" && anchor.Scope != scope {
			continue
		}
		standing := resolution.LookupActorAt(anchor.Subject, frontier)
		if !standing.Anchored || standing.Record != record.ID || seen[record.ID] {
			continue
		}
		seen[record.ID] = true
		if len(page.Anchors) == limit {
			page.Truncated = true
			break
		}
		page.Anchors = append(page.Anchors, StandingAnchor{
			Record: record.ID, Subject: anchor.Subject, Scope: standing.Scope,
			Identity: standing.Identity, Vouching: standing.Vouching.String(),
			Verification: standing.Verification.String(), NotAfter: anchor.NotAfter,
		})
	}
	return page, nil
}

func invalidIdentityFilter(value string) bool {
	return len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00")
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
