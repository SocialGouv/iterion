// Package clilocate centralises the host-side probe used by backends
// that shell out to a CLI binary (claude, codex, …). It collapses the
// previously-duplicated discovery logic in pkg/backend/detect and
// pkg/backend/delegate/claudesdk into one path, and is the source of
// truth for the "well-known install locations" fallback list used when
// exec.LookPath misses because the process was started without an
// interactive shell rc (GUI launcher, devbox/nix wrapper, Homebrew on
// a host where `brew shellenv` isn't loaded into PATH).
package clilocate

import (
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
)

// Spec describes how to look for a CLI binary.
type Spec struct {
	// Name is the bare binary name handed to exec.LookPath.
	Name string
	// Fallbacks is an ordered list of absolute paths consulted when
	// LookPath misses. First executable file wins.
	Fallbacks []string
}

// Locate resolves a CLI binary path.
//
//   - If explicit is non-empty, return it when it exists as a non-directory
//     file (Fallbacks are NOT consulted; the caller asked for a specific
//     path and a miss is a hard miss).
//   - Otherwise, try exec.LookPath(spec.Name), then iterate Fallbacks and
//     return the first executable file.
//
// Returns the resolved path and true on success; "", false on miss.
func Locate(explicit string, spec Spec) (string, bool) {
	if explicit != "" {
		if fileExists(explicit) {
			return explicit, true
		}
		return "", false
	}
	if path, err := exec.LookPath(spec.Name); err == nil {
		return path, true
	}
	for _, p := range spec.Fallbacks {
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

// CommonBinaryCandidates returns an OS-aware list of well-known install
// locations for a CLI tool, in roughly-preferred order. Useful as a
// Spec.Fallbacks value when the process PATH may be missing user-shell
// additions (Volta, ~/.local/bin, Homebrew).
func CommonBinaryCandidates(name string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, ".volta", "bin", name),
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, ".linuxbrew", "bin", name),
		)
	}
	out = append(out,
		"/usr/local/bin/"+name,
		"/usr/bin/"+name,
		// Homebrew on Linux (multi-user shared install)
		"/home/linuxbrew/.linuxbrew/bin/"+name,
		// Homebrew on macOS Apple Silicon
		"/opt/homebrew/bin/"+name,
	)
	return out
}

// ClaudeLocalFallback returns the historical ~/.claude/local/claude
// install path that the Claude Code installer drops alongside an npm
// install. Empty when the user home directory cannot be resolved.
func ClaudeLocalFallback() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "local", "claude")}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	if goruntime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
