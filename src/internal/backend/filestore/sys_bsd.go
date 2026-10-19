//go:build darwin || freebsd || netbsd

package filestore

import (
	"os"
	"syscall"
)

// sysIDs returns the inode and change time (ns) of a file.
func sysIDs(st os.FileInfo) (uint64, int64) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino), int64(s.Ctimespec.Sec)*1e9 + int64(s.Ctimespec.Nsec)
	}
	return 0, 0
}
