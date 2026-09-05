package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
	"github.com/generalbusiness-ai/gitseq/host/live"
)

const (
	identityStateTTL     = 10 * time.Minute
	identityDraftTTL     = 5 * time.Minute
	identityMaxExpiry    = 90 * 24 * time.Hour
	identityMaxPending   = 128
	identityProviderBody = 32 << 10
	identitySignerBytes  = 64 << 10
	identitySignerReply  = ed25519.PublicKeySize + ed25519.SignatureSize
	identitySignerWait   = 3 * time.Second
	githubProofDomain    = "gitseq-chess/github-oauth-possession/v1\x00"
)

type identityHTTPConfig struct {
	GitHubAuthorizeURL string
	GitHubTokenURL     string
	GitHubUserURL      string
	GitHubRedirectURL  string
	GitHubClientID     string
	GitHubClientSecret string
	WitnessSocket      string
	Client             *http.Client
	SignerTimeout      time.Duration
	Now                func() time.Time
	Random             io.Reader
}

type identityHTTP struct {
	repo   string
	open   repositoryOpener
	config identityHTTPConfig
	mu     sync.Mutex
	states map[string]githubIdentityState
	drafts map[string]identityDraft
	proofs map[string]githubPossession
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

type githubPossession struct {
	ActorKey ed25519.PublicKey
	Scope    string
	NotAfter int64
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

type githubPossessionRequest struct {
	ActorKey       []byte                `json:"actor_key"`
	Scope          string                `json:"scope"`
	NotAfter       int64                 `json:"not_after"`
	Challenge      live.SessionChallenge `json:"challenge"`
	ActorSignature []byte                `json:"actor_signature"`
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
	if config.SignerTimeout <= 0 {
		config.SignerTimeout = identitySignerWait
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
		repo: repo, config: config, open: localRepositoryOpener(repo),
		states: make(map[string]githubIdentityState), drafts: make(map[string]identityDraft),
		proofs: make(map[string]githubPossession),
	}
}

func identityHTTPConfigFromEnvironment() (identityHTTPConfig, error) {
	config := identityHTTPConfig{
		GitHubAuthorizeURL: os.Getenv("GITSEQ_CHESS_GITHUB_AUTHORIZE_URL"),
		GitHubTokenURL:     os.Getenv("GITSEQ_CHESS_GITHUB_TOKEN_URL"),
		GitHubUserURL:      os.Getenv("GITSEQ_CHESS_GITHUB_USER_URL"),
		GitHubRedirectURL:  os.Getenv("GITSEQ_CHESS_GITHUB_REDIRECT_URL"),
		GitHubClientID:     os.Getenv("GITSEQ_CHESS_GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITSEQ_CHESS_GITHUB_CLIENT_SECRET"),
		WitnessSocket:      os.Getenv("GITSEQ_CHESS_IDENTITY_WITNESS_SOCKET"),
	}
	configured := config.GitHubRedirectURL != "" || config.GitHubClientID != "" ||
		config.GitHubClientSecret != "" || config.WitnessSocket != "" || config.GitHubAuthorizeURL != "" ||
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
	if config.GitHubRedirectURL == "" || config.GitHubClientID == "" || config.GitHubClientSecret == "" || config.WitnessSocket == "" {
		return identityHTTPConfig{}, errors.New("GitHub identity configuration is incomplete")
	}
	if !filepath.IsAbs(config.WitnessSocket) {
		return identityHTTPConfig{}, errors.New("GitHub identity witness socket must be an absolute path")
	}
	return config, nil
}

func (service *identityHTTP) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/identity/status", service.status)
	mux.HandleFunc("POST /v1/identity/github/challenge", service.githubChallenge)
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
	workspace, _, err := service.open(request.Context())
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

func (service *identityHTTP) githubChallenge(w http.ResponseWriter, request *http.Request) {
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
	challenge, signingBytes, err := service.prepareGitHubPossession(public, input.Scope, input.NotAfter)
	if err != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	serveJSON(w, map[string]any{"challenge": challenge, "signing_bytes": signingBytes})
}

func (service *identityHTTP) githubStart(w http.ResponseWriter, request *http.Request) {
	var input githubPossessionRequest
	if err := decodeHTTPRequest(w, request, &input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proof, signingBytes, ok := service.takeGitHubPossession(input.Challenge)
	if !ok {
		http.Error(w, "GitHub possession proof is invalid", http.StatusBadRequest)
		return
	}
	public, err := browserPublicKey(input.ActorKey)
	if err != nil {
		http.Error(w, "GitHub possession proof is invalid", http.StatusBadRequest)
		return
	}
	if err := service.validateGrant(input.Scope, input.NotAfter); err != nil {
		http.Error(w, "GitHub possession proof is invalid", http.StatusBadRequest)
		return
	}
	if !bytes.Equal(public, proof.ActorKey) || !bytes.Equal(public, input.Challenge.ActorKey) ||
		input.Scope != proof.Scope || input.NotAfter != proof.NotAfter ||
		len(input.ActorSignature) != ed25519.SignatureSize || !ed25519.Verify(public, signingBytes, input.ActorSignature) {
		http.Error(w, "GitHub possession proof is invalid", http.StatusBadRequest)
		return
	}
	actor := liveFingerprint(public)
	workspace, _, err := service.open(request.Context())
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
	workspace, _, err := service.open(request.Context())
	if err != nil || service.githubPreflight(request.Context(), workspace) != nil {
		http.Error(w, "GitHub identity is unavailable", http.StatusServiceUnavailable)
		return
	}
	providerIdentity, err := service.githubIdentity(request.Context(), query.Get("code"), attempt.Verifier)
	if err != nil {
		http.Error(w, "GitHub identity could not be verified", http.StatusBadGateway)
		return
	}
	prepared, err := workspace.PrepareEndorsement(request.Context(), identity.Anchor{
		Subject: attempt.Subject, Identity: &providerIdentity, Scope: attempt.Scope, NotAfter: attempt.NotAfter,
	}, githubIdentityRetryKey(query.Get("state")))
	if err != nil {
		service.githubAnchorFailure(w)
		return
	}
	signingBytes, err := host.ActorSigningBytes(prepared)
	if err != nil || len(signingBytes) == 0 || len(signingBytes) > identitySignerBytes {
		service.githubAnchorFailure(w)
		return
	}
	public, signature, err := service.signGitHubAnchor(request.Context(), signingBytes)
	if err != nil {
		service.githubAnchorFailure(w)
		return
	}
	// Signing crosses an external process boundary. Re-read the confirmed
	// witness declaration before using the reply; an older snapshot cannot
	// authorize a signer that was replaced while the request was in flight.
	workspace, _, err = service.open(request.Context())
	if err != nil {
		service.githubAnchorFailure(w)
		return
	}
	declared, err := githubWitnessKey(request.Context(), workspace)
	if err != nil || !bytes.Equal(public, declared) || !ed25519.Verify(public, signingBytes, signature) {
		service.githubAnchorFailure(w)
		return
	}
	record, err := workspace.AppendSigned(request.Context(), host.SignedAct{
		Prepared: prepared, ActorKey: public, ActorSignature: signature,
	})
	if err != nil {
		service.githubAnchorFailure(w)
		return
	}
	outcome := application.IdentityOutcome(request.Context(), workspace, record)
	if outcome.Outcome != "created" {
		service.githubAnchorFailure(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>GitHub identity complete</title><script src="/assets/app.js" defer></script></head><body data-view="oauth-callback" data-status="complete"><main><h1>Identity connected</h1><p>You may return to the chess game.</p></main></body></html>`)
}

func githubIdentityRetryKey(state string) string {
	digest := sha256.Sum256([]byte("gitseq-chess/github-oauth-anchor\x00" + state))
	return "github-oauth-" + hex.EncodeToString(digest[:])
}

// The signer protocol is one request and one response per Unix connection.
// Each frame starts with a four-byte big-endian length. The request body is
// exactly ActorSigningBytes. The response body is exactly the 32-byte Ed25519
// public key followed by its 64-byte signature.
func (service *identityHTTP) signGitHubAnchor(ctx context.Context, signingBytes []byte) (ed25519.PublicKey, []byte, error) {
	if len(signingBytes) == 0 || len(signingBytes) > identitySignerBytes {
		return nil, nil, errors.New("signing request is out of bounds")
	}
	dialer := net.Dialer{Timeout: service.config.SignerTimeout}
	connection, err := dialer.DialContext(ctx, "unix", service.config.WitnessSocket)
	if err != nil {
		return nil, nil, err
	}
	defer connection.Close()
	deadline := time.Now().Add(service.config.SignerTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	request := make([]byte, 4+len(signingBytes))
	binary.BigEndian.PutUint32(request[:4], uint32(len(signingBytes)))
	copy(request[4:], signingBytes)
	if _, err := io.Copy(connection, bytes.NewReader(request)); err != nil {
		return nil, nil, err
	}
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		return nil, nil, err
	}
	if binary.BigEndian.Uint32(header[:]) != identitySignerReply {
		return nil, nil, errors.New("signer response has the wrong size")
	}
	response := make([]byte, identitySignerReply)
	if _, err := io.ReadFull(connection, response); err != nil {
		return nil, nil, err
	}
	public := ed25519.PublicKey(bytes.Clone(response[:ed25519.PublicKeySize]))
	signature := bytes.Clone(response[ed25519.PublicKeySize:])
	return public, signature, nil
}

func (service *identityHTTP) githubAnchorFailure(w http.ResponseWriter) {
	http.Error(w, "GitHub identity could not be recorded", http.StatusServiceUnavailable)
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
	workspace, _, err := service.open(request.Context())
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
	workspace, _, err := service.open(request.Context())
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
	prepared, err := workspace.PrepareEndorsement(request.Context(), identity.Anchor{
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
	workspace, _, err := service.open(request.Context())
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

func (service *identityHTTP) validateDelegation(ctx context.Context, workspace application.RecordReader, actor, scope string, notAfter int64) error {
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

func currentIdentityView(ctx context.Context, workspace application.RecordReader, actor string) (map[string]any, error) {
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
		service.config.WitnessSocket == "" || service.config.GitHubTokenURL == "" || service.config.GitHubUserURL == "" {
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

func (service *identityHTTP) githubPreflight(ctx context.Context, workspace application.RecordReader) error {
	if _, _, err := service.githubEndpoints(); err != nil || workspace == nil {
		return errors.New("GitHub identity is unavailable")
	}
	_, err := githubWitnessKey(ctx, workspace)
	return err
}

func githubWitnessKey(ctx context.Context, workspace application.RecordReader) (ed25519.PublicKey, error) {
	log, err := workspace.Records(ctx)
	if err != nil {
		return nil, err
	}
	declared, ok := identity.Resolve(log).Witness()
	if !ok {
		return nil, errors.New("GitHub witness key is not declared")
	}
	for _, scheme := range declared.Schemes {
		if scheme == identity.GitHubScheme {
			key, err := hex.DecodeString(declared.Key)
			if err != nil || len(key) != ed25519.PublicKeySize {
				return nil, errors.New("GitHub witness key is invalid")
			}
			return ed25519.PublicKey(key), nil
		}
	}
	return nil, errors.New("GitHub witness does not cover GitHub")
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

func (service *identityHTTP) prepareGitHubPossession(public ed25519.PublicKey, scope string, notAfter int64) (live.SessionChallenge, []byte, error) {
	attempt := make([]byte, 16)
	if _, err := io.ReadFull(service.config.Random, attempt); err != nil {
		return live.SessionChallenge{}, nil, err
	}
	bound := make([]byte, 0, len(githubProofDomain)+len(attempt)+len(public)+2+len(scope)+8)
	bound = append(bound, githubProofDomain...)
	bound = append(bound, attempt...)
	bound = append(bound, public...)
	var encoded [8]byte
	binary.BigEndian.PutUint16(encoded[:2], uint16(len(scope)))
	bound = append(bound, encoded[:2]...)
	bound = append(bound, scope...)
	binary.BigEndian.PutUint64(encoded[:], uint64(notAfter))
	bound = append(bound, encoded[:]...)
	nonce := sha256.Sum256(bound)
	challenge := live.SessionChallenge{
		Version: 0, Generation: "generation:" + hex.EncodeToString(attempt),
		Nonce: nonce[:], ActorKey: bytes.Clone(public),
	}
	signingBytes, err := live.SessionSigningBytes(challenge)
	if err != nil {
		return live.SessionChallenge{}, nil, err
	}
	key := githubPossessionKey(signingBytes)
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	if len(service.proofs) >= identityMaxPending {
		return live.SessionChallenge{}, nil, errors.New("pending GitHub proof limit reached")
	}
	if _, exists := service.proofs[key]; exists {
		return live.SessionChallenge{}, nil, errors.New("GitHub proof collision")
	}
	service.proofs[key] = githubPossession{
		ActorKey: bytes.Clone(public), Scope: scope, NotAfter: notAfter,
		Expires: service.config.Now().Add(live.SessionChallengeTTL),
	}
	return challenge, signingBytes, nil
}

func githubPossessionKey(signingBytes []byte) string {
	digest := sha256.Sum256(signingBytes)
	return hex.EncodeToString(digest[:])
}

func (service *identityHTTP) takeGitHubPossession(challenge live.SessionChallenge) (githubPossession, []byte, bool) {
	signingBytes, err := live.SessionSigningBytes(challenge)
	if err != nil {
		return githubPossession{}, nil, false
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.expireLocked()
	key := githubPossessionKey(signingBytes)
	proof, ok := service.proofs[key]
	delete(service.proofs, key)
	return proof, signingBytes, ok
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
	for key, proof := range service.proofs {
		if !now.Before(proof.Expires) {
			delete(service.proofs, key)
		}
	}
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
