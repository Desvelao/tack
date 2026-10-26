// Package pathenv expands environment variables and a leading ~ in a path.
package pathenv

import (
	"fmt"
	"os"
	"strings"
)

// Expand replaces $VAR and ${VAR} with the environment's value and a leading
// ~ or ~/ with the home directory; $$ is a literal $. A variable that is not
// set is an error (an empty value is fine), so "$ROOT/x" never becomes "/x".
func Expand(s string) (string, error) {
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%q: %w", s, err)
		}
		s = home + s[1:]
	}
	missing := ""
	out := os.Expand(s, func(name string) string {
		if name == "$" {
			return "$"
		}
		v, ok := os.LookupEnv(name)
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("$%s is not set", missing)
	}
	return out, nil
}
