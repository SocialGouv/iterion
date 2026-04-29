package server

import "strings"

// isSkippedDir returns true for directories that should not be walked or watched.
func isSkippedDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules"
}

// isIterFile returns true if the path ends with .iter.
func isIterFile(path string) bool {
	return strings.HasSuffix(path, ".iter")
}
