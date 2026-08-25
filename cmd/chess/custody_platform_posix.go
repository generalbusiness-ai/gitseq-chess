//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

// requireKeyCustodyPlatform is the shared boundary for every player-key path.
// These targets provide the POSIX owner-mode semantics the custody contract
// checks and documents.
func requireKeyCustodyPlatform() error {
	return nil
}
