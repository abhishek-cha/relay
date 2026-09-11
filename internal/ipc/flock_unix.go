//go:build unix

package ipc

import (
	"errors"
	"os"
	"syscall"
)

// flock takes an exclusive, non-blocking advisory lock.
func flock(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrLocked
	}
	return err
}

// funlock releases the advisory lock.
func funlock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
