//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func requireUnsupportedCustodyRefusal(t *testing.T, err error) {
	t.Helper()
	want := "player-key custody is unsupported on " + runtime.GOOS
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("custody error = %v, want %q", err, want)
	}
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists after custody refusal: %v", path, err)
	}
}

func TestUnsupportedPlatformPlayerKeyCustodyFailsClosedBeforeState(t *testing.T) {
	ctx := context.Background()

	t.Run("init", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "new-repository")
		err := run(ctx, []string{"init", "--repo", repo}, io.Discard, strings.NewReader(""))
		requireUnsupportedCustodyRefusal(t, err)
		requireAbsent(t, repo)
	})

	t.Run("managed storage", func(t *testing.T) {
		repo := t.TempDir()
		_, err := openKeyStore(ctx, "", repo, false)
		requireUnsupportedCustodyRefusal(t, err)
		requireAbsent(t, filepath.Join(repo, "chess"))
	})

	t.Run("direct key operations", func(t *testing.T) {
		root := t.TempDir()
		opened, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		store := newKeyStore(opened, "player.key")
		operations := []struct {
			name string
			run  func() error
		}{
			{"ensure", func() error {
				_, err := ensureKey(store)
				return err
			}},
			{"read", func() error {
				_, err := readKey(store)
				return err
			}},
			{"publish", func() error {
				return publishKey(store, make(ed25519.PrivateKey, ed25519.PrivateKeySize))
			}},
		}
		for _, operation := range operations {
			t.Run(operation.name, func(t *testing.T) {
				requireUnsupportedCustodyRefusal(t, operation.run())
				requireAbsent(t, filepath.Join(root, "player.key"))
			})
		}
	})

	t.Run("explicit key and writer", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "repository")
		key := filepath.Join(base, "player.key")
		_, _, err := openWriter(ctx, &commonFlags{repo: repo, key: key})
		requireUnsupportedCustodyRefusal(t, err)
		requireAbsent(t, repo)
		requireAbsent(t, key)
	})

	t.Run("MCP discovery and write", func(t *testing.T) {
		initialized, respond := handleRPC(ctx, &commonFlags{}, rpcRequest{
			JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize",
		})
		if !respond || initialized.Error != nil {
			t.Fatalf("initialize = %+v, respond %v", initialized, respond)
		}
		listed, respond := handleRPC(ctx, &commonFlags{}, rpcRequest{
			JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "tools/list",
		})
		if !respond || listed.Error != nil {
			t.Fatalf("tools/list = %+v, respond %v", listed, respond)
		}

		base := t.TempDir()
		repo := filepath.Join(base, "repository")
		key := filepath.Join(base, "player.key")
		params, err := json.Marshal(callParams{
			Name: "create", Arguments: json.RawMessage(`{"color":"white"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		response, respond := handleRPC(ctx, &commonFlags{repo: repo, key: key}, rpcRequest{
			JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "tools/call", Params: params,
		})
		if !respond || response.Error != nil {
			t.Fatalf("tools/call = %+v, respond %v", response, respond)
		}
		result, ok := response.Result.(map[string]any)
		if !ok || result["isError"] != true {
			t.Fatalf("tools/call result = %#v", response.Result)
		}
		content, ok := result["content"].([]map[string]string)
		if !ok || len(content) != 1 {
			t.Fatalf("tools/call content = %#v", result["content"])
		}
		requireUnsupportedCustodyRefusal(t, errors.New(content[0]["text"]))
		requireAbsent(t, repo)
		requireAbsent(t, key)
	})
}
