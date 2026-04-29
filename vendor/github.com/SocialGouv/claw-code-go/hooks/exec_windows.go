//go:build windows

package hooks

// shellArgs returns ("cmd", ["/C", command]) for Windows.
func shellArgs(command string) (string, []string) {
	return "cmd", []string{"/C", command}
}
