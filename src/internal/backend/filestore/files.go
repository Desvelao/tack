// Package filestore holds the file-safety code shared by the backends that keep
// tasks in files: the cross-process lock, change stamps, atomic replacement, and
// Store, a whole-file engine driven by a Codec (json, jsonl, yaml).
package filestore

import (
	"os"
	"path/filepath"
	"time"
)

// LockTimeout is how long a write waits for another tkan holding the file lock.
const LockTimeout = 5 * time.Second

// Stamp identifies a version of a file (modification time and size).
type Stamp struct {
	Mod  time.Time
	Size int64
}

// StatFile returns the current Stamp of path.
func StatFile(path string) (Stamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Stamp{}, err
	}
	return Stamp{st.ModTime(), st.Size()}, nil
}

// WriteAtomic replaces path with data through a temp file + rename in the same
// directory (writing through a symlink instead of replacing it). It returns the
// Stamp of the new file, taken before the rename so that an external write
// landing afterwards differs from it and is noticed.
func WriteAtomic(path string, data []byte) (Stamp, error) {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tkan-*.tmp")
	if err != nil {
		return Stamp{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return Stamp{}, err
	}
	// Flush to disk before the rename so a power loss can't leave an empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return Stamp{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return Stamp{}, err
	}
	if st, err := os.Stat(target); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	}
	cur, err := StatFile(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return Stamp{}, err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return Stamp{}, err
	}
	SyncDir(dir)
	return cur, nil
}

// SyncDir makes a rename durable; best effort, some filesystems don't support it.
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
