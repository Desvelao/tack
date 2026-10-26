package yamlbackend

import (
	"os"
	"strings"

	"tack/internal/backend"
	"tack/internal/backend/filestore"
)

func init() { backend.RegisterExample("yaml", example) }

func example(path string) (backend.Example, error) {
	var recs []filestore.Record
	var keys []string
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if _, err := Records(data, nil); err != nil {
				if paths := ListPaths(data); len(paths) == 1 {
					keys = strings.Split(paths[0], ".")
				}
			}
			if recs, err = Records(data, keys); err != nil {
				return backend.Example{}, err
			}
		case os.IsNotExist(err):
		default:
			return backend.Example{}, err
		}
	}
	ex := filestore.ExampleFromRecords("yaml", recs, filestore.GenericSection("# path: data.tasks    # where the task list is, as dotted keys (default: the top level or \"tasks\")\nindent: 2            # spaces per level when writing"))
	if keys != nil {
		ex.Section = strings.Replace(ex.Section, "source: {{source}}\n", "source: {{source}}\npath: "+strings.Join(keys, ".")+"\n", 1)
	}
	return ex, nil
}
