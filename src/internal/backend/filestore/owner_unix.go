//go:build !windows

package filestore

import (
	"os"
	"syscall"
)

// copyOwner gives path the owner and group of st, as far as the process may:
// only root can change the owner, so without that right the group alone is set.
// Best effort; a failure leaves the file with ours.
func copyOwner(path string, st os.FileInfo) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if os.Chown(path, int(s.Uid), int(s.Gid)) != nil {
		_ = os.Chown(path, -1, int(s.Gid))
	}
}
