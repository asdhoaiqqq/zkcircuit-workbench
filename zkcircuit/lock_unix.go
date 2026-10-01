//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package zkcircuit

import (
	"os"
	"syscall"
)

// lockShared acquires an advisory shared lock on f. It blocks until granted.
func lockShared(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
}

// lockExclusive acquires an advisory exclusive lock on f. It blocks until
// granted; separate open file descriptions from two processes (or two
// handles in one process) mutually exclude each other.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockLock releases the advisory lock held on f. Closing f also releases it.
func unlockLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// syncDir flushes directory metadata so that a recently renamed data file is
// durable across a crash or power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
