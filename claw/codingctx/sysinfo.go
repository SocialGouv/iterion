package codingctx

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// SystemInfo returns a string describing the current system environment.
func SystemInfo(workDir string) string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "unknown"
	}
	return fmt.Sprintf(
		"Working directory: %s\nOS: %s\nArchitecture: %s\nShell: %s\nDate/Time: %s",
		workDir,
		runtime.GOOS,
		runtime.GOARCH,
		shell,
		time.Now().Format("2006-01-02 15:04:05 MST"),
	)
}
