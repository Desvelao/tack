package proc

import "testing"

func TestOutputIsBounded(t *testing.T) {
	w := &CapWriter{Max: 10}
	n, err := w.Write([]byte("0123456789abcdef"))
	if n != 16 || err != nil || w.Buf.String() != "0123456789" || !w.Truncated {
		t.Fatalf("n=%d err=%v buf=%q truncated=%v", n, err, w.Buf.String(), w.Truncated)
	}
	if n, _ := w.Write([]byte("more")); n != 4 || w.Buf.Len() != 10 {
		t.Fatalf("after cap: n=%d len=%d", n, w.Buf.Len())
	}
}

func TestLiveWriterReadsWhileWriting(t *testing.T) {
	w := NewLiveWriter(1000)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			w.Write([]byte("0123456789"))
		}
	}()
	for i := 0; i < 200; i++ {
		w.String()
	}
	<-done
	s, trunc := w.String()
	if len(s) != 1000 || !trunc {
		t.Fatalf("len=%d truncated=%v", len(s), trunc)
	}
}
