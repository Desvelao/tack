package filestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tack/internal/backend"
	"tack/internal/task"
)

// lineCodec stores "id|title|status" per line.
type lineCodec struct{}

func (lineCodec) Decode(data []byte) ([]task.Task, error) {
	var out []task.Task
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		p := strings.Split(l, "|")
		out = append(out, task.Task{ID: p[0], Title: p[1], Status: p[2]})
	}
	return out, nil
}

func (lineCodec) Encode(ts []task.Task, _ []byte) ([]byte, error) {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(t.ID + "|" + t.Title + "|" + t.Status + "\n")
	}
	return []byte(b.String()), nil
}

func newStore(t *testing.T, content string, poll backend.PollMode) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore("line", p, poll, lineCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func touch(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(p, future, future)
}

func TestLockTimeoutAndWait(t *testing.T) {
	s, p := newStore(t, "1|a|s\n", backend.PollMode{})
	s.LockTimeout = 150 * time.Millisecond
	release, err := LockFile(p+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err == nil || !strings.Contains(err.Error(), "locked by another tack") {
		t.Fatalf("got %v", err)
	}
	release()
	if _, err := s.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedWriteRollsBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, p := newStore(t, "1|a|s\n", backend.PollMode{})
	_ = os.WriteFile(p+".lock", nil, 0o644) // lock file exists, so only the temp file fails
	dir := filepath.Dir(p)
	_ = os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if _, err := s.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	got, _ := s.List(context.Background())
	if len(got) != 1 {
		t.Fatalf("unsaved task visible: %+v", got)
	}
}

func TestPollNoneServesCacheUntilReload(t *testing.T) {
	s, p := newStore(t, "1|a|s\n", backend.PollMode{Kind: backend.PollNone})
	touch(t, p, "1|a|s\n2|b|s\n")
	if ch, _ := s.Changed(); ch {
		t.Fatal("poll none reports changes")
	}
	if got, _ := s.List(context.Background()); len(got) != 1 {
		t.Fatalf("cache not served: %+v", got)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(context.Background()); len(got) != 2 {
		t.Fatalf("reload ignored: %+v", got)
	}
}

func TestPollEveryThrottles(t *testing.T) {
	s, p := newStore(t, "1|a|s\n", backend.PollMode{Kind: backend.PollEvery, Interval: 10 * time.Second})
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	if ch, _ := s.Changed(); ch {
		t.Fatal("unchanged file reported")
	}
	touch(t, p, "1|a|s\n2|b|s\n")
	if ch, _ := s.Changed(); ch {
		t.Fatal("checked again before the interval")
	}
	now = now.Add(11 * time.Second)
	if ch, _ := s.Changed(); !ch {
		t.Fatal("change not noticed after the interval")
	}
	if got, _ := s.List(context.Background()); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestEmptyIDRejectedAndSymlinkKept(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	_ = os.WriteFile(real, []byte("1|a|s\n"), 0o640)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks")
	}
	s, err := NewStore("line", link, backend.PollMode{}, lineCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), task.Task{Title: "b", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "2|b|s") {
		t.Fatalf("target not updated: %q", data)
	}
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	touch(t, real, "|a|s\n")
	if _, err := s.List(context.Background()); err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("got %v", err)
	}
}

// flock is tied to the process, so the lock file a crashed tack leaves behind
// holds nothing and the next writer is not kept out.
func TestStaleLockFileDoesNotBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(p, []byte("pid 4242 died here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockFile(p, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("a leftover lock file blocked: %v", err)
	}
	// While held, a second taker times out; after the release it gets in.
	if _, err := LockFile(p, 100*time.Millisecond); err == nil {
		t.Fatal("a held lock was taken twice")
	}
	unlock()
	again, err := LockFile(p, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestStampHashesOnlySmallRecentFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("todo"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := StatFile(p)
	if err != nil || a.Hash == 0 {
		t.Fatalf("recent small file: %+v, %v; want a hash", a, err)
	}
	st, _ := os.Stat(p)
	if err := os.WriteFile(p, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, st.ModTime(), st.ModTime())
	b, _ := StatFile(p)
	if a == b || a.Hash == b.Hash {
		t.Fatalf("same-size edit with restored mtime not told apart: %+v vs %+v", a, b)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	if c, _ := StatFile(p); c.Hash != 0 {
		t.Fatalf("old file hashed: %+v", c)
	}
}

// Separate Stores on one file behave like separate processes: each has its own
// cache and they meet only at the lock file. No Create may be lost.
func TestConcurrentWritersLoseNothing(t *testing.T) {
	_, p := newStore(t, "", backend.PollMode{})
	const writers, each = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		s, err := NewStore("line", p, backend.PollMode{}, lineCodec{})
		if err != nil {
			t.Fatal(err)
		}
		s.LockTimeout = 30 * time.Second
		wg.Add(1)
		go func(w int, s *Store) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				title := string(rune('a'+w)) + string(rune('0'+i))
				if _, err := s.Create(context.Background(), task.Task{Title: title, Status: "s"}); err != nil {
					errs <- err
				}
			}
		}(w, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ids, titles := map[string]bool{}, map[string]bool{}
	ts, _ := lineCodec{}.Decode(data)
	for _, tk := range ts {
		ids[tk.ID], titles[tk.Title] = true, true
	}
	if len(ts) != writers*each || len(ids) != writers*each || len(titles) != writers*each {
		t.Fatalf("want %d distinct tasks, got %d rows, %d ids, %d titles", writers*each, len(ts), len(ids), len(titles))
	}
}

func TestWriteAtomicPreservesMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteAtomic(p, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("existing file mode became %v", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".tack-*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left behind: %v", m)
	}
}

func TestStampSameIgnoresAHashOnlyOneSideHas(t *testing.T) {
	fresh := Stamp{Mod: time.Unix(100, 0), Size: 5, Ino: 1, Ctime: 2, Hash: 42}
	aged := fresh
	aged.Hash = 0 // the same file stamped after the hash window
	if !fresh.Same(aged) || !aged.Same(fresh) {
		t.Error("a stamp that lost its hash to age is the same file")
	}
	other := fresh
	other.Hash = 43
	if fresh.Same(other) {
		t.Error("two different hashes are different content")
	}
	moved := aged
	moved.Size++
	if aged.Same(moved) || fresh.Same(moved) {
		t.Error("a different size is a different version")
	}
}

// Our own write must not look like an outside change once its hash is old.
func TestOwnWriteIsNotAChangeAfterTheHashWindow(t *testing.T) {
	s, _ := newStore(t, "1|a|s\n", backend.PollMode{})
	if _, err := s.Create(context.Background(), task.Task{Title: "b", Status: "s"}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := s.Changed(); ch {
		t.Fatal("own write reported as a change at once")
	}
	time.Sleep(hashWindow + 200*time.Millisecond)
	if ch, err := s.Changed(); ch || err != nil {
		t.Fatalf("own write reported as a change after %s: %v %v", hashWindow, ch, err)
	}
}

// A write waiting for another process's file lock must not stop List and
// Changed from answering.
func TestReadsDoNotWaitForAWriterBlockedOnTheFileLock(t *testing.T) {
	s, p := newStore(t, "1|a|s\n", backend.PollMode{})
	s.LockTimeout = 2 * time.Second
	release, err := LockFile(p+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Create(context.Background(), task.Task{Title: "x", Status: "s"})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // the writer is now waiting for the lock
	read := make(chan struct{})
	go func() {
		s.List(context.Background())
		s.Changed()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("List/Changed blocked behind a writer waiting for the file lock")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
