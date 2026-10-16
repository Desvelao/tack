//go:build windows

package csvbackend

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive LockFileEx lock on the sidecar file path (created
// if missing); it fails after timeout if another process keeps it. Compile-
// checked only: it has not been run on Windows.
func lockFile(path string, timeout time.Duration) (func(), error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			break
		}
		if err != windows.ERROR_LOCK_VIOLATION && err != windows.ERROR_IO_PENDING {
			windows.CloseHandle(h)
			return nil, err
		}
		if time.Now().After(deadline) {
			windows.CloseHandle(h)
			return nil, fmt.Errorf("locked by another tkan for more than %s (%s)", timeout, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		windows.CloseHandle(h)
	}, nil
}
