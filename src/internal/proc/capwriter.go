// Package proc holds what the packages that run external commands share: a
// capped output buffer and process-group handling for timeouts.
package proc

import (
	"bytes"
	"sync"
)

// CapWriter keeps at most Max bytes and silently drops the rest, so a runaway
// command cannot exhaust memory (writes never fail, the command keeps running
// until it exits or times out).
type CapWriter struct {
	Buf       bytes.Buffer
	Max       int
	Truncated bool
}

func (w *CapWriter) Write(p []byte) (int, error) {
	n := len(p) // always report everything as written, or exec fails with a short write
	if room := w.Max - w.Buf.Len(); len(p) > room {
		w.Truncated = true
		p = p[:max(room, 0)]
	}
	w.Buf.Write(p)
	return n, nil
}

// LiveWriter is a CapWriter that can be read while the command is still
// writing (the board shows a running action's output as it grows).
type LiveWriter struct {
	mu sync.Mutex
	w  CapWriter
}

// NewLiveWriter keeps at most max bytes.
func NewLiveWriter(max int) *LiveWriter { return &LiveWriter{w: CapWriter{Max: max}} }

func (l *LiveWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// String is what was kept so far; truncated says whether more was dropped.
func (l *LiveWriter) String() (s string, truncated bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Buf.String(), l.w.Truncated
}
