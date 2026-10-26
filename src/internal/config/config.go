// Package config loads tack.yaml: the list of backends (type, name, options)
// and how raw statuses map to board columns.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"tack/internal/action"
	"tack/internal/actionlog"
	"tack/internal/audit"
	"tack/internal/backend"
	"tack/internal/backend/overlay"
	"tack/internal/board"
	"tack/internal/hook"
	"tack/internal/query"
)

// DefaultFile is looked up in the working directory when no config is given.
const DefaultFile = "tack.yaml"

// ConfigNames are the file names Locate accepts in each directory, in priority order.
var ConfigNames = []string{".tack.yaml", ".tack.yml", DefaultFile, "tack.yml"}

// HomeConfigNames are the names looked up in the home directory.
var HomeConfigNames = []string{".tack.yaml", ".tack.yml"}

// Entry is one item of the `backend:` list.
type Entry struct {
	Type    string    `yaml:"type"`
	Name    string    `yaml:"name"`    // optional; default "<type><position>", e.g. csv1
	Command []string  `yaml:"command"` // external command: implies type custom
	Options yaml.Node `yaml:"options"` // backend-specific settings
	// Disabled keeps the entry in the file but skips it: it is validated, not
	// opened, and its actions and hooks are ignored.
	Disabled bool `yaml:"disabled,omitempty"`
	// Overlay keeps some task fields (`claims`: assignee, lease, claimed_at) in
	// a local file instead of the store, for a store that cannot hold them.
	Overlay *Overlay `yaml:"overlay,omitempty"`
	// Actions and Hooks apply to this backend's tasks only, besides the
	// top-level ones; one with the name of a top-level one replaces it here.
	Actions []action.Spec `yaml:"actions,omitempty"`
	Hooks   []hook.Spec   `yaml:"hooks,omitempty"`
}

// Config is the parsed tack.yaml.
type Config struct {
	Backends []Entry    `yaml:"backend"`
	Board    board.Spec `yaml:"board"`
	// DefaultBackend names the backend used when a command gets no -b (and
	// `add` creates there); $TACK_BACKEND overrides it. Default: none, i.e.
	// the first backend for add and a search through all for IDs.
	DefaultBackend string `yaml:"default_backend,omitempty"`
	// Audit configures the audit trail of reloads and writes.
	Audit AuditSpec `yaml:"audit"`
	// Actions are commands the TUI picker and `tack run` start on a task
	// (every backend; see also Entry.Actions and AllActions).
	Actions []action.Spec `yaml:"actions"`
	// ActionMaxParallel is how many runs go at once when the board starts an
	// action on its marked cards (0 = action.DefaultMaxParallel).
	ActionMaxParallel int `yaml:"action_max_parallel,omitempty"`
	// Hooks are commands started by events (task written, reload, action
	// ended); see internal/hook. Entry.Hooks and AllHooks as for Actions.
	Hooks []hook.Spec `yaml:"hooks"`
	// HookMaxDepth is how deep hooks may chain: 1 (default) means a tack
	// started by a hook fires nothing; at most hook.HardMaxDepth.
	HookMaxDepth int `yaml:"hook_max_depth,omitempty"`
	// HookMaxParallel caps the hook runs at once over all hooks (0 = unlimited,
	// at most hook.MaxGlobalParallel); a hook's own max_parallel still applies.
	HookMaxParallel int `yaml:"hook_max_parallel,omitempty"`
	// ActionLog stores the details of every action run (command, stdin,
	// output, times). Disabled unless set; the audit trail keeps a summary only.
	ActionLog actionlog.Spec `yaml:"action_log"`

	Path string `yaml:"-"` // absolute path of the file it was loaded from

	allActions []action.Spec
	allHooks   []hook.Spec
}

// AllActions are the top-level actions followed by those of each backend
// (Backend set); action.For picks the ones of a task.
func (c *Config) AllActions() []action.Spec { return c.allActions }

// AllHooks are the top-level hooks followed by those of each backend (Backend set).
func (c *Config) AllHooks() []hook.Spec { return c.allHooks }

// AuditSpec is the `audit:` section. Without a file the trail lives in memory
// and ends with the run.
type AuditSpec struct {
	File    string `yaml:"file,omitempty"`     // .jsonl or .csv, relative to the config file
	MaxSize int    `yaml:"max_size,omitempty"` // KiB; past it the file is moved to FILE.1 (0 = grow forever)
}

