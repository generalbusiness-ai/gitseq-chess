package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// managedStore is the store this program chooses: bounded by a stand-in for
// the Git common directory.
func managedStore(t *testing.T, root string) *keyStore {
	t.Helper()
	store, err := managedStoreOrError(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// managedStoreOrError builds the store the way openKeyStore does for a path
// this program chose, without needing a Git repository.
func managedStoreOrError(root string) (*keyStore, error) {
	outer, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer outer.Close()
	inner, err := openDirectory(outer, "chess")
	if err != nil {
		return nil, err
	}
	return newKeyStore(inner, "player.key"), nil
}

// namedStore is the store an operator names with --key: bounded only by the
// file's own parent directory.
func namedStore(t *testing.T, path string) *keyStore {
	t.Helper()
	opened, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	return newKeyStore(opened, filepath.Base(path))
}

func writeKey(t *testing.T, path string, mode os.FileMode) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(private)+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is filtered by umask, so say it again.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return private
}

func TestKeyIsNotReadThroughASymbolicLink(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "someone-elses.key")
	writeKey(t, elsewhere, 0o600)
	path := filepath.Join(root, "chess", "player.key")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	store, err := managedStoreOrError(root)
	if err != nil {
		t.Fatalf("the store itself was refused: %v", err)
	}
	defer store.Close()
	if _, err := ensureKey(store); err == nil {
		t.Fatal("reading a key through a symlink out of the root was allowed")
	}
}

// A link that stays inside the root is still a link. os.Root would follow it,
// so the refusal has to come from the explicit Lstat/open/SameFile guard.
func TestKeyIsNotReadThroughALinkThatStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "decoy.key")
	writeKey(t, inside, 0o600)
	path := filepath.Join(root, "chess", "player.key")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../decoy.key", path); err != nil {
		t.Fatal(err)
	}
	store, err := managedStoreOrError(root)
	if err != nil {
		t.Fatalf("the store itself was refused: %v", err)
	}
	defer store.Close()
	if _, err := ensureKey(store); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a link inside the root was followed: %v", err)
	}
}

func TestNamedKeyPathStillRefusesASymbolicLink(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "someone-elses.key")
	writeKey(t, elsewhere, 0o600)
	link := filepath.Join(t.TempDir(), "named.key")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureKey(namedStore(t, link)); err == nil {
		t.Fatal("a named path followed a symlink out of its directory to a private key")
	}
}

func TestKeyIsNotPlacedBeneathASymbolicLinkOutOfTheRoot(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "chess")); err != nil {
		t.Fatal(err)
	}
	if _, err := managedStoreOrError(root); err == nil {
		t.Fatal("a key directory that is a symbolic link was accepted")
	}
	if _, err := os.Stat(filepath.Join(target, "player.key")); err == nil {
		t.Fatal("the key was written into the link target")
	}
}

func TestKeyReadableByAnyoneElseIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		root := t.TempDir()
		writeKey(t, filepath.Join(root, "chess", "player.key"), mode)
		if _, err := ensureKey(managedStore(t, root)); err == nil || !strings.Contains(err.Error(), "mode 0400 or 0600") {
			t.Fatalf("mode %04o was accepted: %v", mode, err)
		}
	}
}

func TestExistingOwnerOnlyKeyModesAreAccepted(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600} {
		root := t.TempDir()
		want := writeKey(t, filepath.Join(root, "chess", "player.key"), mode)
		got, err := ensureKey(managedStore(t, root))
		if err != nil {
			t.Fatalf("mode %04o: %v", mode, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("mode %04o: ensureKey returned a different key than the file holds", mode)
		}
	}
}

func TestPublicationLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	if _, err := ensureKey(managedStore(t, root)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "chess"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "player.key" {
			t.Fatalf("publication left %s behind", entry.Name())
		}
	}
}

// A crash between writing the temporary file and linking it must leave no key
// at the real name, so the next run publishes cleanly rather than reading a
// half-written one. The interruption is injected at the link itself: writing
// an unrelated temporary file would pass even if publishKey wrote straight to
// the target.
func TestInterruptionAtTheLinkLeavesNoKey(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	interrupted := errors.New("interrupted before the link")
	var temporary string
	store.link = func(from, to string) error {
		temporary = from
		if _, err := store.root.Lstat(to); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s already exists at the moment of linking: %v", to, err)
		}
		return interrupted
	}
	if _, err := ensureKey(store); !errors.Is(err, interrupted) {
		t.Fatalf("ensureKey = %v, want the injected interruption", err)
	}
	if _, err := store.root.Lstat("player.key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an interrupted publication left a key behind: %v", err)
	}
	if _, err := store.root.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an interrupted publication left %s behind: %v", temporary, err)
	}
}

