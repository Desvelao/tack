package markdownbackend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"tkan/internal/backend"
	"tkan/internal/backend/filestore"
	yamlbackend "tkan/internal/backend/yaml"
)

func init() { backend.RegisterExample("markdown", example) }

func generic() string {
	return "source: {{source}}     # directory of .md files, relative to this file\n" +
		"fields:             # logical field -> frontmatter key (only where it differs)\n" +
		"  # title: title    # required\n" +
		"  # status: status  # required\n" +
		"# The file name (without .md) is the id; the body is the description.\n" +
		"# Frontmatter keys not listed here are preserved untouched."
}

// example reads the frontmatter of the .md files in dir (if it exists).
func example(dir string) (backend.Example, error) {
	var recs []filestore.Record
	if dir != "" {
		entries, err := os.ReadDir(dir)
		switch {
		case err == nil:
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.EqualFold(filepath.Ext(e.Name()), ext) {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return backend.Example{}, err
				}
				front, _ := split(data)
				var n yaml.Node
				if err := yaml.Unmarshal([]byte(front), &n); err != nil || len(n.Content) == 0 {
					continue
				}
				r, err := yamlbackend.MappingRecord(n.Content[0])
				if err != nil {
					return backend.Example{}, fmt.Errorf("%s: %w", e.Name(), err)
				}
				recs = append(recs, r)
			}
		case os.IsNotExist(err):
		default:
			return backend.Example{}, err
		}
	}
	ex := filestore.ExampleFromRecords("markdown", recs, generic())
	// The id and the description never come from frontmatter keys.
	var lines, notes []string
	for _, l := range strings.Split(ex.Section, "\n") {
		if t := strings.TrimPrefix(strings.TrimSpace(l), "# "); !strings.HasPrefix(t, "id:") && !strings.HasPrefix(t, "description:") {
			lines = append(lines, l)
		}
	}
	for _, n := range ex.Notes {
		if !strings.Contains(n, `"id"`) && !strings.Contains(n, `"description"`) {
			notes = append(notes, n)
		}
	}
	ex.Section, ex.Notes = strings.Join(lines, "\n"), notes
	return ex, nil
}
