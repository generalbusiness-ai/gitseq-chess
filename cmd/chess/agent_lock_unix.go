//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"errors"
	"os"
	"syscall"
)

func privateAgentFile(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400) || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return errors.New("agent file must be private, regular, singly linked and owned by this user")
	}
	return nil
}

func lockAgentFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another agent command is using this key; retry after it finishes")
	}
	return nil
}

func openAgentFile(root *os.Root, name string, flag int, mode os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
}
