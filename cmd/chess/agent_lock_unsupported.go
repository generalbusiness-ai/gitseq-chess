//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import (
	"errors"
	"os"
)

func privateAgentFile(os.FileInfo) error {
	return errors.New("agent custody is unsupported on this platform")
}
func lockAgentFile(*os.File) error {
	return errors.New("agent custody is unsupported on this platform")
}

func openAgentFile(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, errors.New("agent custody is unsupported on this platform")
}
