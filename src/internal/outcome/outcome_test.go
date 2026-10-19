package outcome

import (
	"strings"
	"testing"

	"tkan/internal/action"
	"tkan/internal/actionlog"
)

func TestNotesBlockSources(t *testing.T) {
	code := 3
	rec := actionlog.Record{RunID: "r1", Result: actionlog.ResultError, Error: "exit status 3", Stdout: "out text", Stderr: "err text", ExitCode: &code}
	s := action.Spec{Name: "x"}
	for src, want := range map[string]string{
		action.NotesStdout:  "out text",
		action.NotesError:   "exit status 3",
		action.NotesSummary: "exit 3",
	} {
		if b := notesBlock(s, rec, src); !strings.HasPrefix(b, "[run r1 x error exit 3]") || !strings.Contains(b, want) {
			t.Errorf("%s: %q", src, b)
		}
	}
	rec.Error = ""
	if b := notesBlock(s, rec, action.NotesError); !strings.Contains(b, "err text") {
		t.Errorf("error falls back to stderr: %q", b)
	}
	s.CaptureMax = 1
	rec.Stdout = strings.Repeat("é", 2000)
	if b := notesBlock(s, rec, action.NotesStdout); !strings.HasSuffix(b, "[output cut]") || len(b) > 1200 {
		t.Errorf("cap: %d bytes", len(b))
	}
}

func TestAppendBlock(t *testing.T) {
	if got := AppendBlock("a\n", "b"); got != "a\n\nb" {
		t.Errorf("%q", got)
	}
	if got := AppendBlock(" ", "b"); got != "b" {
		t.Errorf("%q", got)
	}
}
