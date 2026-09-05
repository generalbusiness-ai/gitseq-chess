//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import (
	"context"
	"errors"
)

type writerOwnership struct{}

func acquireWriter(context.Context, string) (*writerOwnership, error) {
	return nil, errors.New("Chess writer ownership is unsupported on this platform")
}
func (*writerOwnership) checkFile() error { return errors.New("writer ownership is unavailable") }
func (*writerOwnership) Close() error     { return nil }
