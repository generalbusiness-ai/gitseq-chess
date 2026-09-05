package main

import (
	"context"
	"crypto/ed25519"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host/live"
)

//go:embed ui/*
var embeddedUI embed.FS

var webTemplates = template.Must(template.ParseFS(embeddedUI, "ui/*.html"))

func newReadHandler(ctx context.Context, repo string) http.Handler {
	runtime, err := newChessLive()
	if err != nil {
		return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "live runtime is unavailable", http.StatusServiceUnavailable)
		}))
	}
	return newReadHandlerWithLive(ctx, repo, runtime)
}

type projectionReader func(context.Context) (application.Projection, error)

type chessLive struct {
	hub       *live.Hub
	mu        sync.Mutex
	waitSlots chan struct{}
	seatFor   func(application.Projection, string, string) (string, bool)
	// Chat is the sole retained conversation for each game. Motion hints live
	// only in leased presence and disappear on renewal, revocation, or expiry.
	chat map[string]liveChatProjection
}

type liveChatProjection struct {
	conversation string
	messages     []liveChatMessage
}

func newChessLive() (*chessLive, error) {
	hub, err := live.New(512)
	if err != nil {
		return nil, err
	}
	return &chessLive{
		hub: hub, waitSlots: make(chan struct{}, maxLiveWaiters),
		chat: make(map[string]liveChatProjection),
		seatFor: func(projection application.Projection, game, actor string) (string, bool) {
			return projection.SeatFor(game, actor)
		},
	}, nil
}

func newReadHandlerWithLive(ctx context.Context, repo string, runtime *chessLive) http.Handler {
	return newReadHandlerWithIdentity(ctx, repo, runtime, identityHTTPConfig{})
}