// AuditPath is the absolute audit file ("" = in memory only).
func (c *Config) AuditPath() string {
	f := strings.TrimSpace(c.Audit.File)
	if f == "" || filepath.IsAbs(f) {
		return f
	}
	return filepath.Join(c.Dir(), f)
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
	if err := action.ValidateAll(c.Actions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.scopeEntries(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := action.CheckMaxParallel(c.ActionMaxParallel); err != nil {
		return nil, fmt.Errorf("%s: action_max_parallel: %w", path, err)
	}
	if err := hook.CheckMaxDepth(c.HookMaxDepth); err != nil {
		return nil, fmt.Errorf("%s: hook_max_depth: %w", path, err)
	}
	if err := hook.CheckMaxParallel(c.HookMaxParallel); err != nil {
		return nil, fmt.Errorf("%s: hook_max_parallel: %w", path, err)
	}
	if err := hook.ValidateAll(c.Hooks); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.allHooks = append(slices.Clone(c.Hooks), c.allHooks...)
	if _, err := actionlog.New(c.ActionLog, c.Dir(), ""); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := audit.New(c.AuditPath()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.DefaultBackend != "" {
		found := false
		var names []string
		for _, e := range c.Backends {
			if e.Disabled {
				if strings.EqualFold(e.Name, strings.TrimSpace(c.DefaultBackend)) {
					return nil, fmt.Errorf("%s: default_backend %q is disabled", path, c.DefaultBackend)
				}
				continue
			}
			names = append(names, e.Name)
			found = found || strings.EqualFold(e.Name, strings.TrimSpace(c.DefaultBackend))
		}
		if !found {
			return nil, fmt.Errorf("%s: default_backend %q is not one of the configured backends (%s)", path, c.DefaultBackend, strings.Join(names, ", "))
		}
	}
	return &c, nil
}

// nameEntries validates the entries and fills in generated names.
func (c *Config) nameEntries() error {
	seen := map[string]bool{}
	for i := range c.Backends {
		e := &c.Backends[i]
		e.Type = strings.ToLower(strings.TrimSpace(e.Type))
		if len(e.Command) > 0 {
			if e.Type == "" {
				e.Type = "custom"
			}
			if e.Type != "custom" {
				return fmt.Errorf("backend[%d]: command is only for the custom backend, not type %q", i+1, e.Type)
			}
		}
		if e.Type == "" {
			return fmt.Errorf("backend[%d]: type or command is required (supported types: %s)", i+1, strings.Join(backend.Supported(), ", "))
		}
		if e.Overlay != nil {
			if err := e.Overlay.check(); err != nil {
				return fmt.Errorf("backend[%d]: %w", i+1, err)
			}
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

// scopeEntries validates the actions and hooks of each backend entry and
// collects them with the top-level actions (the hooks are joined once the
// top-level ones are valid).
func (c *Config) scopeEntries() error {
	c.allActions = slices.Clone(c.Actions)
	c.allHooks = nil
	for i, e := range c.Backends {
		if err := action.ValidateFor(e.Actions, e.Name); err != nil {
			return fmt.Errorf("backend[%d] %s: %w", i+1, e.Name, err)
		}
		if err := hook.ValidateFor(e.Hooks, e.Name); err != nil {
			return fmt.Errorf("backend[%d] %s: %w", i+1, e.Name, err)
		}
		if e.Disabled {
			continue
		}
		c.allActions = append(c.allActions, e.Actions...)
		c.allHooks = append(c.allHooks, e.Hooks...)
	}
	for i, e := range c.Backends {
		if e.Disabled {
			continue
		}
		if err := action.CheckKeys(c.allActions, e.Name); err != nil {
			return fmt.Errorf("backend[%d] %s: %w", i+1, e.Name, err)
		}
	}
	return nil
}

// ActionParallel is the effective action_max_parallel.
func (c *Config) ActionParallel() int {
	if c.ActionMaxParallel == 0 {
		return action.DefaultMaxParallel
	}
	return c.ActionMaxParallel
}

// Dir is the directory relative paths in the config resolve against.
func (c *Config) Dir() string { return filepath.Dir(c.Path) }

// Locate picks the config file: the explicit path (-c / $TACK_CONFIG), else
// the first of ConfigNames in the working directory, else ~/.tack.yaml or
// ~/.tack.yml (HomeConfigNames).
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
	return "", fmt.Errorf("no config: pass -c FILE, set $TACK_CONFIG, or create %s (or %s) in this directory, or ~/%s (or ~/%s) (try `tack init`)", ConfigNames[0], strings.Join(ConfigNames[1:], ", "), HomeConfigNames[0], HomeConfigNames[1])
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
func (c *Config) Open() (*backend.Set, error) { return c.open(false) }

// OpenLazy is Open for a caller that lists in the background (the board): a
// slow backend does not delay opening, so its first list happens later.
func (c *Config) OpenLazy() (*backend.Set, error) { return c.open(true) }

func (c *Config) open(lazy bool) (*backend.Set, error) {
	if len(c.Backends) == 0 {
		return nil, fmt.Errorf("%s: no backends configured (add a `backend:` list; see docs/config.md)", c.Path)
	}
	var items []backend.Named
	for _, e := range c.Backends {
		if e.Disabled {
			continue
		}
		be, err := backend.OpenSpec(backend.Spec{Type: e.Type, Name: e.Name, Options: e.Options, Command: e.Command, BaseDir: c.Dir(), Lazy: lazy})
		if err != nil {
			for _, it := range items {
				it.Backend.Close()
			}
			return nil, fmt.Errorf("backend %s: %w", e.Name, err)
		}
		if e.Overlay != nil {
			ob, err := overlay.Wrap(be, e.Overlay.path(c.Dir(), e.Name), e.Overlay.Fields)
			if err != nil {
				be.Close()
				for _, it := range items {
					it.Backend.Close()
				}
				return nil, fmt.Errorf("backend %s: %w", e.Name, err)
			}
			be = ob
		}
		items = append(items, backend.Named{Name: e.Name, Type: e.Type, Backend: be})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%s: no backends enabled (every backend entry is disabled)", c.Path)
	}
	return backend.NewSet(items), nil
}
