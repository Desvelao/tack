// Package filestore holds the file-safety code shared by the backends that keep
// tasks in files: the cross-process lock, change stamps, atomic replacement, and
// Store, a whole-file engine driven by a Codec (json, jsonl, yaml).
package filestore

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// LockTimeout is how long a write waits for another tack holding the file lock.
const LockTimeout = 5 * time.Second

// hashWindow and hashMax bound the content hash: only files that are small and
// were modified very recently get one, so it stays cheap. It covers filesystems
// with a coarse mtime (1-2 s on FAT/exFAT, some NFS/SMB) where a same-size edit
// can keep the same modification time.
const (
	hashWindow = 2 * time.Second
	hashMax    = 256 << 10
)

// Stamp identifies a version of a file: modification time, size, inode and
// change time (an editor that replaces the file by rename changes the inode; an
// in-place write changes the ctime even when the mtime was restored) and, for a
// small file modified within the last couple of seconds, a hash of its content.
// Inode and ctime are zero where the platform does not expose them.
type Stamp struct {
	Mod   time.Time
	Size  int64
	Ino   uint64
	Ctime int64
	Hash  uint64
}

// Same reports whether b is the same version of the file as s. The content hash
// is taken only while a file is fresh (hashWindow), so a stamp taken right after
// a write has one and the same file stamped later does not: the hash decides
// only when both stamps have one.
func (s Stamp) Same(b Stamp) bool {
	if s.Hash != 0 && b.Hash != 0 && s.Hash != b.Hash {
		return false
	}
	s.Hash, b.Hash = 0, 0
	return s == b
}

// StatFile returns the current Stamp of path (following symlinks).
func StatFile(path string) (Stamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Stamp{}, err
	}
	return StampOf(path, st), nil
}

// StampOf builds the Stamp of path from an os.Stat result for it.
func StampOf(path string, st os.FileInfo) Stamp {
	s := Stamp{Mod: st.ModTime(), Size: st.Size()}
	s.Ino, s.Ctime = sysIDs(st)
	if st.Size() <= hashMax && time.Since(st.ModTime()) < hashWindow {
		if f, err := os.Open(path); err == nil {
			h := fnv.New64a()
			if _, err := io.Copy(h, f); err == nil {
				s.Hash = h.Sum64()
			}
			f.Close()
		}
	}
	return s
}

// createTemp makes the temporary file next to the target. Unlike os.CreateTemp
// (always 0600) it asks for 0666, so a file created for the first time gets the
// mode the umask gives any file, like the backlog itself.
func createTemp(dir string) (*os.File, error) {
	for i := 0; i < 100; i++ {
		name := filepath.Join(dir, fmt.Sprintf(".tack-%d.tmp", rand.Uint64()))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, fmt.Errorf("cannot create a temporary file in %s", dir)
}

// WriteAtomic replaces path with data through a temp file + rename in the same
// directory (writing through a symlink instead of replacing it). It returns the
// Stamp of the new file (content taken before the rename so that an external
// write landing afterwards differs from it and is noticed).
func WriteAtomic(path string, data []byte) (Stamp, error) {
	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real
	}
	dir := filepath.Dir(target)
	tmp, err := createTemp(dir)
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
		// Replacing a file keeps its mode and, as far as we are allowed to, its
		// owner and group (a backlog shared by users or containers).
		_ = os.Chmod(tmpName, st.Mode().Perm())
		copyOwner(tmpName, st)
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
	// The rename changes the ctime, so take the inode and ctime from the renamed
	// file; if something else already replaced it, keep the stamp as it is so the
	// difference is noticed.
	if after, err := StatFile(target); err == nil && after.Mod.Equal(cur.Mod) && after.Size == cur.Size {
		cur.Ino, cur.Ctime = after.Ino, after.Ctime
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
