//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// writerOwnership is shared by linked worktrees and all supported writer
// entry points. The file stays in place; unlinking a held lock would let a
// second writer lock another inode. A process exit releases the OS lock.
type writerOwnership struct {
	file *os.File
	path string
}

func acquireWriter(ctx context.Context, repo string) (*writerOwnership, error) {
	dir, err := gitCommonDir(ctx, repo)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "chess-writer.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open writer ownership: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if created {
		if err := file.Chmod(0600); err != nil {
			file.Close()
			return nil, err
		}
	}
	owner := &writerOwnership{file: file, path: path}
	if err := owner.checkFile(); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another Chess writer owns this repository")
	}
	return owner, nil
}

func (o *writerOwnership) checkFile() error {
	if o == nil || o.file == nil {
		return errors.New("writer ownership is required")
	}
	info, err := o.file.Stat()
	if err != nil {
		return errors.New("writer ownership was lost")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return errors.New("writer ownership file must be private, regular and owned by this user")
	}
	current, err := os.Lstat(o.path)
	if err != nil || !os.SameFile(info, current) {
		return errors.New("writer ownership file was replaced")
	}
	return nil
}

func (o *writerOwnership) Close() error {
	if o == nil || o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	return err
}
