package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/generalbusiness-ai/gitseq/host"
)

const maxAgentResponse = 1 << 20
const agentRequestTimeout = 10 * time.Second

type agentClient struct {
	base    string
	genesis string
	http    *http.Client
}

func agentServerURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") || u.Path != "" || u.RawPath != "" || u.Opaque != "" {
		return nil, errors.New("server must be a literal loopback HTTP origin without path, query, fragment or credentials")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Host == "" {
		return nil, errors.New("server must use a literal loopback IP")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid server port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid server port")
	}
	return u, nil
}
func newAgentClient(common *commonFlags) (*agentClient, error) {
	u, err := agentServerURL(common.server)
	if err != nil {
		return nil, err
	}
	if !validObjectName(common.genesis) || common.key == "" {
		return nil, errors.New("server mode requires --genesis and an existing --key")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 15 * time.Second, DisableCompression: true}
	return &agentClient{base: u.String(), genesis: common.genesis, http: &http.Client{Transport: transport, Timeout: agentRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("agent service redirects are refused") }}}, nil
}
func (c *agentClient) close() { c.http.CloseIdleConnections() }
func (c *agentClient) request(ctx context.Context, method, path string, input, target any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if len(data) > 32<<10 {
			return errors.New("native request is oversized")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("agent service is unavailable or timed out")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAgentResponse+1))
	if err != nil || len(data) > maxAgentResponse {
		return errors.New("agent service response is incomplete or oversized")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("agent service refused request (HTTP %d)", response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || strictJSON(data, target) != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("agent service returned a malformed response")
	}
	return nil
}
func (c *agentClient) check(ctx context.Context) error {
	var service agentService
	if err := c.request(ctx, "GET", "/v1/service", nil, &service); err != nil {
		return err
	}
	if service.Genesis != c.genesis || service.Version != agentTransportVersion || !reflect.DeepEqual(service.Operations, agentOperations) {
		return errors.New("agent service genesis or protocol does not match")
	}
	return nil
}
func (c *agentClient) mutate(ctx context.Context, custody *agentCustody, action agentAction, retry bool) (agentResult, error) {
	pending, err := custody.pending()
	if err != nil {
		return agentResult{}, err
	}
	if retry {
		if pending == nil {
			return agentResult{}, errors.New("there is no retained agent action to retry")
		}
		if pending.Action.Genesis != c.genesis {
			return agentResult{}, errors.New("retained action belongs to another genesis")
		}
	} else {
		if pending != nil {
			return agentResult{}, errors.New("outcome not confirmed; use retry for the retained action before another mutation")
		}
		action.Genesis = c.genesis
		action.ActorKey = custody.key.Public().(ed25519.PublicKey)
		if action.IdempotencyKey == "" {
			token := make([]byte, 24)
			if _, err := rand.Read(token); err != nil {
				return agentResult{}, err
			}
			action.IdempotencyKey = "agent:" + hex.EncodeToString(token)
		}
		act, err := action.act()
		if err != nil {
			return agentResult{}, err
		}
		if err = c.check(ctx); err != nil {
			return agentResult{}, err
		}
		var prepared agentPreparation
		if err = c.request(ctx, "POST", "/v1/actions/native/prepare", action, &prepared); err != nil {
			return agentResult{}, err
		}
		signing, err := host.ActorSigningBytes(prepared.Prepared)
		if err != nil || prepared.Version != agentTransportVersion || !reflect.DeepEqual(prepared.Echo, action) || !bytes.Equal(prepared.Prepared.Payload, act.Payload) || !bytes.Equal(prepared.SigningBytes, signing) {
			return agentResult{}, errors.New("prepared action does not match the chosen action")
		}
		pending = &agentSubmission{Action: action, Signature: ed25519.Sign(custody.key, signing)}
		if err = custody.retain(*pending); err != nil {
			return agentResult{}, fmt.Errorf("retain signed action before submission: %w", err)
		}
	}
	// Two submits at most. Every connection attempt checks the binding again;
	// neither branch prepares another action or opens a local repository.
	for attempt := 0; attempt < 2; attempt++ {
		var result agentResult
		err = c.check(ctx)
		if err == nil {
			err = c.request(ctx, "POST", "/v1/actions/native/submit", pending, &result)
		}
		if err == nil {
			if result.Genesis != c.genesis || !validEventID(result.Record) || !strings.HasPrefix(result.Record, c.genesis+"#") || result.Effective == nil || canonicalAgentObject(result.Head) == "" || result.Depth < 1 {
				err = errors.New("agent service returned an invalid confirmation")
			} else {
				if err = custody.clear(); err != nil {
					return agentResult{}, fmt.Errorf("action %s confirmed but pending cleanup failed; retry: %w", result.Record, err)
				}
				return result, nil
			}
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return agentResult{}, unknownAgentOutcome(pending, ctx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return agentResult{}, unknownAgentOutcome(pending, err)
}
func unknownAgentOutcome(pending *agentSubmission, err error) error {
	return fmt.Errorf("outcome not confirmed; use retry with this key and genesis (idempotency key %s): %w", pending.Action.IdempotencyKey, err)
}

func (c *agentClient) read(ctx context.Context, path string) (any, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := c.request(ctx, "GET", path, nil, &result); err != nil {
		return nil, err
	}
	var head string
	if json.Unmarshal(result["head"], &head) != nil || canonicalAgentObject(head) == "" {
		return nil, errors.New("agent read has no valid confirmed frontier")
	}
	return result, nil
}
