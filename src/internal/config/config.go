// Package config loads tkan.yaml: the list of backends (type, name, options)
// and how raw statuses map to board columns.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/board"
	"tkan/internal/query"
)

// DefaultFile is looked up in the working directory when no config is given.
const DefaultFile = "tkan.yaml"

// ConfigNames are the file names Locate accepts in each directory, in priority order.
var ConfigNames = []string{".tkan.yaml", ".tkan.yml", DefaultFile, "tkan.yml"}

// HomeConfigNames are the names looked up in the home directory.
var HomeConfigNames = []string{".tkan.yaml", ".tkan.yml"}

// Entry is one item of the `backend:` list.
type Entry struct {
	Type    string    `yaml:"type"`
	Name    string    `yaml:"name"`    // optional; default "<type><position>", e.g. csv1
	Options yaml.Node `yaml:"options"` // backend-specific settings
}

// Config is the parsed tkan.yaml.
type Config struct {
	Backends []Entry    `yaml:"backend"`
	Board    board.Spec `yaml:"board"`
	// DefaultBackend names the backend used when a command gets no -b (and
	// `add` creates there); $TKAN_BACKEND overrides it. Default: none, i.e.
	// the first backend for add and a search through all for IDs.
	DefaultBackend string `yaml:"default_backend,omitempty"`

	Path string `yaml:"-"` // absolute path of the file it was loaded from
}

// Load reads and validates a config file. Unknown keys are rejected, so files
// in the old single-backend format (backend: csv, source:, csv:) fail loudly.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		if isLegacyFormat(err) {
			return nil, fmt.Errorf("%s: %w (expected a `backend:` list of {type, name, options}; see docs/config.md)", path, err)
		}
		return nil, fmt.Errorf("%s: %s", path, explainUnknownKeys(err.Error()))
	}
	if err := c.Board.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, v := range c.Board.Views { // syntax now; field names are checked when the view is used
		if _, err := query.Parse(v.Query); err != nil {
			return nil, fmt.Errorf("%s: board.views %q: %w", path, v.Name, err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	c.Path = abs
	if err := c.nameEntries(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.DefaultBackend != "" {
		found := false
		var names []string
		for _, e := range c.Backends {
			names = append(names, e.Name)
			found = found || strings.EqualFold(e.Name, strings.TrimSpace(c.DefaultBackend))
		}
		if !found {
			return nil, fmt.Errorf("%s: default_backend %q is not one of the configured backends (%s)", path, c.DefaultBackend, strings.Join(names, ", "))
		}
	}
	return &c, nil
}

// isLegacyFormat reports whether a decode error comes from the old
// single-backend keys (source:, csv:, or `backend:` as a plain string).
func isLegacyFormat(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "field source not found") ||
		strings.Contains(msg, "field csv not found") ||
		strings.Contains(msg, "into []config.Entry")
}

// nameEntries validates the entries and fills in generated names.
func (c *Config) nameEntries() error {
	seen := map[string]bool{}
	for i := range c.Backends {
		e := &c.Backends[i]
		e.Type = strings.ToLower(strings.TrimSpace(e.Type))
		if e.Type == "" {
			return fmt.Errorf("backend[%d]: type is required (supported: %s)", i+1, strings.Join(backend.Supported(), ", "))
		}
		e.Name = strings.TrimSpace(e.Name)
		if e.Name == "" {
			e.Name = e.Type + strconv.Itoa(i+1)
		}
		if bad := strings.IndexFunc(e.Name, func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-')
		}); bad >= 0 {
			return fmt.Errorf("backend[%d]: name %q may only contain letters, digits, '_', '.' and '-' (it is used as NAME:ID and in -b NAME)", i+1, e.Name)
		}
		key := strings.ToLower(e.Name)
		if seen[key] {
			return fmt.Errorf("backend[%d]: duplicate name %q", i+1, e.Name)
		}
		seen[key] = true
	}
	return nil
}

// Dir is the directory relative paths in the config resolve against.
func (c *Config) Dir() string { return filepath.Dir(c.Path) }

// Locate picks the config file: the explicit path (-c / $TKAN_CONFIG), else
// the first of ConfigNames in the working directory, else ~/.tkan.yaml or
// ~/.tkan.yml (HomeConfigNames).
func Locate(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p, ok := findIn(".", ConfigNames); ok {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p, ok := findIn(home, HomeConfigNames); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config: pass -c FILE, set $TKAN_CONFIG, or create %s (or %s) in this directory, or ~/%s (or ~/%s) (try `tkan init`)", ConfigNames[0], strings.Join(ConfigNames[1:], ", "), HomeConfigNames[0], HomeConfigNames[1])
}

// findIn returns the first existing file of names in dir.
// For dir ".", the path is the bare file name.
func findIn(dir string, names []string) (string, bool) {
	for _, name := range names {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// Open opens every configured backend as one Set.
func (c *Config) Open() (*backend.Set, error) {
	if len(c.Backends) == 0 {
		return nil, fmt.Errorf("%s: no backends configured (add a `backend:` list; see docs/config.md)", c.Path)
	}
	var items []backend.Named
	for _, e := range c.Backends {
		be, err := backend.OpenSpec(backend.Spec{Type: e.Type, Name: e.Name, Options: e.Options, BaseDir: c.Dir()})
		if err != nil {
			for _, it := range items {
				it.Backend.Close()
			}
			return nil, fmt.Errorf("backend %s: %w", e.Name, err)
		}
		items = append(items, backend.Named{Name: e.Name, Type: e.Type, Backend: be})
	}
	return backend.NewSet(items), nil
}
