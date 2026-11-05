//go:build unix

package openai

import (
	"errors"
	"os"
	"syscall"
)

// lockFile holds an exclusive advisory lock on f until unlockFile. Every process that opens the
// budget or the ledger goes through this, which is what makes two bridges (or a bridge and a
// diagnostic) on one machine agree on the number.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// pidAlive reports whether a process with this id exists. EPERM means it exists and belongs to
// someone else, which is still alive.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
