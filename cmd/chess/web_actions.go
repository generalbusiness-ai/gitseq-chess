package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
)

const gameDraftTTL = 5 * time.Minute
const maxGameDrafts = 128

type gameActionEcho struct {
	Action      string `json:"action"`
	Game        string `json:"game"`
	Move        string `json:"move"`
	Secret      string `json:"secret"`
	Predecessor string `json:"predecessor"`
	Actor       string `json:"actor"`
}

type gameActionRequest struct {
	Action      string `json:"action"`
	Game        string `json:"game"`
	Move        string `json:"move"`
	Secret      string `json:"secret"`
	Predecessor string `json:"predecessor"`
	ActorKey    []byte `json:"actor_key"`
}

type gameActionDraft struct {
	prepared  host.PreparedAct
	actor     string
	expires   time.Time
	record    *host.Record
	signature []byte
}

type gameActionsHTTP struct {
	repo   string
	open   repositoryOpener
	mu     sync.Mutex
	now    func() time.Time
	drafts map[string]*gameActionDraft
}

func newGameActionsHTTP(repo string) *gameActionsHTTP {
	return &gameActionsHTTP{repo: repo, open: localRepositoryOpener(repo), now: time.Now, drafts: make(map[string]*gameActionDraft)}
}

func (s *gameActionsHTTP) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/actions/prepare", s.prepare)
	mux.HandleFunc("POST /v1/actions/submit", s.submit)
}

func (s *gameActionsHTTP) prune() {
	for id, draft := range s.drafts {
		if !s.now().Before(draft.expires) {
			delete(s.drafts, id)
		}
	}
}

func (s *gameActionsHTTP) prepare(w http.ResponseWriter, r *http.Request) {
	var input gameActionRequest
	if err := decodeHTTPRequest(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil || !validEventID(input.Game) {
		http.Error(w, "actor key and game are required", 400)
		return
	}
	if (input.Action != "join" && input.Action != "move") ||
		(input.Action == "join" && (input.Move != "" || input.Predecessor != "")) ||
		(input.Action == "move" && (input.Secret != "" || !validEventID(input.Predecessor))) {
		http.Error(w, "invalid game action", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(s.drafts) >= maxGameDrafts {
		http.Error(w, "game action service is busy", 429)
		return
	}
	workspace, projection, err := s.open(r.Context())
	if err != nil {
		http.Error(w, "repository is unavailable", 503)
		return
	}
	current, ok := projection.GameByID(input.Game)
	if !ok {
		http.Error(w, "game does not exist", 404)
		return
	}
	if input.Action == "move" && current.LastMove != input.Predecessor {
		http.Error(w, "position changed; choose the move again", 409)
		return
	}
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		http.Error(w, "draft is unavailable", 503)
		return
	}
	id := hex.EncodeToString(token)
	var act host.Act
	if input.Action == "join" {
		act, err = application.JoinAct(input.Game, input.Secret, "browser-game:"+id)
	} else {
		act, err = application.MoveAct(input.Game, input.Move, input.Predecessor, "browser-game:"+id)
	}
	if err != nil || len(act.Payload) > 8<<10 {
		http.Error(w, "invalid or oversized game action", 400)
		return
	}
	prepared, err := workspace.Prepare(act)
	if err != nil {
		http.Error(w, "game action could not be prepared", 400)
		return
	}
	signingBytes, err := host.ActorSigningBytes(prepared)
	if err != nil {
		http.Error(w, "signing bytes are unavailable", 503)
		return
	}
	actor := liveFingerprint(public)
	expires := s.now().Add(gameDraftTTL)
	s.drafts[id] = &gameActionDraft{prepared: prepared, actor: actor, expires: expires}
	serveJSON(w, map[string]any{"draft": id, "signing_bytes": signingBytes, "expires": expires.Unix(), "head": projection.Head,
		"echo": gameActionEcho{Action: input.Action, Game: input.Game, Move: input.Move, Secret: input.Secret, Predecessor: input.Predecessor, Actor: actor}})
}

func (s *gameActionsHTTP) submit(w http.ResponseWriter, r *http.Request) {
	var input identitySubmitRequest
	if err := decodeHTTPRequest(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil || len(input.ActorSignature) != ed25519.SignatureSize {
		http.Error(w, "actor key and signature are required", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	draft, ok := s.drafts[input.Draft]
	if !ok || draft.actor != liveFingerprint(public) {
		http.Error(w, "draft is unknown or expired; refresh and choose a new action", 400)
		return
	}
	signingBytes, err := host.ActorSigningBytes(draft.prepared)
	if err != nil || !ed25519.Verify(public, signingBytes, input.ActorSignature) {
		http.Error(w, "game action signature is invalid", 400)
		return
	}
	if draft.record != nil && !bytes.Equal(draft.signature, input.ActorSignature) {
		http.Error(w, "retry must carry the original signature", 400)
		return
	}
	workspace, _, err := s.open(r.Context())
	if err != nil {
		http.Error(w, "repository is unavailable; retry the same signed draft", 503)
		return
	}
	if draft.record == nil {
		record, err := workspace.AppendSigned(r.Context(), host.SignedAct{Prepared: draft.prepared, ActorKey: public, ActorSignature: input.ActorSignature})
		if err != nil {
			http.Error(w, "append was not confirmed; retry the same signed draft", 503)
			return
		}
		draft.record = &record
		draft.signature = append([]byte(nil), input.ActorSignature...)
	}
	result, err := actionResult(r.Context(), workspace, *draft.record)
	if err != nil {
		http.Error(w, "record was appended but its decision is unavailable; retry the same signed draft", 503)
		return
	}
	log, err := workspace.Records(r.Context())
	if err != nil {
		http.Error(w, "record was appended but its frontier is unavailable; retry the same signed draft", 503)
		return
	}
	result["head"] = log.Head
	result["depth"] = log.Depth
	serveJSON(w, result)
}
