package main

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"reflect"
	"strings"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
)

const agentTransportVersion = "chess-agent@1"
const maxAgentAction = 8 << 10

var agentOperations = []string{"list_games", "show_board", "legal_destinations", "create", "join", "move", "resign", "draw_offer", "draw_accept", "retry"}

type agentService struct {
	Genesis    string   `json:"genesis"`
	Version    string   `json:"version"`
	Operations []string `json:"operations"`
}

// Native replay is an application command, never an arbitrary host intent.
// Every field survives a retry, including the chosen predecessor and retry key.
type agentAction struct {
	Action         string `json:"action"`
	Genesis        string `json:"genesis"`
	ActorKey       []byte `json:"actor_key"`
	IdempotencyKey string `json:"idempotency_key"`
	Game           string `json:"game,omitempty"`
	Name           string `json:"name,omitempty"`
	Color          string `json:"color,omitempty"`
	InviteKey      string `json:"invite_key,omitempty"`
	Secret         string `json:"secret,omitempty"`
	Move           string `json:"move,omitempty"`
	Predecessor    string `json:"predecessor,omitempty"`
	Offer          string `json:"offer,omitempty"`
}

type agentPreparation struct {
	Version      string           `json:"version"`
	Echo         agentAction      `json:"echo"`
	Prepared     host.PreparedAct `json:"prepared"`
	SigningBytes []byte           `json:"signing_bytes"`
}

type agentSubmission struct {
	Action    agentAction `json:"action"`
	Signature []byte      `json:"signature"`
}

type agentResult struct {
	Genesis   string `json:"genesis"`
	Record    string `json:"record"`
	Effective *bool  `json:"effective"`
	Reason    string `json:"reason,omitempty"`
	Head      string `json:"head"`
	Depth     int    `json:"depth"`
}

func (a agentAction) act() (host.Act, error) {
	invalid := errors.New("invalid or unsupported native game action")
	if !validObjectName(a.Genesis) || len(a.ActorKey) != ed25519.PublicKeySize || a.IdempotencyKey == "" || len(a.IdempotencyKey) > 128 || strings.ContainsAny(a.IdempotencyKey, "\r\n\x00") || len(a.Secret) > 4096 {
		return host.Act{}, invalid
	}
	// Compare against the exact fields allowed for this operation. This also
	// rejects fields from other operations rather than silently discarding them.
	allowed := agentAction{Action: a.Action, Genesis: a.Genesis, ActorKey: a.ActorKey, IdempotencyKey: a.IdempotencyKey}
	var act host.Act
	var err error
	switch a.Action {
	case "create":
		allowed.Name, allowed.Color, allowed.InviteKey, allowed.Secret = a.Name, a.Color, a.InviteKey, a.Secret
		act, err = application.CreateNamedAct(a.Name, a.Color, a.InviteKey, a.Secret, a.IdempotencyKey)
	case "join":
		allowed.Game, allowed.Secret = a.Game, a.Secret
		if !validEventID(a.Game) {
			return host.Act{}, invalid
		}
		act, err = application.JoinAct(a.Game, a.Secret, a.IdempotencyKey)
	case "move", "resign", "draw_offer":
		allowed.Game, allowed.Predecessor = a.Game, a.Predecessor
		if !validEventID(a.Game) || !validEventID(a.Predecessor) {
			return host.Act{}, invalid
		}
		switch a.Action {
		case "move":
			allowed.Move = a.Move
			act, err = application.MoveAct(a.Game, a.Move, a.Predecessor, a.IdempotencyKey)
		case "resign":
			act, err = application.ResignAct(a.Game, a.Predecessor, a.IdempotencyKey)
		case "draw_offer":
			act, err = application.OfferDrawAct(a.Game, a.Predecessor, a.IdempotencyKey)
		}
	case "draw_accept":
		allowed.Game, allowed.Offer = a.Game, a.Offer
		if !validEventID(a.Game) || !validEventID(a.Offer) {
			return host.Act{}, invalid
		}
		act, err = application.AcceptDrawAct(a.Game, a.Offer, a.IdempotencyKey)
	default:
		return host.Act{}, invalid
	}
	if !reflect.DeepEqual(a, allowed) || len(act.Payload) > maxAgentAction {
		return host.Act{}, invalid
	}
	return act, err
}

