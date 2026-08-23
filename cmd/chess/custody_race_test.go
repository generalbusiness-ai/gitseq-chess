package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These witnesses reach guards that only fire when something moves underneath
// the program. Each one holds the answer an operation already got, changes what
// the name refers to, and requires the refusal. Racing goroutines would reach
// the same code only by luck, and a test that passes by luck is not coverage.

func TestManagedDirectoryWritableByOthersIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o770, 0o707, 0o720, 0o702} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "chess"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, "chess"), mode); err != nil {
			t.Fatal(err)
		}
		_, err := managedStoreOrError(root)
		if err == nil || !strings.Contains(err.Error(), "group or others can write") {
			t.Fatalf("mode %04o was accepted: %v", mode, err)
		}
	}
}

// Chmod does not change an inode's identity, so a directory made writable
// after the Lstat still satisfies SameFile. Only the open descriptor knows.
func TestManagedDirectoryMadeWritableAfterTheCheckIsRefused(t *testing.T) {
	root := t.TempDir()
	outer, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	if err := outer.Mkdir("chess", 0o700); err != nil {
		t.Fatal(err)
	}
	named, err := outer.Lstat("chess")
	if err != nil {
		t.Fatal(err)
	}
	// The same inode, now writable by everyone.
	if err := os.Chmod(filepath.Join(root, "chess"), 0o777); err != nil {
		t.Fatal(err)
	}
	opened, err := adoptDirectory(outer, "chess", named)
	if err == nil {
		opened.Close()
		t.Fatal("a directory made writable after the check was accepted")
	}
	if !strings.Contains(err.Error(), "group or others can write") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestManagedDirectoryReplacedAfterTheCheckIsRefused(t *testing.T) {
	root := t.TempDir()
	outer, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	if err := outer.Mkdir("chess", 0o700); err != nil {
		t.Fatal(err)
	}
	named, err := outer.Lstat("chess")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the checked inode live under another name while putting a different
	// inode at the checked name. This makes the mismatch deterministic even on
	// filesystems that may quickly reuse an inode after deletion.
	if err := os.Rename(filepath.Join(root, "chess"), filepath.Join(root, "checked-chess")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "chess"), 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := adoptDirectory(outer, "chess", named)
	if err == nil {
		opened.Close()
		t.Fatal("a replaced directory was accepted")
	}
	if !strings.Contains(err.Error(), "changed while it was being opened") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestKeyReplacedAfterTheCheckIsRefused(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	writeKey(t, filepath.Join(root, "chess", "player.key"), 0o600)
	named, err := store.root.Lstat(store.name)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the checked inode live under another name while putting a different
	// inode at the checked name, so inode reuse cannot make SameFile pass.
	if err := os.Rename(
		filepath.Join(root, "chess", "player.key"),
		filepath.Join(root, "chess", "checked-player.key"),
	); err != nil {
		t.Fatal(err)
	}
	writeKey(t, filepath.Join(root, "chess", "player.key"), 0o600)
	if _, err := readNamedKey(store, named); err == nil || !strings.Contains(err.Error(), "changed while it was being opened") {
		t.Fatalf("a replaced key file was accepted: %v", err)
	}
}

// Losing the creation race is ordinary. The directory another process made is
// checked exactly as our own would have been.
func TestConcurrentDirectoryCreationIsTolerated(t *testing.T) {
	root := t.TempDir()
	outer, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	losing := func(name string, mode os.FileMode) error {
		// Somebody else got there first, exactly as a real loser would see.
		if err := outer.Mkdir(name, mode); err != nil {
			return err
		}
		return os.ErrExist
	}
	opened, err := openNamedDirectory(outer, "chess", losing)
	if err != nil {
		t.Fatalf("losing the creation race was treated as failure: %v", err)
	}
	defer opened.Close()
	if _, err := os.Stat(filepath.Join(root, "chess")); err != nil {
		t.Fatal(err)
	}
}

// A directory the winner made group-writable is still refused, so tolerating
// the race does not tolerate what the race produced.
func TestConcurrentDirectoryCreationStillChecksWhatTheWinnerMade(t *testing.T) {
	root := t.TempDir()
	outer, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	losing := func(name string, mode os.FileMode) error {
		if err := outer.Mkdir(name, mode); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(root, name), 0o770); err != nil {
			return err
		}
		return os.ErrExist
	}
	opened, err := openNamedDirectory(outer, "chess", losing)
	if err == nil {
		opened.Close()
		t.Fatal("a group-writable directory made by the race winner was accepted")
	}
	if !strings.Contains(err.Error(), "group or others can write") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// Failing to remove the temporary name leaves a second copy of the private key
// on disk. That must be reported even though the key itself was published.
func TestCleanupFailureAfterASuccessfulLinkIsReported(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	store.link = func(from, to string) error {
		if err := store.root.Link(from, to); err != nil {
			return err
		}
		// Closing the root makes the deferred Remove fail deterministically,
		// the way a removal denied by the filesystem would.
		return store.root.Close()
	}
	_, err := ensureKey(store)
	if err == nil {
		t.Fatal("a failed cleanup was reported as success")
	}
	if !strings.Contains(err.Error(), "remove temporary player key") {
		t.Fatalf("cleanup failure was not named: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "chess", "player.key")); statErr != nil {
		t.Fatalf("the key was not published despite the link succeeding: %v", statErr)
	}
}

// The dangerous combination: the link loses the race AND cleanup fails. If the
// returned error still reads as ErrExist, ensureKey takes the winner fallback
// and the leftover private key is never mentioned.
func TestCleanupFailureIsNotHiddenByALostLinkRace(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	writeKey(t, filepath.Join(root, "chess", "player.key"), 0o600)
	store.link = func(from, to string) error {
		if err := store.root.Close(); err != nil {
			return err
		}
		return os.ErrExist
	}
	err := publishKey(store, make(ed25519.PrivateKey, ed25519.PrivateKeySize))
	if err == nil {
		t.Fatal("a lost race with a failed cleanup was reported as success")
	}
	if !strings.Contains(err.Error(), "remove temporary player key") {
		t.Fatalf("cleanup failure was not named: %v", err)
	}
	if errors.Is(err, os.ErrExist) {
		t.Fatal("the error still reads as ErrExist, so ensureKey would read the winner and hide the leftover key")
	}
}

func TestEnsureKeyStillReadsTheWinnerWhenCleanupSucceeds(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	var winner ed25519.PrivateKey
	store.link = func(from, to string) error {
		// The first read saw no destination. Materialize the winner exactly at
		// the publication boundary, then return the EEXIST a losing hard link
		// receives. This reaches ensureKey's fallback deterministically.
		winner = writeKey(t, filepath.Join(root, "chess", "player.key"), 0o600)
		return os.ErrExist
	}
	got, err := ensureKey(store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, winner) {
		t.Fatal("losing the link race did not return the winner's key")
	}
}
