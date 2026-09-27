// Package designcontext resolves and materializes the design-context files a
// run's agents check the change against.
package designcontext

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveGlobalPaths expands machine-global design-context paths and requires
// an absolute result. The daemon has no meaningful working directory against
// which to resolve relative operator configuration.
func ResolveGlobalPaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			return nil, fmt.Errorf("empty design_context.files entry")
		}
		expanded, err := expandHome(path)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(expanded) {
			return nil, fmt.Errorf("design_context.files %q must be an absolute or ~/ path", path)
		}
		out = append(out, filepath.Clean(expanded))
	}
	return out, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}
