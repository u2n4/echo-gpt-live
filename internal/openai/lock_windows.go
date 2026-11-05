//go:build windows

package openai

import (
	"os"

	"golang.org/x/sys/windows"
)

// The service runs on Linux. These exist so the package builds and its tests run on the
// developer's Windows machine, with the same meaning: an exclusive lock on the whole file, and a
// liveness check by process id.

func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol)
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// pidAlive reports whether a process with this id exists. Opening it with the least right there
// is succeeds for any live process, including another user's; a dead id fails.
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	const stillActive = 259
	return code == stillActive
}
