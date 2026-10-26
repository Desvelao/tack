package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverlayLoadsAndWrapsTheBackend(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.csv", "id,title,status\n1,One,todo\n")
	write(t, dir, "b.csv", "id,title,status\n1,Two,todo\n")
	p := write(t, dir, "tack.yaml", `
backend:
  - {type: csv, name: a, options: {source: a.csv}, overlay: claims}
  - type: csv
    name: b
    options: {source: b.csv}
    overlay: {file: sub/b.json, fields: [Priority]}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Backends[0].Overlay.Fields; strings.Join(got, ",") != "assignee,lease,claimed_at" {
		t.Fatalf("claims shorthand: %v", got)
	}
	if got := c.Backends[1].Overlay.Fields; strings.Join(got, ",") != "priority" {
		t.Fatalf("fields are lower-cased: %v", got)
	}
	set, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if !set.Stores("a", "assignee") || !set.Stores("b", "priority") {
		t.Fatal("overlaid fields must count as stored")
	}
	if set.Stores("b", "assignee") {
		t.Fatal("b has no assignee column and no overlay for it")
	}
	if want := filepath.Join(dir, ".tack", "overlay-a.json"); c.Backends[0].Overlay.path(dir, "a") != want {
		t.Fatalf("default path %q", c.Backends[0].Overlay.path(dir, "a"))
	}
	if want := filepath.Join(dir, "sub", "b.json"); c.Backends[1].Overlay.path(dir, "b") != want {
		t.Fatalf("path %q", c.Backends[1].Overlay.path(dir, "b"))
	}
	_ = os.Remove(p)
}

func TestOverlayRejectsBadSpecs(t *testing.T) {
	for name, y := range map[string]string{
		"unknown shorthand": "overlay: everything",
		"unknown field":     "overlay: {fields: [nope]}",
		"the id":            "overlay: {fields: [id]}",
		"no fields":         "overlay: {file: x.json}",
		"unknown key":       "overlay: {fields: [priority], flie: x}",
	} {
		p := write(t, t.TempDir(), "tack.yaml", "backend:\n  - {type: csv, options: {source: a.csv}, "+y+"}\n")
		if _, err := Load(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
