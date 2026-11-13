//go:build !windows

package plugin

// shellArgs returns ("sh", ["-lc", command]) for Unix.
func shellArgs(command string) (string, []string) {
	return "sh", []string{"-lc", command}
}
