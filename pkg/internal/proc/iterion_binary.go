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
	p := locateIterionBinaryCandidate()
	if p == "" {
		return ""
	}
	// A relative ITERION_BIN is stat'd against the engine's cwd, but the
	// path travels to subprocesses that run with a DIFFERENT cwd (a tool
	// node's workDir, an MCP server, a bind-mount source) where it
	// resolves elsewhere or nowhere — silently. Every consumer gets one
	// absolute form.
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	return abs
}

func locateIterionBinaryCandidate() string {
	if exe, err := os.Executable(); err == nil && !isVolatileBuildPath(exe) {
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

// isVolatileBuildPath reports whether p looks like a Go-toolchain
// temporary build artifact (`go run`, `go test`, watchexec-driven
// hot rebuilds). Such paths get unlinked and recreated under load,
// so callers that hand the path to a subprocess (sandbox bind-mount,
// MCP stdio server spawned out-of-process) hit ENOENT once the
// build dir rotates. The resolver skips the sibling-of-Executable
// shortcut for these paths and falls through to stable install
// locations instead.
//
// Matches both Linux `/tmp/go-build*` and macOS `/var/folders/.../T/go-build*`.
func isVolatileBuildPath(p string) bool {
	return strings.Contains(p, "/go-build") || strings.Contains(p, "/T/go-build")
}

// IsVolatileExecutable reports whether p looks like a Go-toolchain
// temporary build artifact (`go run`, `go test`, watchexec-driven hot
// rebuilds) — see isVolatileBuildPath. For callers that degrade quietly
// when no engine binary can be located and need that degradation to stay
// quiet exactly on the volatile paths, warning everywhere else.
func IsVolatileExecutable(p string) bool {
	return isVolatileBuildPath(p)
}

func isExecutableFile(p string) bool {
	// #nosec G304 G703 — p originates from os.Executable(), the operator-set
	// ITERION_BIN override, or fixed install-path constants (see
	// LocateIterionBinary), never raw external request input.
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
