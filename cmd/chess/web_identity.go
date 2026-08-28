package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

const (
	identityStateTTL     = 10 * time.Minute
	identityDraftTTL     = 5 * time.Minute
	identityMaxExpiry    = 90 * 24 * time.Hour
	identityMaxPending   = 128
	identityProviderBody = 32 << 10
)

type identityHTTPConfig struct {
	GitHubAuthorizeURL string
	GitHubTokenURL     string
	GitHubUserURL      string
	GitHubRedirectURL  string
	GitHubClientID     string
	GitHubClientSecret string
	WitnessKey         ed25519.PrivateKey
	Client             *http.Client
	Now                func() time.Time
	Random             io.Reader
}

type identityHTTP struct {
	repo   string
	config identityHTTPConfig
	mu     sync.Mutex
	states map[string]githubIdentityState
	drafts map[string]identityDraft
}

type githubIdentityState struct {
	Verifier string
	Actor    string
	Subject  string
	Scope    string
	NotAfter int64
	Expires  time.Time
}

type identityDraft struct {
	Prepared host.PreparedAct
	Actor    string
	Subject  string
	Expires  time.Time
}

type identityActorRequest struct {
	ActorKey []byte `json:"actor_key"`
}

type identityAnchorRequest struct {
	ActorKey []byte               `json:"actor_key"`
	Subject  string               `json:"subject"`
	Scope    string               `json:"scope"`
	NotAfter int64                `json:"not_after"`
	Nostr    *identity.NostrProof `json:"nostr,omitempty"`
}

type identitySubmitRequest struct {
	Draft          string `json:"draft"`
	ActorKey       []byte `json:"actor_key"`
	ActorSignature []byte `json:"actor_signature"`
}

func newIdentityHTTP(repo string, config identityHTTPConfig) *identityHTTP {
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 10 * time.Second}
	} else {
		client := *config.Client
		config.Client = &client
	}
	if config.Client.Timeout <= 0 {
		config.Client.Timeout = 10 * time.Second
	}
	config.Client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("provider redirects are refused")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &identityHTTP{
		repo: repo, config: config,
		states: make(map[string]githubIdentityState), drafts: make(map[string]identityDraft),
	}
}

func identityHTTPConfigFromEnvironment(ctx context.Context, repo string) (identityHTTPConfig, error) {
	config := identityHTTPConfig{
		GitHubAuthorizeURL: os.Getenv("GITSEQ_CHESS_GITHUB_AUTHORIZE_URL"),
		GitHubTokenURL:     os.Getenv("GITSEQ_CHESS_GITHUB_TOKEN_URL"),
		GitHubUserURL:      os.Getenv("GITSEQ_CHESS_GITHUB_USER_URL"),
		GitHubRedirectURL:  os.Getenv("GITSEQ_CHESS_GITHUB_REDIRECT_URL"),
		GitHubClientID:     os.Getenv("GITSEQ_CHESS_GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITSEQ_CHESS_GITHUB_CLIENT_SECRET"),
	}
	witnessPath := os.Getenv("GITSEQ_CHESS_IDENTITY_WITNESS_KEY")
	configured := config.GitHubRedirectURL != "" || config.GitHubClientID != "" ||
		config.GitHubClientSecret != "" || witnessPath != "" || config.GitHubAuthorizeURL != "" ||
		config.GitHubTokenURL != "" || config.GitHubUserURL != ""
	if !configured {
		return config, nil
	}
	if config.GitHubAuthorizeURL == "" {
		config.GitHubAuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if config.GitHubTokenURL == "" {
		config.GitHubTokenURL = "https://github.com/login/oauth/access_token"
	}
	if config.GitHubUserURL == "" {
		config.GitHubUserURL = "https://api.github.com/user"
	}
	if config.GitHubRedirectURL == "" || config.GitHubClientID == "" || config.GitHubClientSecret == "" || witnessPath == "" {
		return identityHTTPConfig{}, errors.New("GitHub identity configuration is incomplete")
	}
	store, err := openKeyStore(ctx, witnessPath, repo, true)
	if err != nil {
		return identityHTTPConfig{}, errors.New("GitHub witness key is unavailable")
	}
	defer store.Close()
	config.WitnessKey, err = readKey(store)
	if err != nil {
		return identityHTTPConfig{}, errors.New("GitHub witness key is unavailable")
	}
	return config, nil
}

func (service *identityHTTP) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/identity/status", service.status)
	mux.HandleFunc("POST /v1/identity/github/start", service.githubStart)
	mux.HandleFunc("GET /v1/identity/github/callback", service.githubCallback)
	mux.HandleFunc("POST /v1/identity/nostr/template", service.nostrTemplate)
	mux.HandleFunc("POST /v1/identity/endorsement/prepare", service.prepareEndorsement)
	mux.HandleFunc("POST /v1/identity/endorsement/submit", service.submitEndorsement)
}

