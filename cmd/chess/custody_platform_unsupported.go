//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package main

import (
	"fmt"
	"runtime"
)

func requireKeyCustodyPlatform() error {
	return fmt.Errorf(
		"player-key custody is unsupported on %s: POSIX owner-mode semantics are required for managed and --key storage",
		runtime.GOOS,
	)
}
