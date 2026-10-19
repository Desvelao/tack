//go:build !linux && !darwin && !freebsd && !netbsd

package filestore

import "os"

// sysIDs: no inode or change time on this platform; mtime, size and the content
// hash still apply.
func sysIDs(os.FileInfo) (uint64, int64) { return 0, 0 }
