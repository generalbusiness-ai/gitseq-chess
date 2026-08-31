package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

func TestServeGitHubOAuthUsesOnlyTheExternalWitnessSigner(t *testing.T) {
	requireWritableKeyCustody(t)
	ctx := context.Background()
	repo, initializer, workspace := newIdentityTestRepository(t, ctx)
	_, witness := generateIdentityKey(t)
	if _, err := identity.DeclareWitness(ctx, workspace, initializer, witness.Public().(ed25519.PublicKey), []string{identity.GitHubScheme}); err != nil {
		t.Fatal(err)
	}
	signerSocket := startIdentitySigner(t, witness, nil)
	provider := newGitHubTestProvider(t)

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}

	processContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	command := exec.CommandContext(processContext, os.Args[0], "-test.run=^TestServeCommandHelper$", "--", "serve", "--repo", repo, "--listen", address)
	command.Env = identityServeEnvironment(os.Environ(), map[string]string{
		"CHESS_SERVE_COMMAND_HELPER":           "1",
		"GITSEQ_CHESS_GITHUB_AUTHORIZE_URL":    provider.URL + "/authorize",
		"GITSEQ_CHESS_GITHUB_TOKEN_URL":        provider.URL + "/token",
		"GITSEQ_CHESS_GITHUB_USER_URL":         provider.URL + "/user",
		"GITSEQ_CHESS_GITHUB_REDIRECT_URL":     "http://" + address + "/v1/identity/github/callback",
		"GITSEQ_CHESS_GITHUB_CLIENT_ID":        "client-id",
		"GITSEQ_CHESS_GITHUB_CLIENT_SECRET":    "client-secret",
		"GITSEQ_CHESS_IDENTITY_WITNESS_SOCKET": signerSocket,
	})
	for _, variable := range command.Env {
		if strings.HasPrefix(variable, "GITSEQ_CHESS_IDENTITY_WITNESS_KEY=") {
			t.Fatalf("serve process received retired private-key configuration %q", variable)
		}
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("serve process did not stop")
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "http://"+address {
		t.Fatalf("serve startup = %q err %v stderr %q", line, err, stderr.String())
	}

	client := &http.Client{Timeout: 2 * time.Second}
	actorPublic, _ := generateIdentityKey(t)
	started := struct {
		AuthorizeURL string `json:"authorize_url"`
	}{}
	postProcessJSON(t, client, "http://"+address+"/v1/identity/github/start", identityAnchorRequest{
		ActorKey: actorPublic, Scope: "chess", NotAfter: time.Now().Add(time.Hour).Unix(),
	}, &started)
	authorize, err := url.Parse(started.AuthorizeURL)
	if err != nil || authorize.Query().Get("state") == "" {
		t.Fatalf("authorize URL = %q err %v", started.AuthorizeURL, err)
	}
	callback := "http://" + address + "/v1/identity/github/callback?code=process-code&state=" + url.QueryEscape(authorize.Query().Get("state"))
	response, err := client.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-status="complete"`) {
		t.Fatalf("callback = %d %q err %v", response.StatusCode, body, readErr)
	}

	log, err := workspace.Records(ctx)
	if err != nil {
		t.Fatal(err)
	}
	anchored := false
	for _, record := range log.Records {
		if record.Schema == identity.AnchorSchema {
			anchored = true
			if record.Actor != liveFingerprint(witness.Public().(ed25519.PublicKey)) {
				t.Fatalf("anchor signer = %s, want external witness", record.Actor)
			}
		}
	}
	if !anchored {
		t.Fatal("serve completed OAuth without appending the externally signed anchor")
	}
	_, projection, err := application.OpenProjection(ctx, repo)
	if err != nil || len(projection.Games) != 0 {
		t.Fatalf("process test changed chess state: games=%d err=%v", len(projection.Games), err)
	}
}

func identityServeEnvironment(base []string, values map[string]string) []string {
	filtered := make([]string, 0, len(base)+len(values))
	for _, variable := range base {
		if strings.HasPrefix(variable, "GITSEQ_CHESS_GITHUB_") ||
			strings.HasPrefix(variable, "GITSEQ_CHESS_IDENTITY_") ||
			strings.HasPrefix(variable, "CHESS_SERVE_COMMAND_HELPER=") {
			continue
		}
		filtered = append(filtered, variable)
	}
	for key, value := range values {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}

func postProcessJSON(t *testing.T, client *http.Client, target string, value, decoded any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s = %d %q", target, response.StatusCode, message)
	}
	if err := json.NewDecoder(response.Body).Decode(decoded); err != nil {
		t.Fatal(err)
	}
}
