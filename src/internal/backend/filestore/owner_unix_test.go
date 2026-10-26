//go:build !windows

package filestore

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A file WriteAtomic creates gets the mode any new file gets (0666 less the
// umask), not the 0600 of a bare temporary file.
func TestWriteAtomicNewFileHonoursUmask(t *testing.T) {
	for _, umask := range []int{0o022, 0o077} {
		old := syscall.Umask(umask)
		q := filepath.Join(t.TempDir(), "new.txt")
		_, err := WriteAtomic(q, []byte("x\n"))
		syscall.Umask(old)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(q)
		if want := os.FileMode(0o666 &^ umask); st.Mode().Perm() != want {
			t.Errorf("umask %04o: new file mode %v, want %v", umask, st.Mode().Perm(), want)
		}
	}
}

// Replacing a file keeps the group it has (the owner too when running as root).
func TestWriteAtomicKeepsTheGroupOfAnExistingFile(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil || len(groups) < 2 {
		t.Skip("needs a second group to give the file")
	}
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("old\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getgid() {
			other = g
			break
		}
	}
	if err := os.Chown(p, -1, other); err != nil {
		t.Skip("cannot change the group:", err)
	}
	if _, err := WriteAtomic(p, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if gid := int(st.Sys().(*syscall.Stat_t).Gid); gid != other {
		t.Errorf("group became %d, want %d", gid, other)
	}
}