func newReadHandlerWithIdentity(_ context.Context, repo string, runtime *chessLive, identityConfig identityHTTPConfig, owned ...*chessRepository) http.Handler {
	mux := http.NewServeMux()
	open := localRepositoryOpener(repo)
	if len(owned) > 0 {
		open = owned[0].openView
	}
	read := projectionReader(func(requestContext context.Context) (application.Projection, error) {
		_, projection, err := open(requestContext)
		return projection, err
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(w, request)
			return
		}
		if _, err := boundedQuery(request, nil); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, err := read(request.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		games, _ := projection.GamesPage("", 100)
		serveHTML(w, "lobby.html", lobbyView{
			Head: projection.Head, Games: games, RefusedTotal: projection.RefusedTotal,
		})
	})
	mux.HandleFunc("GET /game", func(w http.ResponseWriter, request *http.Request) {
		query, err := boundedQuery(request, map[string]queryRule{"game": {required: true, eventID: true}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, err := read(request.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		game, ok := projection.GameByID(query.Get("game"))
		if !ok {
			http.Error(w, "game does not exist", http.StatusNotFound)
			return
		}
		squares, err := boardSquares(game.FEN)
		if err != nil {
			http.Error(w, "folded board is unavailable", http.StatusServiceUnavailable)
			return
		}
		refused := make([]application.Refusal, 0)
		for _, refusal := range projection.Refused {
			if refusal.Game == game.ID {
				refused = append(refused, refusal)
			}
		}
		serveHTML(w, "game.html", gameView{Head: projection.Head, Game: game, Squares: squares, Refused: refused})
	})
	mux.HandleFunc("GET /assets/app.css", serveEmbedded("ui/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /assets/app.js", serveEmbedded("ui/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /v1/games", func(w http.ResponseWriter, request *http.Request) {
		query, err := boundedQuery(request, map[string]queryRule{
			"after": {eventID: true},
			"limit": {maxBytes: 3},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		limit := 100
		if stated := query.Get("limit"); stated != "" {
			parsed, parseErr := strconv.Atoi(stated)
			if parseErr != nil || parsed < 1 || parsed > 100 || strconv.Itoa(parsed) != stated {
				http.Error(w, "limit must be between 1 and 100", http.StatusBadRequest)
				return
			}
			limit = parsed
		}
		projection, err := read(request.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		games, next := projection.GamesPage(query.Get("after"), limit)
		serveJSON(w, map[string]any{"games": games, "next": next, "head": projection.Head})
	})
	mux.HandleFunc("GET /v1/board", func(w http.ResponseWriter, request *http.Request) {
		query, err := boundedQuery(request, map[string]queryRule{"game": {required: true, eventID: true}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, err := read(request.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		game, ok := projection.GameByID(query.Get("game"))
		if !ok {
			http.Error(w, "game does not exist", http.StatusNotFound)
			return
		}
		squares, err := boardSquares(game.FEN)
		if err != nil {
			http.Error(w, "folded board is unavailable", 503)
			return
		}
		refusals := []application.Refusal{}
		for _, refusal := range projection.Refused {
			if refusal.Game == game.ID {
				refusals = append(refusals, refusal)
			}
		}
		serveJSON(w, map[string]any{"game": game, "squares": squares, "refusals": refusals, "head": projection.Head})
	})
	mux.HandleFunc("GET /v1/legal", func(w http.ResponseWriter, request *http.Request) {
		query, err := boundedQuery(request, map[string]queryRule{
			"game": {required: true, eventID: true},
			"from": {required: true, square: true},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, err := read(request.Context())
		if err != nil {
			http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
			return
		}
		game, ok := projection.GameByID(query.Get("game"))
		if !ok {
			http.Error(w, "game does not exist", http.StatusNotFound)
			return
		}
		selection := application.LegalSelection(game, query.Get("from"))
		serveJSON(w, map[string]any{"destinations": selection.Destinations, "reason": selection.Reason, "head": projection.Head})
	})
	runtime.register(mux, read)
	identities := newIdentityHTTP(repo, identityConfig)
	identities.open = open
	identities.register(mux)
	actions := newGameActionsHTTP(repo)
	actions.open = open
	actions.register(mux)
	return securityHeaders(rejectLiveQueries(trustedLiveHost(mux)))
}

// trustedLiveHost applies the same browser-facing mutation guard as the core
// resident service. It keeps a listener opened more broadly by mistake from
// becoming a DNS-rebinding path into process-local credentials and state.
func trustedLiveHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && browserMutationPath(request.URL.Path) {
			if !loopbackRequestHost(request.Host) {
				http.Error(w, "mutation host must resolve only to loopback", http.StatusBadRequest)
				return
			}
			if err := guardLiveMutation(request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, request)
	})
}

func browserMutationPath(path string) bool {
	return strings.HasPrefix(path, "/v1/live/") || strings.HasPrefix(path, "/v1/identity/") || strings.HasPrefix(path, "/v1/actions/")
}

func loopbackRequestHost(value string) bool {
	host, _, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	addresses, err := net.LookupIP(host)
	if err != nil || len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if !address.IsLoopback() {
			return false
		}
	}
	return true
}

func guardLiveMutation(request *http.Request) error {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return errors.New("mutations require application/json")
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil || parsed.Scheme != "http" || parsed.Host != request.Host || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("cross-origin mutation refused")
		}
	}
	if site := request.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return errors.New("cross-site mutation refused")
	}
	return nil
}

func rejectLiveQueries(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v1/actions/") || strings.HasPrefix(request.URL.Path, "/v1/live/") ||
			(strings.HasPrefix(request.URL.Path, "/v1/identity/") && request.URL.Path != "/v1/identity/github/callback") {
			if _, err := boundedQuery(request, nil); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, request)
	})
}

const (
	maxLiveRequestBytes = 32 << 10
	maxLiveFrames       = 100
	maxLiveWaiters      = 32
	maxDisplayNameRunes = 64
	liveWriteTimeout    = live.MaxCompositeWait + 5*time.Second
)

var (
	errGameIdentifier  = errors.New("game identifier is invalid")
	errGameMissing     = errors.New("game does not exist")
	errRepoUnavailable = errors.New("repository is unavailable")
)

type livePresence struct {
	Game        string      `json:"game"`
	Actor       string      `json:"actor"`
	Role        string      `json:"role"`
	DisplayName string      `json:"display_name,omitempty"`
	Motion      *liveMotion `json:"motion,omitempty"`
}

type liveSessionPrepareRequest struct {
	Game        string  `json:"game"`
	ActorKey    []byte  `json:"actor_key"`
	DisplayName *string `json:"display_name,omitempty"`
}

type liveSessionOpenRequest struct {
	Challenge live.SessionChallenge `json:"challenge"`
	Signature []byte                `json:"signature"`
}

type liveSessionRenewRequest struct {
	Credential  string  `json:"credential"`
	Game        string  `json:"game"`
	ActorKey    []byte  `json:"actor_key"`
	DisplayName *string `json:"display_name,omitempty"`
}

type liveSessionRevokeRequest struct {
	Credential string `json:"credential"`
}

type liveMotionRequest struct {
	Credential  string  `json:"credential"`
	Game        string  `json:"game"`
	ActorKey    []byte  `json:"actor_key"`
	DisplayName *string `json:"display_name,omitempty"`
	Phase       string  `json:"phase"`
	From        string  `json:"from"`
	To          string  `json:"to,omitempty"`
}

type liveMotion struct {
	Phase string `json:"phase"`
	Head  string `json:"head"`
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
	Role  string `json:"role"`
}

type liveChatPrepareRequest struct {
	Credential string `json:"credential"`
	Game       string `json:"game"`
	ActorKey   []byte `json:"actor_key"`
	Text       string `json:"text"`
}

type liveSubmitRequest struct {
	Credential string          `json:"credential"`
	Game       string          `json:"game"`
	Submission live.Submission `json:"submission"`
}

type liveObserveRequest struct {
	Credential string               `json:"credential,omitempty"`
	Game       string               `json:"game"`
	Cursor     live.CompositeCursor `json:"cursor"`
	WaitMS     int                  `json:"wait_ms"`
}

type liveParticipantView struct {
	Handle      string `json:"handle"`
	Actor       string `json:"actor"`
	Role        string `json:"role"`
	DisplayName string `json:"display_name,omitempty"`
}

type liveMotionView struct {
	ID          string `json:"id"`
	Actor       string `json:"actor"`
	DisplayName string `json:"display_name,omitempty"`
	liveMotion
}

type liveChatView struct {
	ID          string `json:"id"`
	Actor       string `json:"actor"`
	DisplayName string `json:"display_name,omitempty"`
	Text        string `json:"text"`
}

type liveChatMessage struct {
	ID    string
	Actor string
	Text  string
}

type liveObserveResponse struct {
	Changed      bool                  `json:"changed"`
	Reset        bool                  `json:"reset"`
	Game         application.Game      `json:"game"`
	Cursor       live.CompositeCursor  `json:"cursor"`
	Participants []liveParticipantView `json:"participants"`
	Motions      []liveMotionView      `json:"motions"`
	Chat         []liveChatView        `json:"chat"`
	NexusKey     []byte                `json:"nexus_key"`
}

func (runtime *chessLive) register(mux *http.ServeMux, read projectionReader) {
	mux.HandleFunc("POST /v1/live/session/prepare", func(w http.ResponseWriter, request *http.Request) {
		var input liveSessionPrepareRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		public, err := browserPublicKey(input.ActorKey)
		if err != nil || !validEventID(input.Game) {
			http.Error(w, "game and actor_key are required", http.StatusBadRequest)
			return
		}
		displayName, err := normalizeDisplayName(input.DisplayName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, game, err := readGame(request.Context(), read, input.Game)
		if err != nil {
			serveGameReadError(w, err)
			return
		}
		fingerprint, _ := live.ActorFingerprint(public)
		role := runtime.role(projection, game.ID, fingerprint)
		// A prepared challenge carries only watcher authority. Player authority is
		// recomputed from the current fold after proof succeeds, so a challenge
		// cannot preserve a role across a durable seat change.
		presence := livePresence{Game: game.ID, Actor: fingerprint, Role: "watcher", DisplayName: displayName}
		value, _ := json.Marshal(presence)
		challenge, err := runtime.hub.PrepareSession(sessionActor(game.ID, fingerprint), public, string(value), live.DefaultSessionTTL, live.ActivityUpdate{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		signingBytes, err := live.SessionSigningBytes(challenge)
		if err != nil {
			http.Error(w, "session challenge is unavailable", http.StatusServiceUnavailable)
			return
		}
		serveJSON(w, map[string]any{
			"challenge": challenge, "signing_bytes": signingBytes, "role": role,
			"head": projection.Head,
		})
	})
	mux.HandleFunc("POST /v1/live/session/open", func(w http.ResponseWriter, request *http.Request) {
		var input liveSessionOpenRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		runtime.mu.Lock()
		credential, change, err := runtime.hub.OpenSession(input.Challenge, input.Signature)
		if err != nil {
			runtime.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var presence livePresence
		if err := json.Unmarshal([]byte(change.Value), &presence); err != nil {
			_, _ = runtime.hub.RevokeSession(credential)
			runtime.mu.Unlock()
			http.Error(w, "session scope is unavailable", http.StatusServiceUnavailable)
			return
		}
		projection, game, readErr := readGame(request.Context(), read, presence.Game)
		if readErr != nil {
			_, _ = runtime.hub.RevokeSession(credential)
			runtime.mu.Unlock()
			serveGameReadError(w, readErr)
			return
		}
		presence.Role = runtime.role(projection, game.ID, presence.Actor)
		change, err = runtime.renewPresence(credential, input.Challenge.ActorKey, presence)
		if err != nil {
			_, _ = runtime.hub.RevokeSession(credential)
			runtime.mu.Unlock()
			http.Error(w, "session role is unavailable", http.StatusServiceUnavailable)
			return
		}
		runtime.mu.Unlock()
		serveJSON(w, map[string]any{
			"credential": credential, "handle": change.ID, "actor": presence.Actor,
			"role": presence.Role, "cursor": change.Cursor,
		})
	})
	mux.HandleFunc("POST /v1/live/session/renew", func(w http.ResponseWriter, request *http.Request) {
		var input liveSessionRenewRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		public, err := browserPublicKey(input.ActorKey)
		if err != nil || !validEventID(input.Game) {
			http.Error(w, "game and actor_key are required", http.StatusBadRequest)
			return
		}
		displayName, err := normalizeDisplayName(input.DisplayName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, game, err := readGame(request.Context(), read, input.Game)
		if err != nil {
			serveGameReadError(w, err)
			return
		}
		fingerprint, _ := live.ActorFingerprint(public)
		if !runtime.sessionMatches(input.Credential, game.ID, fingerprint) {
			http.Error(w, "credential is not valid", http.StatusBadRequest)
			return
		}
		presence := livePresence{
			Game: game.ID, Actor: fingerprint, Role: runtime.role(projection, game.ID, fingerprint), DisplayName: displayName,
		}
		change, err := runtime.renewPresence(input.Credential, public, presence)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serveJSON(w, map[string]any{"cursor": change.Cursor, "role": presence.Role, "head": projection.Head})
	})
	mux.HandleFunc("POST /v1/live/session/revoke", func(w http.ResponseWriter, request *http.Request) {
		var input liveSessionRevokeRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := runtime.hub.RevokeSession(input.Credential); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serveJSON(w, map[string]bool{"revoked": true})
	})
	mux.HandleFunc("POST /v1/live/motion", func(w http.ResponseWriter, request *http.Request) {
		var input liveMotionRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		public, err := browserPublicKey(input.ActorKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		displayName, err := normalizeDisplayName(input.DisplayName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		projection, game, role, err := runtime.validateMotion(request.Context(), read, input.Credential, input.Game, input.Phase, input.From, input.To, public)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		presence := livePresence{
			Game: game.ID, Actor: liveFingerprint(public), Role: role, DisplayName: displayName,
			Motion: &liveMotion{Phase: input.Phase, Head: projection.Head, From: input.From, To: input.To, Role: role},
		}
		change, err := runtime.renewPresence(input.Credential, public, presence)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serveJSON(w, map[string]any{"cursor": change.Cursor, "role": role, "head": projection.Head})
	})
	mux.HandleFunc("POST /v1/live/chat/prepare", func(w http.ResponseWriter, request *http.Request) {
		var input liveChatPrepareRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		public, err := browserPublicKey(input.ActorKey)
		if err != nil || !validEventID(input.Game) {
			http.Error(w, "game and actor_key are required", http.StatusBadRequest)
			return
		}
		if _, _, err := readGame(request.Context(), read, input.Game); err != nil {
			serveGameReadError(w, err)
			return
		}
		fingerprint, _ := live.ActorFingerprint(public)
		if !runtime.sessionMatches(input.Credential, input.Game, fingerprint) {
			http.Error(w, "credential is not valid", http.StatusBadRequest)
			return
		}
		draft, err := runtime.prepareMessage(input.Credential, input.Game, live.Message{About: chatScope(input.Game), Text: input.Text}, public)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		signingBytes, _ := live.ActorSigningBytes(draft)
		serveJSON(w, map[string]any{"draft": draft, "signing_bytes": signingBytes})
	})
	mux.HandleFunc("POST /v1/live/chat/submit", func(w http.ResponseWriter, request *http.Request) {
		var input liveSubmitRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		public, err := browserPublicKey(input.Submission.ActorKey)
		if err != nil || !validEventID(input.Game) || input.Submission.Draft.Scope != chatScope(input.Game) {
			http.Error(w, "signed chat does not match the game", http.StatusBadRequest)
			return
		}
		fingerprint, _ := live.ActorFingerprint(public)
		if !runtime.sessionMatches(input.Credential, input.Game, fingerprint) {
			http.Error(w, "credential is not valid", http.StatusBadRequest)
			return
		}
		var message live.Message
		if err := decodeCanonicalLivePayload(input.Submission.Draft.Payload, &message); err != nil || message.About != chatScope(input.Game) || message.Text == "" {
			http.Error(w, "signed chat does not match the game", http.StatusBadRequest)
			return
		}
		runtime.mu.Lock()
		frame, err := runtime.hub.SubmitMessageForSession(input.Credential, input.Submission)
		if err != nil {
			runtime.mu.Unlock()
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		runtime.rememberChatLocked(input.Game, frame, message)
		runtime.mu.Unlock()
		serveJSON(w, map[string]any{"conversation": frame.Conversation, "sequence": frame.Sequence})
	})
	mux.HandleFunc("POST /v1/live/observe", func(w http.ResponseWriter, request *http.Request) {
		var input liveObserveRequest
		if err := decodeHTTPRequest(w, request, &input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !validEventID(input.Game) || input.WaitMS < 1 || input.WaitMS > int(live.MaxCompositeWait/time.Millisecond) {
			http.Error(w, "game and wait_ms between 1 and 30000 are required", http.StatusBadRequest)
			return
		}
		if input.Credential != "" && !runtime.sessionHasGame(input.Credential, input.Game) {
			http.Error(w, "credential is not valid", http.StatusBadRequest)
			return
		}
		select {
		case runtime.waitSlots <- struct{}{}:
			defer func() { <-runtime.waitSlots }()
		default:
			http.Error(w, "live wait capacity is exhausted", http.StatusTooManyRequests)
			return
		}
		_, _, err := live.WaitComposite(request.Context(), runtime.hub, input.Credential, input.Cursor, time.Duration(input.WaitMS)*time.Millisecond,
			func(waitContext context.Context) (application.Game, live.DurableFrontier, error) {
				projection, game, err := readGame(waitContext, read, input.Game)
				return game, live.DurableFrontier{Genesis: projection.Genesis, Head: projection.Head, Depth: projection.Depth}, err
			})
		if err != nil {
			serveGameReadError(w, err)
			return
		}
		// WaitComposite is only the unlocked wake-up. The application gate then
		// captures the Hub cursor, durable fold, and chat projection as one view.
		// Chat submission uses the same gate for its Hub commit and projection.
		runtime.mu.Lock()
		observation, err := runtime.hub.Observe(input.Credential, &input.Cursor.Live)
		projection, game, readErr := readGame(request.Context(), read, input.Game)
		if err == nil && readErr == nil {
			runtime.reconcileChatLocked(input.Game, observation.Snapshot)
		}
		participants, motions, chat := runtime.projectLiveLocked(input.Game, projection.Head, observation.Snapshot)
		runtime.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if readErr != nil {
			serveGameReadError(w, readErr)
			return
		}
		frontier := live.DurableFrontier{Genesis: projection.Genesis, Head: projection.Head, Depth: projection.Depth}
		cursor := live.CompositeCursor{Durable: frontier, Live: observation.Snapshot.Cursor}
		changed := observation.Reset || len(observation.Changes) != 0 || len(observation.Inbox.Frames) != 0 || frontier != input.Cursor.Durable
		serveJSON(w, liveObserveResponse{
			Changed: changed, Reset: observation.Reset, Game: game,
			Cursor: cursor, Participants: participants, Motions: motions, Chat: chat,
			NexusKey: runtime.hub.PublicKey(),
		})
	})
}

func decodeHTTPRequest(w http.ResponseWriter, request *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxLiveRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body has trailing data")
	}
	return nil
}

func browserPublicKey(raw []byte) (ed25519.PublicKey, error) {
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("actor_key must be an Ed25519 public key")
	}
	return ed25519.PublicKey(append([]byte(nil), raw...)), nil
}

func normalizeDisplayName(value *string) (string, error) {
	if value == nil {
		return "", nil
	}
	for _, character := range *value {
		if unicode.IsControl(character) {
			return "", errors.New("display_name must not contain control characters")
		}
	}
	name := strings.TrimSpace(*value)
	if name == "" {
		return "", errors.New("display_name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxDisplayNameRunes {
		return "", fmt.Errorf("display_name must be at most %d characters", maxDisplayNameRunes)
	}
	return name, nil
}

func decodeCanonicalLivePayload(payload []byte, target any) error {
	if err := strictJSON(payload, target); err != nil {
		return err
	}
	canonical, err := json.Marshal(target)
	if err != nil || !slices.Equal(canonical, payload) {
		return errors.New("live payload is not canonical")
	}
	return nil
}

func readGame(ctx context.Context, read projectionReader, gameID string) (application.Projection, application.Game, error) {
	if !validEventID(gameID) {
		return application.Projection{}, application.Game{}, errGameIdentifier
	}
	projection, err := read(ctx)
	if err != nil {
		return application.Projection{}, application.Game{}, errRepoUnavailable
	}
	game, ok := projection.GameByID(gameID)
	if !ok {
		return projection, application.Game{}, errGameMissing
	}
	return projection, game, nil
}

func serveGameReadError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errGameMissing) {
		status = http.StatusNotFound
	} else if errors.Is(err, errRepoUnavailable) {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), status)
}

func gameScope(game string) string { return "chess:game:" + game }

func chatScope(game string) string { return "chess:chat:" + game }

func sessionActor(game, fingerprint string) string { return gameScope(game) + "|" + fingerprint }

func (runtime *chessLive) sessionMatches(credential, game, fingerprint string) bool {
	actor, ok := runtime.hub.SessionActor(credential)
	return ok && actor == sessionActor(game, fingerprint)
}

func (runtime *chessLive) sessionHasGame(credential, game string) bool {
	actor, ok := runtime.hub.SessionActor(credential)
	return ok && strings.HasPrefix(actor, gameScope(game)+"|")
}

func (runtime *chessLive) role(projection application.Projection, game, fingerprint string) string {
	if side, ok := runtime.seatFor(projection, game, fingerprint); ok {
		return side
	}
	return "watcher"
}

func liveFingerprint(public ed25519.PublicKey) string {
	fingerprint, _ := live.ActorFingerprint(public)
	return fingerprint
}

func (runtime *chessLive) renewPresence(credential string, public ed25519.PublicKey, presence livePresence) (live.Change, error) {
	value, err := json.Marshal(presence)
	if err != nil || len(value) > live.MaxPresenceValueBytes {
		return live.Change{}, errors.New("presence value is invalid")
	}
	return runtime.hub.RenewSession(
		credential, sessionActor(presence.Game, presence.Actor), public,
		string(value), live.DefaultSessionTTL, live.ActivityUpdate{},
	)
}

func (runtime *chessLive) validateMotion(ctx context.Context, read projectionReader, credential, gameID, phase, from, to string, public ed25519.PublicKey) (application.Projection, application.Game, string, error) {
	projection, game, err := readGame(ctx, read, gameID)
	if err != nil {
		return projection, game, "", err
	}
	fingerprint := liveFingerprint(public)
	if !runtime.sessionMatches(credential, game.ID, fingerprint) {
		return projection, game, "", errors.New("credential is not valid")
	}
	role := runtime.role(projection, game.ID, fingerprint)
	if role != application.SideToMove(game) {
		return projection, game, role, errors.New("session does not hold the side to move")
	}
	destinations := application.LegalDestinations(game, from)
	if !validSquare(from) || len(destinations) == 0 {
		return projection, game, role, errors.New("motion is not legal in the folded position")
	}
	switch phase {
	case "dragged":
		if to != "" {
			return projection, game, role, errors.New("dragged motion must not claim a destination")
		}
	case "submitting":
		if (len(to) != 2 && len(to) != 3) || !slices.Contains(destinations, to) {
			return projection, game, role, errors.New("motion is not legal in the folded position")
		}
	default:
		return projection, game, role, errors.New("motion phase must be dragged or submitting")
	}
	return projection, game, role, nil
}

func (runtime *chessLive) chatConversation(game string) string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.chat[game].conversation
}

func (runtime *chessLive) rememberChatLocked(game string, frame live.Frame, message live.Message) {
	projection := runtime.chat[game]
	if projection.conversation != "" && projection.conversation != frame.Conversation {
		projection = liveChatProjection{}
	}
	projection.conversation = frame.Conversation
	actor := liveFingerprint(frame.ActorKey)
	projection.messages = append(projection.messages, liveChatMessage{
		ID: frame.Conversation + ":" + strconv.FormatUint(frame.Sequence, 10), Actor: actor, Text: message.Text,
	})
	if len(projection.messages) > maxLiveFrames {
		projection.messages = slices.Clone(projection.messages[len(projection.messages)-maxLiveFrames:])
	}
	runtime.chat[game] = projection
}

func (runtime *chessLive) reconcileChatLocked(game string, snapshot live.Snapshot) {
	projection, ok := runtime.chat[game]
	if !ok || slices.Contains(snapshot.Conversations, projection.conversation) {
		return
	}
	delete(runtime.chat, game)
}

func (runtime *chessLive) prepareMessage(credential, game string, message live.Message, public ed25519.PublicKey) (live.Draft, error) {
	return runtime.hub.PrepareMessageForSession(credential, runtime.chatConversation(game), message, public)
}

func (runtime *chessLive) projectLiveLocked(game, head string, snapshot live.Snapshot) ([]liveParticipantView, []liveMotionView, []liveChatView) {
	participants := make([]liveParticipantView, 0)
	motions := make([]liveMotionView, 0)
	for handle, value := range snapshot.Presence {
		var presence livePresence
		if json.Unmarshal([]byte(value), &presence) == nil && presence.Game == game {
			displayName := presence.DisplayName
			if normalized, err := normalizeDisplayName(&displayName); err != nil || normalized != displayName {
				displayName = ""
			}
			participants = append(participants, liveParticipantView{
				Handle: handle, Actor: presence.Actor, Role: presence.Role, DisplayName: displayName,
			})
			if presence.Motion != nil && presence.Motion.Head == head {
				motion := *presence.Motion
				motions = append(motions, liveMotionView{
					ID:    handle + ":" + motion.Head + ":" + motion.Phase + ":" + motion.From + ":" + motion.To,
					Actor: presence.Actor, DisplayName: displayName, liveMotion: motion,
				})
			}
		}
	}
	slices.SortFunc(participants, func(left, right liveParticipantView) int { return strings.Compare(left.Handle, right.Handle) })
	slices.SortFunc(motions, func(left, right liveMotionView) int { return strings.Compare(left.ID, right.ID) })
	displayNames := make(map[string]string, len(participants))
	for _, participant := range participants {
		if _, exists := displayNames[participant.Actor]; !exists && participant.DisplayName != "" {
			displayNames[participant.Actor] = participant.DisplayName
		}
	}
	// A chat frame retains only its verified actor fingerprint. Its self-asserted
	// name is joined from current presence so the label expires with the lease.
	messages := runtime.chat[game].messages
	chat := make([]liveChatView, len(messages))
	for index, message := range messages {
		chat[index] = liveChatView{
			ID: message.ID, Actor: message.Actor, DisplayName: displayNames[message.Actor], Text: message.Text,
		}
	}
	return participants, motions, chat
}

type queryRule struct {
	required bool
	eventID  bool
	square   bool
	maxBytes int
}

// boundedQuery is the shared HTTP boundary. It rejects malformed encoding,
// duplicate and unknown fields, oversized input, and values whose shape does
// not belong to the projection query they address.
func boundedQuery(request *http.Request, rules map[string]queryRule) (url.Values, error) {
	if len(request.URL.RawQuery) > 2048 {
		return nil, errors.New("query is too large")
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return nil, errors.New("query is malformed")
	}
	for name, entries := range values {
		rule, ok := rules[name]
		if !ok {
			return nil, fmt.Errorf("unknown query field %q", name)
		}
		if len(entries) != 1 {
			return nil, fmt.Errorf("query field %q must appear once", name)
		}
		value := entries[0]
		limit := rule.maxBytes
		if limit == 0 {
			limit = 192
		}
		if len(value) > limit {
			return nil, fmt.Errorf("query field %q is too large", name)
		}
		if rule.eventID && value != "" && !validEventID(value) {
			return nil, fmt.Errorf("query field %q must be a canonical event identifier", name)
		}
		if rule.square && !validSquare(value) {
			return nil, fmt.Errorf("query field %q must be a lowercase board square", name)
		}
	}
	for name, rule := range rules {
		if rule.required && values.Get(name) == "" {
			return nil, fmt.Errorf("query field %q is required", name)
		}
	}
	return values, nil
}

func validEventID(value string) bool {
	parts := strings.Split(value, "#")
	if len(parts) != 2 || !validObjectName(parts[0]) || !validObjectName(parts[1]) {
		return false
	}
	return strings.Split(parts[0], ":")[1] == strings.Split(parts[1], ":")[1]
}

func validObjectName(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "git" {
		return false
	}
	want := 0
	switch parts[1] {
	case "sha1":
		want = 40
	case "sha256":
		want = 64
	default:
		return false
	}
	if len(parts[2]) != want {
		return false
	}
	for _, character := range parts[2] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func validSquare(value string) bool {
	return len(value) == 2 && value[0] >= 'a' && value[0] <= 'h' && value[1] >= '1' && value[1] <= '8'
}

type lobbyView struct {
	Head         string
	Games        []application.Game
	RefusedTotal int
}

type gameView struct {
	Head    string
	Game    application.Game
	Squares []boardSquare
	Refused []application.Refusal
}

type boardSquare struct {
	Name  string
	Piece string
	Label string
	Dark  bool
}

func boardSquares(fen string) ([]boardSquare, error) {
	fields := strings.Fields(fen)
	if len(fields) == 0 {
		return nil, errors.New("FEN has no board")
	}
	ranks := strings.Split(fields[0], "/")
	if len(ranks) != 8 {
		return nil, errors.New("FEN must have eight ranks")
	}
	pieces := map[byte]struct {
		glyph string
		name  string
	}{
		'K': {"♔", "white king"}, 'Q': {"♕", "white queen"}, 'R': {"♖", "white rook"},
		'B': {"♗", "white bishop"}, 'N': {"♘", "white knight"}, 'P': {"♙", "white pawn"},
		'k': {"♚", "black king"}, 'q': {"♛", "black queen"}, 'r': {"♜", "black rook"},
		'b': {"♝", "black bishop"}, 'n': {"♞", "black knight"}, 'p': {"♟", "black pawn"},
	}
	result := make([]boardSquare, 0, 64)
	for rankIndex, encoded := range ranks {
		file := 0
		for index := 0; index < len(encoded); index++ {
			character := encoded[index]
			if character >= '1' && character <= '8' {
				count := int(character - '0')
				if file+count > 8 {
					return nil, errors.New("FEN rank has the wrong width")
				}
				for range count {
					result = append(result, makeBoardSquare(file, 8-rankIndex, "", "empty"))
					file++
				}
				continue
			}
			piece, ok := pieces[character]
			if !ok || file >= 8 {
				return nil, errors.New("FEN contains an invalid piece")
			}
			result = append(result, makeBoardSquare(file, 8-rankIndex, piece.glyph, piece.name))
			file++
		}
		if file != 8 {
			return nil, errors.New("FEN rank has the wrong width")
		}
	}
	return result, nil
}

func makeBoardSquare(file, rank int, piece, label string) boardSquare {
	name := string(rune('a'+file)) + strconv.Itoa(rank)
	return boardSquare{Name: name, Piece: piece, Label: label + " at " + name, Dark: (file+rank)%2 == 1}
}

func serveHTML(w http.ResponseWriter, name string, value any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = webTemplates.ExecuteTemplate(w, name, value)
}

func serveEmbedded(name, contentType string) http.HandlerFunc {
	content, err := embeddedUI.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, request *http.Request) {
		if _, err := boundedQuery(request, nil); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(content)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, request)
	})
}

func serveJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
