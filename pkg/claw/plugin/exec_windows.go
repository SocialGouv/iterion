//go:build windows

package plugin

// shellArgs returns ("cmd", ["/C", command]) for Windows.
func shellArgs(command string) (string, []string) {
	return "cmd", []string{"/C", command}
}
