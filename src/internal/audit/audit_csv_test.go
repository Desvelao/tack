package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Several processes (the board and an agent's commands) starting on a missing
// file at once must leave exactly one header row.
func TestCSVHeaderIsWrittenOnceByConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.csv")
	const writers, each = 6, 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		l, err := New(path) // separate handles: separate mutexes, like separate processes
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				l.Record(Entry{Op: OpCreate, ID: "1", Details: "x"})
			}
			if err := l.Err(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	headers := 0
	for _, ln := range lines {
		if strings.HasPrefix(ln, "time,session,") {
			headers++
		}
	}
	if headers != 1 || !strings.HasPrefix(lines[0], "time,session,") || len(lines) != 1+writers*each {
		t.Fatalf("%d header rows, %d lines (want 1 and %d):\n%s", headers, len(lines), 1+writers*each, data)
	}
}

// A field cannot be added to the filter without saying what it is.
func TestEveryAuditFieldIsDocumented(t *testing.T) {
	if len(FieldDocs) != len(Fields) {
		t.Fatalf("%d docs for %d fields", len(FieldDocs), len(Fields))
	}
	for i, f := range Fields {
		if FieldDocs[i].Name != f || strings.TrimSpace(FieldDocs[i].Desc) == "" {
			t.Errorf("field %q: doc %+v", f, FieldDocs[i])
		}
	}
}