func (service *identityHTTP) status(w http.ResponseWriter, request *http.Request) {
	var input identityActorRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	view, err := currentIdentityView(request.Context(), workspace, liveFingerprint(public))
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	serveJSON(w, view)
}

func (service *identityHTTP) githubStart(w http.ResponseWriter, request *http.Request) {
	var input identityAnchorRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := liveFingerprint(public)
	if input.Subject != "" && input.Subject != actor {
		http.Error(w, "subject must match actor_key", http.StatusBadRequest)
		return
	}
	if err := service.validateGrant(input.Scope, input.NotAfter); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil || service.githubPreflight(request.Context(), workspace) != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	authorize, redirect, err := service.githubEndpoints()
	if err != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	state, err := service.randomToken(32)
	if err != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	verifier, err := service.randomToken(48)
	if err != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !service.storeState(state, githubIdentityState{
		Verifier: verifier, Actor: actor, Subject: actor, Scope: input.Scope,
		NotAfter: input.NotAfter, Expires: service.config.Now().Add(identityStateTTL),
	}) {
		http.Error(w, "identity service is busy", http.StatusTooManyRequests)
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := authorize.Query()
	query.Set("client_id", service.config.GitHubClientID)
	query.Set("redirect_uri", redirect.String())
	query.Set("scope", "read:user")
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	query.Set("code_challenge_method", "S256")
	authorize.RawQuery = query.Encode()
	serveJSON(w, map[string]string{"authorize_url": authorize.String()})
}

func (service *identityHTTP) githubCallback(w http.ResponseWriter, request *http.Request) {
	query, err := boundedQuery(request, map[string]queryRule{
		"code": {required: true, maxBytes: 512}, "state": {required: true, maxBytes: 128},
	})
	if err != nil || service.guardGitHubCallback(request) != nil {
		http.Error(w, "GitHub callback is invalid", http.StatusBadRequest)
		return
	}
	attempt, ok := service.takeState(query.Get("state"))
	if !ok {
		http.Error(w, "GitHub callback is invalid", http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil || service.githubPreflight(request.Context(), workspace) != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	providerIdentity, err := service.githubIdentity(request.Context(), query.Get("code"), attempt.Verifier)
	if err != nil {
		http.Error(w, "GitHub identity could not be verified", http.StatusBadGateway)
		return
	}
	record, err := identity.Endorse(request.Context(), workspace, service.config.WitnessKey, identity.Anchor{
		Subject: attempt.Subject, Identity: &providerIdentity, Scope: attempt.Scope, NotAfter: attempt.NotAfter,
	})
	if err != nil {
		http.Error(w, "GitHub identity could not be recorded", http.StatusServiceUnavailable)
		return
	}
	outcome := application.IdentityOutcome(request.Context(), workspace, record)
	if outcome.Outcome != "created" {
		http.Error(w, "GitHub identity did not create recovery authority", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>GitHub identity complete</title><script src="/assets/app.js" defer></script></head><body data-view="oauth-callback" data-status="complete"><main><h1>Identity connected</h1><p>You may return to the chess game.</p></main></body></html>`)
}

func (service *identityHTTP) nostrTemplate(w http.ResponseWriter, request *http.Request) {
	var input identityAnchorRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := liveFingerprint(public)
	if input.Subject != "" && input.Subject != actor {
		http.Error(w, "subject must match actor_key", http.StatusBadRequest)
		return
	}
	if err := service.validateGrant(input.Scope, input.NotAfter); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	log, err := workspace.Records(request.Context())
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	content, err := identity.NostrDelegation(identity.Anchor{
		Genesis: log.Genesis, Subject: actor, Scope: input.Scope, NotAfter: input.NotAfter,
	})
	if err != nil {
		http.Error(w, "Nostr identity template is unavailable", http.StatusBadRequest)
		return
	}
	serveJSON(w, map[string]any{
		"actor": actor,
		"event": map[string]any{"kind": identity.NostrProofKind, "created_at": service.config.Now().Unix(), "tags": [][]string{}, "content": content},
	})
}

func (service *identityHTTP) prepareEndorsement(w http.ResponseWriter, request *http.Request) {
	var input identityAnchorRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := liveFingerprint(public)
	if !host.ValidActorFingerprint(input.Subject) {
		http.Error(w, "subject must be an actor fingerprint", http.StatusBadRequest)
		return
	}
	if err := service.validateGrant(input.Scope, input.NotAfter); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	if input.Nostr != nil {
		if input.Subject != actor {
			http.Error(w, "Nostr subject must match actor_key", http.StatusBadRequest)
			return
		}
	} else if err := service.validateDelegation(request.Context(), workspace, actor, input.Scope, input.NotAfter); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	prepared, err := identity.PrepareEndorsement(request.Context(), workspace, identity.Anchor{
		Subject: input.Subject, Scope: input.Scope, NotAfter: input.NotAfter, Nostr: input.Nostr,
	}, "")
	if err != nil {
		http.Error(w, "identity endorsement is invalid", http.StatusBadRequest)
		return
	}
	signingBytes, err := host.ActorSigningBytes(prepared)
	if err != nil {
		http.Error(w, "identity endorsement is unavailable", http.StatusServiceUnavailable)
		return
	}
	draft, err := service.randomToken(24)
	if err != nil {
		http.Error(w, "identity endorsement is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !service.storeDraft(draft, identityDraft{
		Prepared: prepared, Actor: actor, Subject: input.Subject, Expires: service.config.Now().Add(identityDraftTTL),
	}) {
		http.Error(w, "identity service is busy", http.StatusTooManyRequests)
		return
	}
	serveJSON(w, map[string]any{"draft": draft, "signing_bytes": signingBytes, "actor": actor})
}

func (service *identityHTTP) submitEndorsement(w http.ResponseWriter, request *http.Request) {
	var input identitySubmitRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil || len(input.ActorSignature) != ed25519.SignatureSize {
		http.Error(w, "actor_key and actor_signature are required", http.StatusBadRequest)
		return
	}
	draft, ok := service.takeDraft(input.Draft)
	if !ok || draft.Actor != liveFingerprint(public) {
		http.Error(w, "identity draft is invalid", http.StatusBadRequest)
		return
	}
	workspace, _, err := application.OpenProjection(request.Context(), service.repo)
	if err != nil {
		http.Error(w, "repository is unavailable", http.StatusServiceUnavailable)
		return
	}
	record, err := workspace.AppendSigned(request.Context(), host.SignedAct{
		Prepared: draft.Prepared, ActorKey: public, ActorSignature: input.ActorSignature,
	})
	if err != nil {
		http.Error(w, "identity signature is invalid", http.StatusBadRequest)
		return
	}
	outcome := application.IdentityOutcome(request.Context(), workspace, record)
	view, viewErr := currentIdentityView(request.Context(), workspace, draft.Subject)
	if viewErr != nil {
		http.Error(w, "identity record was appended but its outcome is unavailable", http.StatusServiceUnavailable)
		return
	}
	view["outcome"] = outcome.Outcome
	view["effective"] = outcome.Outcome == "created"
	if outcome.Reason != "" {
		view["reason"] = outcome.Reason
	}
	serveJSON(w, view)
}

func (service *identityHTTP) validateGrant(scope string, notAfter int64) error {
	if scope != "chess" {
		if !strings.HasPrefix(scope, "chess:") || !validEventID(strings.TrimPrefix(scope, "chess:")) {
			return errors.New("scope must be chess or chess:<game>")
		}
	}
	now := service.config.Now()
	if notAfter <= now.Unix() || notAfter > now.Add(identityMaxExpiry).Unix() {
		return errors.New("not_after must be in the next 90 days")
	}
	return nil
}

func (service *identityHTTP) validateDelegation(ctx context.Context, workspace *host.Workspace, actor, scope string, notAfter int64) error {
	log, err := workspace.Records(ctx)
	if err != nil || len(log.Records) == 0 {
		return errors.New("anchored identity is required")
	}
	standing := identity.Resolve(log).LookupActorAt(actor, log.Records[len(log.Records)-1].ID)
	if !standing.Anchored || (standing.Scope != "chess" && standing.Scope != scope) {
		return errors.New("anchored identity with matching chess scope is required")
	}
	page, err := application.ListAnchors(ctx, workspace, actor, "", 100)
	if err != nil {
		return errors.New("anchored identity is required")
	}
	for _, anchor := range page.Anchors {
		if anchor.Record == standing.Record {
			if anchor.NotAfter != 0 && notAfter > anchor.NotAfter {
				return errors.New("delegation must not outlive its identity anchor")
			}
			return nil
		}
	}
	return errors.New("anchored identity is required")
}

func currentIdentityView(ctx context.Context, workspace *host.Workspace, actor string) (map[string]any, error) {
	log, err := workspace.Records(ctx)
	if err != nil {
		return nil, err
	}
	resolved := identity.Resolved{}
	if len(log.Records) != 0 {
		resolved = identity.Resolve(log).LookupActorAt(actor, log.Records[len(log.Records)-1].ID)
	}
	view := map[string]any{
		"actor": actor, "anchored": resolved.Anchored, "display": resolved.Display(actor),
		"vouching": resolved.Vouching.String(), "verification": resolved.Verification.String(),
		"scope": resolved.Scope, "record": resolved.Record, "head": log.Head,
	}
	if resolved.Anchored {
		view["identity"] = resolved.Identity
	}
	return view, nil
}

func (service *identityHTTP) githubEndpoints() (*url.URL, *url.URL, error) {
	if service.config.GitHubClientID == "" || service.config.GitHubClientSecret == "" ||
		len(service.config.WitnessKey) != ed25519.PrivateKeySize || service.config.GitHubTokenURL == "" || service.config.GitHubUserURL == "" {
		return nil, nil, errors.New("GitHub identity is not configured")
	}
	authorize, err := fixedEndpoint(service.config.GitHubAuthorizeURL)
	if err != nil {
		return nil, nil, err
	}
	redirect, err := fixedEndpoint(service.config.GitHubRedirectURL)
	if err != nil {
		return nil, nil, err
	}
	if authorize.RawQuery != "" || redirect.RawQuery != "" || redirect.Fragment != "" || redirect.Path != "/v1/identity/github/callback" {
		return nil, nil, errors.New("GitHub identity endpoints are invalid")
	}
	if !loopbackRequestHost(redirect.Host) {
		return nil, nil, errors.New("GitHub redirect must resolve only to loopback")
	}
	token, err := fixedEndpoint(service.config.GitHubTokenURL)
	if err != nil {
		return nil, nil, err
	}
	if token.RawQuery != "" {
		return nil, nil, errors.New("GitHub token endpoint must not contain a query")
	}
	user, err := fixedEndpoint(service.config.GitHubUserURL)
	if err != nil {
		return nil, nil, err
	}
	if user.RawQuery != "" {
		return nil, nil, errors.New("GitHub user endpoint must not contain a query")
	}
	return authorize, redirect, nil
}

func fixedEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("provider endpoint is invalid")
	}
	if parsed.Scheme == "http" && !loopbackRequestHost(parsed.Host) {
		return nil, errors.New("plain HTTP provider endpoint must resolve only to loopback")
	}
	return parsed, nil
}

func (service *identityHTTP) guardGitHubCallback(request *http.Request) error {
	_, redirect, err := service.githubEndpoints()
	if err != nil || request.Host != redirect.Host || request.URL.Path != redirect.Path {
		return errors.New("callback target is invalid")
	}
	if origin := request.Header.Get("Origin"); origin != "" && origin != redirect.Scheme+"://"+redirect.Host {
		return errors.New("callback origin is invalid")
	}
	return nil
}

func (service *identityHTTP) githubPreflight(ctx context.Context, workspace *host.Workspace) error {
	if _, _, err := service.githubEndpoints(); err != nil || workspace == nil {
		return errors.New("GitHub identity is unavailable")
	}
	log, err := workspace.Records(ctx)
	if err != nil {
		return err
	}
	declared, ok := identity.Resolve(log).Witness()
	if !ok || declared.Key != hex.EncodeToString(service.config.WitnessKey.Public().(ed25519.PublicKey)) {
		return errors.New("GitHub witness key is not declared")
	}
	for _, scheme := range declared.Schemes {
		if scheme == identity.GitHubScheme {
			return nil
		}
	}
	return errors.New("GitHub witness does not cover GitHub")
}

func (service *identityHTTP) githubIdentity(ctx context.Context, code, verifier string) (identity.Identity, error) {
	values := url.Values{
		"client_id": {service.config.GitHubClientID}, "client_secret": {service.config.GitHubClientSecret},
		"code": {code}, "redirect_uri": {service.config.GitHubRedirectURL}, "code_verifier": {verifier},
	}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, service.config.GitHubTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return identity.Identity{}, err
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRequest.Header.Set("Accept", "application/json")
	tokenResponse, err := service.config.Client.Do(tokenRequest)
	if err != nil {
		return identity.Identity{}, err
	}
	defer tokenResponse.Body.Close()
	if tokenResponse.StatusCode != http.StatusOK {
		return identity.Identity{}, errors.New("provider refused token exchange")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := decodeProviderJSON(tokenResponse.Body, &token); err != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") {
		return identity.Identity{}, errors.New("provider returned no bearer token")
	}
	userRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, service.config.GitHubUserURL, nil)
	if err != nil {
		return identity.Identity{}, err
	}
	userRequest.Header.Set("Authorization", "Bearer "+token.AccessToken)
	userRequest.Header.Set("Accept", "application/vnd.github+json")
	userResponse, err := service.config.Client.Do(userRequest)
	if err != nil {
		return identity.Identity{}, err
	}
	defer userResponse.Body.Close()
	if userResponse.StatusCode != http.StatusOK {
		return identity.Identity{}, errors.New("provider refused identity lookup")
	}
	var user struct {
		ID    json.Number `json:"id"`
		Login string      `json:"login"`
	}
	if err := decodeProviderJSON(userResponse.Body, &user); err != nil {
		return identity.Identity{}, err
	}
	id, err := strconv.ParseUint(user.ID.String(), 10, 64)
	if err != nil || id == 0 || user.Login == "" || len(user.Login) > 128 || strings.ContainsAny(user.Login, "\r\n\x00") {
		return identity.Identity{}, errors.New("provider identity is invalid")
	}
	return identity.Identity{Scheme: identity.GitHubScheme, Subject: strconv.FormatUint(id, 10), Handle: user.Login}, nil
}

func decodeProviderJSON(reader io.Reader, target any) error {
	encoded, err := io.ReadAll(io.LimitReader(reader, identityProviderBody+1))
	if err != nil {
		return err
	}
	if len(encoded) > identityProviderBody {
		return errors.New("provider response is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("provider response has trailing data")
	}
	return nil
}

func (service *identityHTTP) randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := io.ReadFull(service.config.Random, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (service *identityHTTP) storeState(key string, state githubIdentityState) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	if len(service.states) >= identityMaxPending {
		return false
	}
	service.states[key] = state
	return true
}

func (service *identityHTTP) takeState(key string) (githubIdentityState, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	state, ok := service.states[key]
	delete(service.states, key)
	return state, ok
}

func (service *identityHTTP) storeDraft(key string, draft identityDraft) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	if len(service.drafts) >= identityMaxPending {
		return false
	}
	service.drafts[key] = draft
	return true
}

func (service *identityHTTP) takeDraft(key string) (identityDraft, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	draft, ok := service.drafts[key]
	delete(service.drafts, key)
	return draft, ok
}

func (service *identityHTTP) expireLocked() {
	now := service.config.Now()
	for key, state := range service.states {
		if !now.Before(state.Expires) {
			delete(service.states, key)
		}
	}
	for key, draft := range service.drafts {
		if !now.Before(draft.Expires) {
			delete(service.drafts, key)
		}
	}
}
