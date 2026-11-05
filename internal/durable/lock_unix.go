//go:build darwin || linux

package durable

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockDir prevents two CLI processes from writing the same state directory.
// The lock file is deliberately never unlinked: its inode identifies the lock.
func LockDir(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state directory is already locked (%s): %w", dir, err)
	}
	return f, nil
}
