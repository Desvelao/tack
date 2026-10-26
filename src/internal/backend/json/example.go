package jsonbackend

import (
	"os"
	"strings"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
)

func init() {
	backend.RegisterExample("json", func(p string) (backend.Example, error) { return example("json", p, false) })
	backend.RegisterExample("jsonl", func(p string) (backend.Example, error) { return example("jsonl", p, true) })
}

func example(typ, path string, lines bool) (backend.Example, error) {
	generic := filestore.GenericSection("# path: data.tasks    # where the task list is, as dotted keys (default: the top level or \"tasks\")\nindent: 2            # spaces per level when writing (0 = compact)")
	if lines {
		generic = filestore.GenericSection("")
	}
	var recs []filestore.Record
	var keys []string
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if !lines {
				// A document whose tasks are not at the top or under "tasks": find the list.
				if _, err := Records(data, false, nil); err != nil {
					if paths := ArrayPaths(data); len(paths) == 1 {
						keys = strings.Split(paths[0], ".")
					}
				}
			}
			if recs, err = Records(data, lines, keys); err != nil {
				return backend.Example{}, err
			}
		case os.IsNotExist(err):
		default:
			return backend.Example{}, err
		}
	}
	ex := filestore.ExampleFromRecords(typ, recs, generic)
	if keys != nil {
		ex.Section = strings.Replace(ex.Section, "source: {{source}}\n", "source: {{source}}\npath: "+strings.Join(keys, ".")+"\n", 1)
	}
	return ex, nil
}
