package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeWatchConfig(t *testing.T, hooks string) (cfg, csv string) {
	t.Helper()
	dir := t.TempDir()
	csv = filepath.Join(dir, "tasks.csv")
	if err := os.WriteFile(csv, []byte("id,title,status\n1,Uno,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(dir, "tack.yaml")
	body := "backend:\n  - type: csv\n    options:\n      source: " + csv + "\n" + hooks
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg, csv
}

func TestWatchUsage(t *testing.T) {
	var out, errw bytes.Buffer
	if code := cmdWatch(context.Background(), nil, []string{"-h"}, &out, &errw); code != exitOK {
		t.Fatalf("-h: exit %d", code)
	}
	if code := cmdWatch(context.Background(), nil, []string{"extra"}, &out, &errw); code != exitUsage {
		t.Fatalf("extra arg: exit %d", code)
	}
}

func TestWatchNothingToDo(t *testing.T) {
	cfg, _ := writeWatchConfig(t, "")
	a, err := open(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	var out, errw bytes.Buffer
	if code := cmdWatch(context.Background(), a, nil, &out, &errw); code != exitError {
		t.Fatalf("exit %d, want %d (%q)", code, exitError, errw.String())
	}
}

// syncBuf is a buffer the watch loop writes while the test reads.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestWatchJSONEvents(t *testing.T) {
	cfg, csv := writeWatchConfig(t, "")
	a, err := open(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errw := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() { done <- cmdWatch(ctx, a, []string{"-json", "-interval", "20ms"}, out, errw) }()
	waitFor := func(want string) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if strings.Contains(out.String(), want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no %q in output:\n%s\nstderr: %s", want, out.String(), errw.String())
	}
	waitFor(`"event":"reload.ok"`)
	if strings.Contains(out.String(), "task.appeared") {
		t.Fatalf("first list fired appeared:\n%s", out.String())
	}
	if err := os.WriteFile(csv, []byte("id,title,status\n1,Uno,pending\n2,Dos,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(`"event":"task.appeared"`)
	if !strings.Contains(out.String(), `"source":"poll"`) || !strings.Contains(out.String(), "Dos") {
		t.Fatalf("appeared line wrong:\n%s", out.String())
	}
	if err := os.WriteFile(csv, []byte("id,title,status\n2,Dos,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(`"event":"task.disappeared"`)
	cancel()
	if code := <-done; code != exitInterrupted {
		t.Fatalf("exit %d, want %d", code, exitInterrupted)
	}
}

func TestWatchQueryNarrowsEvents(t *testing.T) {
	cfg, csv := writeWatchConfig(t, "")
	a, err := open(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	out, errw := &syncBuf{}, &syncBuf{}
	done := make(chan int, 1)
	go func() {
		done <- cmdWatch(ctx, a, []string{"-json", "-interval", "20ms", "-q", "title:Tres"}, out, errw)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(csv, []byte("id,title,status\n1,Uno,pending\n2,Dos,pending\n3,Tres,pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done
	got := out.String()
	if !strings.Contains(got, "Tres") || strings.Contains(got, "Dos") {
		t.Fatalf("filter not applied:\n%s", got)
	}
}

func TestWatchRejectsAnUnknownFilterField(t *testing.T) {
	cfg, _ := writeWatchConfig(t, "")
	a, err := open(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.be.Close()
	var out, errw bytes.Buffer
	if code := cmdWatch(context.Background(), a, []string{"-json", "-q", "prioity=high"}, &out, &errw); code != exitUsage || !strings.Contains(errw.String(), "prioity") {
		t.Fatalf("exit %d, want %d (%q)", code, exitUsage, errw.String())
	}
}