func (s *gameActionsHTTP) registerAgent(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/service", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			http.Error(w, "service description takes no query", 400)
			return
		}
		_, p, err := s.open(r.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", 503)
			return
		}
		serveJSON(w, agentService{Genesis: canonicalAgentObject(p.Genesis), Version: agentTransportVersion, Operations: agentOperations})
	})
	mux.HandleFunc("POST /v1/actions/native/prepare", s.prepareAgent)
	mux.HandleFunc("POST /v1/actions/native/submit", s.submitAgent)
}

func (s *gameActionsHTTP) prepareAgent(w http.ResponseWriter, r *http.Request) {
	var input agentAction
	if err := decodeHTTPRequest(w, r, &input); err != nil {
		http.Error(w, "invalid native prepare form", 400)
		return
	}
	act, err := input.act()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !s.mu.TryLock() {
		http.Error(w, "game action service is busy", 429)
		return
	}
	defer s.mu.Unlock()
	view, p, err := s.open(r.Context())
	if err != nil {
		http.Error(w, "repository is unavailable", 503)
		return
	}
	if input.Genesis != canonicalAgentObject(p.Genesis) {
		http.Error(w, "repository genesis mismatch", 409)
		return
	}
	if input.Action != "create" {
		game, ok := p.GameByID(input.Game)
		if !ok {
			http.Error(w, "game does not exist", 404)
			return
		}
		if (input.Predecessor != "" && game.LastMove != input.Predecessor) || (input.Offer != "" && game.DrawOffer != input.Offer) {
			http.Error(w, "position changed; choose the action again", 409)
			return
		}
	}
	prepared, err := view.Prepare(act)
	if err != nil {
		http.Error(w, "game action could not be prepared", 400)
		return
	}
	signing, err := host.ActorSigningBytes(prepared)
	if err != nil {
		http.Error(w, "signing bytes are unavailable", 503)
		return
	}
	serveJSON(w, agentPreparation{Version: agentTransportVersion, Echo: input, Prepared: prepared, SigningBytes: signing})
}

func (s *gameActionsHTTP) submitAgent(w http.ResponseWriter, r *http.Request) {
	var input agentSubmission
	if err := decodeHTTPRequest(w, r, &input); err != nil {
		http.Error(w, "invalid native submit form", 400)
		return
	}
	act, err := input.Action.act()
	if err != nil || len(input.Signature) != ed25519.SignatureSize {
		http.Error(w, "invalid native action or signature", 400)
		return
	}
	if !s.mu.TryLock() {
		http.Error(w, "game action service is busy", 429)
		return
	}
	defer s.mu.Unlock()
	view, p, err := s.open(r.Context())
	if err != nil {
		http.Error(w, "outcome not confirmed; retry the retained action", 503)
		return
	}
	if input.Action.Genesis != canonicalAgentObject(p.Genesis) {
		http.Error(w, "repository genesis mismatch", 409)
		return
	}
	// Do not read the current move here: an accepted replay must reach host
	// idempotency even after later moves or a service restart. The fold judges
	// newly signed actions that lost a race after preparation.
	prepared, err := view.prepareReplay(act)
	if err != nil {
		http.Error(w, "native action could not be reconstructed", 400)
		return
	}
	signing, err := host.ActorSigningBytes(prepared)
	if err != nil || !ed25519.Verify(input.Action.ActorKey, signing, input.Signature) {
		http.Error(w, "native signature is invalid", 400)
		return
	}
	record, err := view.AppendSigned(r.Context(), host.SignedAct{Prepared: prepared, ActorKey: input.Action.ActorKey, ActorSignature: input.Signature})
	if err != nil {
		http.Error(w, "outcome not confirmed; retry the retained action", 503)
		return
	}
	effective, found, reason, err := application.Decision(r.Context(), view, record.ID)
	if err != nil || !found {
		http.Error(w, "decision not confirmed; retry the retained action", 503)
		return
	}
	log, err := view.Records(r.Context())
	if err != nil {
		http.Error(w, "frontier not confirmed; retry the retained action", 503)
		return
	}
	serveJSON(w, agentResult{Genesis: canonicalAgentObject(log.Genesis), Record: record.ID, Effective: &effective, Reason: reason, Head: log.Head, Depth: log.Depth})
}

// Host frontiers use bare object IDs; the transport binding uses their
// canonical object name so SHA-1 and SHA-256 repositories cannot be confused.
func canonicalAgentObject(raw string) string {
	format := "sha1"
	if len(raw) == 64 {
		format = "sha256"
	}
	value := "git:" + format + ":" + raw
	if !validObjectName(value) {
		return ""
	}
	return value
}
