//go:build !windows

package csvbackend

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive advisory lock on path (created if missing) and
// returns the function that releases it. The lock lives on a sidecar file
// because the data file itself is replaced by rename on every write. If another
// process holds it for longer than timeout the call fails instead of hanging.
func lockFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("locked by another tkan for more than %s (%s)", timeout, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