// Two processes racing to create the first key must agree on which one won.
// Overwriting instead of linking would give each of them a different key and
// silently strand whichever seat the loser already took.
func TestConcurrentPublicationAgreesOnOneKey(t *testing.T) {
	root := t.TempDir()
	const racers = 8
	keys := make([]ed25519.PrivateKey, racers)
	errs := make([]error, racers)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for index := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			// Opening the store inside the race is the point: two fresh
			// processes both find the directory absent and both try to make
			// it, and only one can win.
			store, err := managedStoreOrError(root)
			if err != nil {
				errs[index] = err
				return
			}
			defer store.Close()
			keys[index], errs[index] = ensureKey(store)
		}()
	}
	start.Done()
	done.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", index, err)
		}
	}
	for index := 1; index < racers; index++ {
		if !bytes.Equal(keys[index], keys[0]) {
			t.Fatalf("racer %d published a different key than racer 0", index)
		}
	}
}

func TestNamedKeyPathIsAllowedOutsideAnyRepository(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "player.key")
	if _, err := ensureKey(namedStore(t, outside)); err != nil {
		t.Fatalf("a named path outside any repository was refused: %v", err)
	}
	loose := filepath.Join(t.TempDir(), "loose.key")
	writeKey(t, loose, 0o644)
	if _, err := ensureKey(namedStore(t, loose)); err == nil || !strings.Contains(err.Error(), "mode 0400 or 0600") {
		t.Fatalf("a named path skipped the permission check: %v", err)
	}
}

func TestSecretComesFromAFileOrStandardInputAndIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("one use\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecret(path, nil); err != nil || got != "one use" {
		t.Fatalf("readSecret(file) = %q, %v", got, err)
	}
	if got, err := readSecret("-", strings.NewReader("from stdin\r\n")); err != nil || got != "from stdin" {
		t.Fatalf("readSecret(stdin) = %q, %v", got, err)
	}
	if got, err := readSecret("", nil); err != nil || got != "" {
		t.Fatalf("readSecret(unset) = %q, %v", got, err)
	}
	oversized := strings.Repeat("x", maxSecretBytes+1)
	if _, err := readSecret("-", strings.NewReader(oversized)); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("an oversized secret was accepted: %v", err)
	}
	if _, err := readSecret(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("a missing secret file was accepted")
	}
}

// Naming a source and getting nothing back must fail. Returning an empty
// secret would turn a typo into an invitation anybody can accept.
func TestAnExplicitlyEmptySecretFailsClosed(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(empty, nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty secret file was accepted: %v", err)
	}
	newlines := filepath.Join(t.TempDir(), "newlines")
	if err := os.WriteFile(newlines, []byte("\r\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(newlines, nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("a newline-only secret file was accepted: %v", err)
	}
	if _, err := readSecret("-", strings.NewReader("\n")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty standard input was accepted: %v", err)
	}
}

// The old flags carried the secret in argv, where every account on the machine
// could read it. They must not quietly come back.
func TestSecretsAreNotAcceptedAsCommandArguments(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "data")
	var output bytes.Buffer
	if err := run(ctx, []string{"init", "--repo", repo}, &output, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"create", "--repo", repo, "--color", "white", "--join-secret", "in argv"},
		{"join", "--repo", repo, "--game", "whatever", "--secret", "in argv"},
	} {
		err := run(ctx, arguments, &bytes.Buffer{}, bytes.NewReader(nil))
		if err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Fatalf("%v was accepted: %v", arguments, err)
		}
	}
}

// The secret must actually arrive when it comes from standard input, not only
// when it comes from a file.
func TestSecretReachesTheGameFromStandardInput(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "data")
	var initialized bytes.Buffer
	if err := run(ctx, []string{"init", "--repo", repo}, &initialized, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	var created bytes.Buffer
	arguments := []string{"create", "--repo", repo, "--color", "white", "--join-secret-file", "-"}
	if err := run(ctx, arguments, &created, strings.NewReader("through stdin\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.String(), "through+stdin") {
		t.Fatalf("the invitation does not carry the secret from stdin: %s", created.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(created.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	game, ok := decoded["game"].(string)
	if !ok || game == "" {
		t.Fatalf("create output = %+v", decoded)
	}
	// Joining from standard input as well, because covering only create would
	// let a mutation that drops stdin from runJoin pass.
	opponent := filepath.Join(t.TempDir(), "opponent.key")
	var joined bytes.Buffer
	join := []string{"join", "--repo", repo, "--key", opponent, "--game", game, "--secret-file", "-"}
	if err := run(ctx, join, &joined, strings.NewReader("through stdin\n")); err != nil {
		t.Fatal(err)
	}
	var accepted map[string]any
	if err := json.Unmarshal(joined.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["effective"] != true {
		t.Fatalf("the join from stdin was not effective: %+v", accepted)
	}
}

// A directory symlink that stays inside the root is the case os.Root does not
// refuse on its own: measured on Go 1.26, Root.OpenRoot follows it.
func TestKeyDirectoryThatIsAnInRootSymbolicLinkIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(root, "chess")); err != nil {
		t.Fatal(err)
	}
	if _, err := managedStoreOrError(root); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("an in-root directory symlink was accepted: %v", err)
	}
}
