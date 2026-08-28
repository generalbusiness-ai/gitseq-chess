package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

func TestIdentityRoutesUseTheBrowserMutationBoundary(t *testing.T) {
	handler := newReadHandler(context.Background(), filepath.Join(t.TempDir(), "missing"))
	request := httptest.NewRequest(http.MethodPost, "/v1/identity/status", strings.NewReader(`{"actor_key":[]}`))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://attacker.invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Body.String() != "cross-origin mutation refused\n" {
		t.Fatalf("cross-origin identity response = %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/identity/status?secret=not-allowed", strings.NewReader(`{"actor_key":[]}`))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Body.String() != "unknown query field \"secret\"\n" {
		t.Fatalf("identity query response = %d %q", response.Code, response.Body.String())
	}
}

func TestIdentityEndorsementPreparesExactBytesAndRefusesSubstitution(t *testing.T) {
	ctx := context.Background()
	repo, initializer, workspace := newIdentityTestRepository(t, ctx)
	_, witness, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.DeclareWitness(ctx, workspace, initializer, witness.Public().(ed25519.PublicKey), []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	rootPublic, root := generateIdentityKey(t)
	now := time.Now().Truncate(time.Second)
	if _, err := identity.Endorse(ctx, workspace, witness, identity.Anchor{
		Subject: liveFingerprint(rootPublic), Identity: &identity.Identity{Scheme: identity.GitHubScheme, Subject: "42", Handle: "alice"},
		Scope: "chess", NotAfter: now.Add(2 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := newReadHandlerWithIdentity(ctx, repo, runtime, identityHTTPConfig{Now: func() time.Time { return now }})
	agentPublic, _ := generateIdentityKey(t)
	agent := liveFingerprint(agentPublic)
	prepare := func(key []byte, signatureKey ed25519.PrivateKey) (string, []byte) {
		t.Helper()
		var prepared struct {
			Draft        string `json:"draft"`
			SigningBytes []byte `json:"signing_bytes"`
		}
		postJSON(t, handler, "/v1/identity/endorsement/prepare", identityAnchorRequest{
			ActorKey: key, Subject: agent, Scope: "chess", NotAfter: now.Add(time.Hour).Unix(),
		}, &prepared, http.StatusOK)
		if prepared.Draft == "" || len(prepared.SigningBytes) == 0 {
			t.Fatalf("prepared identity response = %+v", prepared)
		}
		return prepared.Draft, ed25519.Sign(signatureKey, prepared.SigningBytes)
	}

	draft, signature := prepare(rootPublic, root)
	wrongPublic, _ := generateIdentityKey(t)
	postJSON(t, handler, "/v1/identity/endorsement/submit", identitySubmitRequest{
		Draft: draft, ActorKey: wrongPublic, ActorSignature: signature,
	}, nil, http.StatusBadRequest)

	draft, signature = prepare(rootPublic, root)
	signature[0] ^= 1
	postJSON(t, handler, "/v1/identity/endorsement/submit", identitySubmitRequest{
		Draft: draft, ActorKey: rootPublic, ActorSignature: signature,
	}, nil, http.StatusBadRequest)

	draft, signature = prepare(rootPublic, root)
	var submitted map[string]any
	postJSON(t, handler, "/v1/identity/endorsement/submit", identitySubmitRequest{
		Draft: draft, ActorKey: rootPublic, ActorSignature: signature,
	}, &submitted, http.StatusOK)
	if submitted["outcome"] != "created" || submitted["actor"] != agent || submitted["display"] != "alice [github:42] (witnessed; in-log)" {
		t.Fatalf("submitted identity = %#v", submitted)
	}
	postJSON(t, handler, "/v1/identity/endorsement/submit", identitySubmitRequest{
		Draft: draft, ActorKey: rootPublic, ActorSignature: signature,
	}, nil, http.StatusBadRequest)

	unanchoredPublic, unanchored := generateIdentityKey(t)
	var refused map[string]any
	postIdentityJSON(t, handler, "/v1/identity/endorsement/prepare", identityAnchorRequest{
		ActorKey: unanchoredPublic, Subject: liveFingerprint(generatePublic(t)), Scope: "chess", NotAfter: now.Add(time.Hour).Unix(),
	}, &refused, http.StatusForbidden)
	_ = unanchored
}

func TestNostrTemplateIsTheExactHostIdentityEvent(t *testing.T) {
	ctx := context.Background()
	repo, _, workspace := newIdentityTestRepository(t, ctx)
	public, _ := generateIdentityKey(t)
	now := time.Unix(1_800_000_000, 0)
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := newReadHandlerWithIdentity(ctx, repo, runtime, identityHTTPConfig{Now: func() time.Time { return now }})
	notAfter := now.Add(time.Hour).Unix()
	var response struct {
		Actor string `json:"actor"`
		Event struct {
			Kind      int64      `json:"kind"`
			CreatedAt int64      `json:"created_at"`
			Tags      [][]string `json:"tags"`
			Content   string     `json:"content"`
		} `json:"event"`
	}
	postJSON(t, handler, "/v1/identity/nostr/template", identityAnchorRequest{
		ActorKey: public, Scope: "chess", NotAfter: notAfter,
	}, &response, http.StatusOK)
	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := identity.NostrDelegation(identity.Anchor{
		Genesis: log.Genesis, Subject: liveFingerprint(public), Scope: "chess", NotAfter: notAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Actor != liveFingerprint(public) || response.Event.Content != want || response.Event.Kind != identity.NostrProofKind || response.Event.CreatedAt != now.Unix() || response.Event.Tags == nil || len(response.Event.Tags) != 0 {
		t.Fatalf("Nostr template = %+v, want exact content %q", response, want)
	}
	fakeProof := identity.NostrProof{
		ID: strings.Repeat("0", 64), PubKey: strings.Repeat("0", 64), Sig: strings.Repeat("0", 128),
		CreatedAt: response.Event.CreatedAt, Kind: response.Event.Kind, Tags: [][]string{}, Content: response.Event.Content,
	}
	refused := postIdentityJSON(t, handler, "/v1/identity/endorsement/prepare", identityAnchorRequest{
		ActorKey: public, Subject: response.Actor, Scope: "chess", NotAfter: notAfter, Nostr: &fakeProof,
	}, nil, http.StatusBadRequest)
	if strings.Contains(refused.Body.String(), fakeProof.ID) || strings.Contains(refused.Body.String(), fakeProof.Sig) {
		t.Fatalf("Nostr proof escaped through error: %q", refused.Body.String())
	}
}

func TestChessSeatRecoveryUsesIdentityAndRefusesOtherAuthority(t *testing.T) {
	ctx := context.Background()
	_, initializer, workspace := newIdentityTestRepository(t, ctx)
	_, witness := generateIdentityKey(t)
	if _, err := identity.DeclareWitness(ctx, workspace, initializer, witness.Public().(ed25519.PublicKey), []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	oldPublic, oldKey := generateIdentityKey(t)
	newPublic, newKey := generateIdentityKey(t)
	bobPublic, bobKey := generateIdentityKey(t)
	if liveFingerprint(oldPublic) == liveFingerprint(newPublic) {
		t.Fatal("old and replacement sessions have the same fingerprint")
	}
	anchor := func(public ed25519.PublicKey, account string, notAfter int64) host.Record {
		t.Helper()
		record, err := identity.Endorse(ctx, workspace, witness, identity.Anchor{
			Subject: liveFingerprint(public), Identity: &identity.Identity{Scheme: identity.GitHubScheme, Subject: account},
			Scope: "chess", NotAfter: notAfter,
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	future := time.Now().Add(time.Hour).Unix()
	anchor(oldPublic, "42", future)
	create, err := application.Create(ctx, workspace, oldKey, "white", "", "", "recovery-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.Join(ctx, workspace, bobKey, create.ID, "", "recovery-join"); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Move(ctx, workspace, oldKey, create.ID, "e2e4", "recovery-white-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Move(ctx, workspace, bobKey, create.ID, "e7e5", "recovery-black-1"); err != nil {
		t.Fatal(err)
	}
	anchor(newPublic, "42", future)
	recovered, err := application.Move(ctx, workspace, newKey, create.ID, "g1f3", "recovery-white-2")
	if err != nil {
		t.Fatal(err)
	}
	assertChessDecision(t, ctx, workspace, recovered.ID, true)
	if _, err := application.Move(ctx, workspace, bobKey, create.ID, "b8c6", "recovery-black-2"); err != nil {
		t.Fatal(err)
	}

	wrongPublic, wrongKey := generateIdentityKey(t)
	anchor(wrongPublic, "99", future)
	wrong, err := application.Move(ctx, workspace, wrongKey, create.ID, "f1b5", "recovery-wrong")
	if err != nil {
		t.Fatal(err)
	}
	assertChessDecision(t, ctx, workspace, wrong.ID, false)

	_, unanchoredKey := generateIdentityKey(t)
	unanchored, err := application.Move(ctx, workspace, unanchoredKey, create.ID, "f1b5", "recovery-unanchored")
	if err != nil {
		t.Fatal(err)
	}
	assertChessDecision(t, ctx, workspace, unanchored.ID, false)

	expiredPublic, expiredKey := generateIdentityKey(t)
	anchor(expiredPublic, "42", time.Now().Add(-time.Hour).Unix())
	expired, err := application.Move(ctx, workspace, expiredKey, create.ID, "f1b5", "recovery-expired")
	if err != nil {
		t.Fatal(err)
	}
	assertChessDecision(t, ctx, workspace, expired.ID, false)

	revokedPublic, revokedKey := generateIdentityKey(t)
	revokedAnchor := anchor(revokedPublic, "42", future)
	if _, err := identity.Revoke(ctx, workspace, witness, revokedAnchor.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := application.Move(ctx, workspace, revokedKey, create.ID, "f1b5", "recovery-revoked")
	if err != nil {
		t.Fatal(err)
	}
	assertChessDecision(t, ctx, workspace, revoked.ID, false)
	_ = bobPublic
}

func TestGitHubOAuthBindsStatePKCEOriginAndRedactsTokens(t *testing.T) {
	ctx := context.Background()
	repo, initializer, workspace := newIdentityTestRepository(t, ctx)
	_, witness := generateIdentityKey(t)
	if _, err := identity.DeclareWitness(ctx, workspace, initializer, witness.Public().(ed25519.PublicKey), []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	const providerToken = "provider-token-must-not-escape"
	var tokenForm url.Values
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/authorize":
			http.Error(w, "browser only", http.StatusBadRequest)
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			tokenForm = request.PostForm
			serveJSON(w, map[string]string{"access_token": providerToken, "token_type": "bearer"})
		case "/user":
			if request.Header.Get("Authorization") != "Bearer "+providerToken {
				t.Errorf("provider Authorization = %q", request.Header.Get("Authorization"))
			}
			serveJSON(w, map[string]any{"id": 4242, "login": "alice"})
		default:
			http.NotFound(w, request)
		}
	}))
	defer provider.Close()
	now := time.Now().Truncate(time.Second)
	config := identityHTTPConfig{
		GitHubAuthorizeURL: provider.URL + "/authorize", GitHubTokenURL: provider.URL + "/token",
		GitHubUserURL: provider.URL + "/user", GitHubRedirectURL: "http://127.0.0.1:8080/v1/identity/github/callback",
		GitHubClientID: "client-id", GitHubClientSecret: "client-secret-must-not-escape", WitnessKey: witness,
		Now: func() time.Time { return now }, Client: provider.Client(),
	}
	runtime, err := newChessLive()
	if err != nil {
		t.Fatal(err)
	}
	handler := newReadHandlerWithIdentity(ctx, repo, runtime, config)
	actorPublic, _ := generateIdentityKey(t)
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	postJSON(t, handler, "/v1/identity/github/start", identityAnchorRequest{
		ActorKey: actorPublic, Scope: "chess", NotAfter: now.Add(time.Hour).Unix(),
	}, &started, http.StatusOK)
	authorize, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authorize.Query().Get("state")
	challenge := authorize.Query().Get("code_challenge")
	if authorize.Scheme+"://"+authorize.Host+authorize.Path != provider.URL+"/authorize" || state == "" || challenge == "" || authorize.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL = %q", started.AuthorizeURL)
	}
	if strings.Contains(started.AuthorizeURL, config.GitHubClientSecret) || strings.Contains(started.AuthorizeURL, providerToken) {
		t.Fatalf("authorize URL contains a secret: %q", started.AuthorizeURL)
	}
	callbackTarget := "/v1/identity/github/callback?code=one-time-code&state=" + url.QueryEscape(state)
	badOrigin := httptest.NewRequest(http.MethodGet, callbackTarget, nil)
	badOrigin.Host = "127.0.0.1:8080"
	badOrigin.Header.Set("Origin", "http://attacker.invalid")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, badOrigin)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad callback origin status = %d", badResponse.Code)
	}

	callback := httptest.NewRequest(http.MethodGet, callbackTarget, nil)
	callback.Host = "127.0.0.1:8080"
	callback.Header.Set("Origin", "http://127.0.0.1:8080")
	callbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusOK || !strings.Contains(callbackResponse.Body.String(), `data-view="oauth-callback" data-status="complete"`) {
		t.Fatalf("callback = %d %q", callbackResponse.Code, callbackResponse.Body.String())
	}
	if tokenForm.Get("code_verifier") == "" || tokenForm.Get("code") != "one-time-code" || tokenForm.Get("client_secret") != config.GitHubClientSecret {
		t.Fatalf("token exchange fields = %#v", tokenForm)
	}
	for _, body := range []string{badResponse.Body.String(), callbackResponse.Body.String()} {
		if strings.Contains(body, providerToken) || strings.Contains(body, config.GitHubClientSecret) || strings.Contains(body, "one-time-code") {
			t.Fatalf("OAuth secret escaped in response %q", body)
		}
	}
	replay := httptest.NewRequest(http.MethodGet, callbackTarget, nil)
	replay.Host = "127.0.0.1:8080"
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("OAuth state replay status = %d", replayResponse.Code)
	}
	var status map[string]any
	postJSON(t, handler, "/v1/identity/status", identityActorRequest{ActorKey: actorPublic}, &status, http.StatusOK)
	if status["anchored"] != true || status["display"] != "alice [github:4242] (witnessed; in-log)" {
		t.Fatalf("GitHub identity status = %#v", status)
	}
}

func newIdentityTestRepository(t *testing.T, ctx context.Context) (string, ed25519.PrivateKey, *host.Workspace) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	_, initializer := generateIdentityKey(t)
	workspace, err := host.Init(ctx, repo, application.Application, initializer, host.Options{PayloadCeiling: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return repo, initializer, workspace
}

func generateIdentityKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func generatePublic(t *testing.T) ed25519.PublicKey {
	t.Helper()
	public, _ := generateIdentityKey(t)
	return public
}

func assertChessDecision(t *testing.T, ctx context.Context, workspace *host.Workspace, record string, want bool) {
	t.Helper()
	effective, found, reason, err := application.Decision(ctx, workspace, record)
	if err != nil || !found || effective != want {
		t.Fatalf("decision %s = effective %v found %v reason %q err %v, want %v", record, effective, found, reason, err, want)
	}
}

func postIdentityJSON(t *testing.T, handler http.Handler, target string, value, decoded any, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("POST %s = %d %q, want %d", target, response.Code, response.Body.String(), wantStatus)
	}
	if decoded != nil && response.Code == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(decoded); err != nil && err != io.EOF {
			t.Fatal(err)
		}
	}
	return response
}
