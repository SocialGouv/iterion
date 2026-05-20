package proc

import (
	"os"
	"path/filepath"
	"strings"
)

// LocateIterionBinary returns the absolute path of the iterion CLI
// binary suitable for subprocess invocation (MCP stdio servers,
// detached runners, sandbox bind-mounts, …). Resolution order:
//
//  1. <dirof(os.Executable())>/iterion — the binary right next to the
//     currently-running one. Critical for desktop deployments where the
//     daemon is `iterion-desktop` (a Wails wrapper that does not
//     dispatch hidden CLI subcommands like `__mcp-board`). os.Executable
//     alone would point at the desktop wrapper; spawning that with a
//     subcommand opens a phantom GUI window instead of running the
//     subprocess (the wrapper falls through to wails.Run on unknown
//     args).
//  2. $ITERION_BIN env var — escape hatch for unusual installs (e.g.
//     CI containers, vendored toolchains).
//  3. Standard Linux install paths: /usr/local/bin/iterion,
//     /usr/bin/iterion, ~/.local/bin/iterion.
//
// Returns "" when no binary can be located. Callers either fall back
// to a degraded mode (skip MCP wiring, log a warning) or expect the
// target environment to ship its own copy on PATH.
//
// Symmetry note: the same logic lives in pkg/runtime/sandbox.go as
// locateHostIterionBinary, kept inline there to avoid leaking the
// sandbox's bind-mount semantics into this generic helper. Edits to
// the lookup contract should land in both places.
func LocateIterionBinary() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "iterion")
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if env := strings.TrimSpace(os.Getenv("ITERION_BIN")); env != "" {
		if isExecutableFile(env) {
			return env
		}
	}
	candidates := []string{"/usr/local/bin/iterion", "/usr/bin/iterion"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "iterion"))
	}
	for _, p := range candidates {
		if isExecutableFile(p) {
			return p
		}
	}
	return ""
}

func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
