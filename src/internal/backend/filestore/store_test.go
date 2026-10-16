package filestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tkan/internal/backend"
	"tkan/internal/task"
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
	if _, err := s.Create(context.Background(), task.Task{Title: "x", Status: "s"}); err == nil || !strings.Contains(err.Error(), "locked by another tkan") {
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
