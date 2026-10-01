//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package zkcircuit

import "os"

// On platforms without flock(2) the workbench still runs; concurrent
// multi-process access is only guaranteed on Unix-like systems. In-process
// access remains serialized by the Store mutex.

func lockShared(f *os.File) error    { return nil }
func lockExclusive(f *os.File) error { return nil }
func unlockLock(f *os.File) error    { return nil }

func syncDir(dir string) error { return nil }
