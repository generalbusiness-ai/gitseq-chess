//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The mode passed to open is filtered by the umask. Only an explicit Chmod on
// the open descriptor makes every newly published key exactly 0600.
func TestPublishedKeyIsOwnerOnlyEvenUnderARestrictiveUmask(t *testing.T) {
	root := t.TempDir()
	store := managedStore(t, root)
	// 0177 would have been useless here: it clears bits 0600 does not have,
	// so the mode came out 0600 either way and the test could not fail. 0277
	// clears owner write, which is exactly what the explicit Chmod restores.
	previous := syscall.Umask(0o277)
	defer syscall.Umask(previous)
	if _, err := ensureKey(store); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(root, "chess", "player.key"))
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != keyFileMode {
		t.Fatalf("published key is mode %04o under umask 0277, want %04o", permission, keyFileMode)
	}
}
