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
	"strings"
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
//   - If explicit is non-empty, return it when it is an EXECUTABLE file
//     (Fallbacks are NOT consulted; the caller asked for a specific path and
//     a miss is a hard miss). The same predicate as the fallback arm: a
//     probe that accepts a path the spawn will fail on with EACCES reports a
//     backend that cannot run.
//   - Otherwise, try exec.LookPath(spec.Name), then iterate Fallbacks and
//     return the first executable file.
//
// Returns the resolved path and true on success; "", false on miss.
func Locate(explicit string, spec Spec) (string, bool) {
	if explicit != "" {
		if isExecutable(explicit) {
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

// PinKind classifies an operator-pinned binary value (an env override, a
// node's `command:`) by how os/exec will resolve it.
type PinKind int

const (
	// PinUnset is an empty value: nothing was pinned.
	PinUnset PinKind = iota
	// PinAbsolute is an absolute path, resolved as given.
	PinAbsolute
	// PinBareName has no path separator, so os/exec resolves it through
	// PATH and the command's Dir never enters.
	PinBareName
	// PinRelative contains a separator without being absolute. os/exec
	// resolves it against the command's Dir — which for an agent CLI is the
	// workspace — so it names a binary inside the checkout.
	PinRelative
)

// pinSeparators are BOTH path separators, on every platform.
//
// Deliberately not `filepath.Separator`: Windows accepts `/` as a separator
// while its own separator constant is `\`, so a host-separator test
// classifies "./cli" as a bare name there and hands it straight back — to be
// joined with the command's Dir, which for an agent CLI is the workspace. And
// a host-separator test cannot be falsified on a Unix CI, because the two
// predicates agree for every input a Unix host can express.
//
// One rule for every platform is therefore both safer and testable. It is
// stricter than Unix's own os/exec for exactly one shape — a file whose name
// literally contains a backslash — which is not worth platform-dependent
// reasoning about what runs.
const pinSeparators = `/\`

// ClassifyPin answers the question os/exec asks before it decides whether to
// resolve a value against the command's Dir.
func ClassifyPin(v string) PinKind {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return PinUnset
	case filepath.IsAbs(v):
		return PinAbsolute
	case !strings.ContainsAny(v, pinSeparators):
		return PinBareName
	default:
		return PinRelative
	}
}

// LocatePinned resolves an operator-pinned value the way the agent backends
// spawn it, so a probe and a spawn can never answer differently for the same
// string: an absolute path as given, a bare name through PATH, and a relative
// path as a MISS — because it is a refusal at spawn time.
func LocatePinned(pinned string, spec Spec) (string, bool) {
	switch ClassifyPin(pinned) {
	case PinAbsolute:
		return Locate(strings.TrimSpace(pinned), Spec{Name: spec.Name})
	case PinBareName:
		return Locate("", Spec{Name: strings.TrimSpace(pinned)})
	case PinRelative:
		return "", false
	default:
		return Locate("", spec)
	}
}
