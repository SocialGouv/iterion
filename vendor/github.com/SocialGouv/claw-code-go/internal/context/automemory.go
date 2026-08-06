package context

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
)

const (
	// autoMemoryMaxBytes caps the injected MEMORY.md content.
	autoMemoryMaxBytes = 8 * 1024
	// autoMemoryDirEnv overrides the auto-memory root directory.
	autoMemoryDirEnv = "CLAW_MEMORY_DIR"
	// autoMemoryFile is the index file injected at session start.
	autoMemoryFile = "MEMORY.md"
)

// WorkspaceFingerprint produces a stable 16-char hex digest of the workspace
// path using FNV-1a (64-bit). This matches the Rust implementation exactly
// (same constants: offset=0xcbf29ce484222325, prime=0x100000001b3). Canonical
// implementation — internal/runtime delegates here.
func WorkspaceFingerprint(workspaceRoot string) string {
	h := fnv.New64a()
	h.Write([]byte(workspaceRoot))
	return fmt.Sprintf("%016x", h.Sum64())
}

// AutoMemoryDir returns the persistent memory directory for workDir:
// ~/.claw-code/memory/<workspace-fingerprint>/ by default, or
// $CLAW_MEMORY_DIR/<workspace-fingerprint>/ when the env override is set.
// Returns "" when no home directory is available.
func AutoMemoryDir(workDir string) string {
	root := os.Getenv(autoMemoryDirEnv)
	if root == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil || homeDir == "" {
			return ""
		}
		root = filepath.Join(homeDir, ".claw-code", "memory")
	}
	return filepath.Join(root, WorkspaceFingerprint(workDir))
}

// LoadAutoMemorySection renders the "# Auto memory" prompt section for
// workDir: the memory directory path, short maintenance instructions, and the
// current MEMORY.md content (head-truncated at 8KB). The instructions are
// always emitted so the model knows the mechanism exists even before the
// first write. Returns "" only when no memory directory can be resolved.
// Read-only: never creates the directory — the model's file tools do that on
// first write.
func LoadAutoMemorySection(workDir string) string {
	return LoadAutoMemorySectionAt(AutoMemoryDir(workDir))
}

// LoadAutoMemorySectionAt is LoadAutoMemorySection against an EXPLICIT memory
// directory, for hosts that own where a session's memory lives.
//
// The workDir-derived default fingerprints the working directory, which is
// wrong for any host that runs an agent in a fresh directory per session (a
// git worktree, an ephemeral container): the fingerprint changes, so the
// agent starts from an empty memory every time and the mechanism silently
// does nothing. Such a host resolves the directory from its own identity —
// project, agent, tenant — and passes it here.
//
// An empty dir returns "", matching LoadAutoMemorySection's contract for an
// unresolvable directory.
func LoadAutoMemorySectionAt(dir string) string {
	if dir == "" {
		return ""
	}

	content := "(empty — no memory recorded yet)"
	if data, err := os.ReadFile(filepath.Join(dir, autoMemoryFile)); err == nil {
		text := strings.TrimSpace(string(data))
		if len(text) > autoMemoryMaxBytes {
			text = text[:autoMemoryMaxBytes] + "\n... (truncated — read the full file if needed)"
		}
		if text != "" {
			content = text
		}
	}

	return fmt.Sprintf(`Your persistent memory directory for this project is: %s
%s from that directory is injected below at session start (first %dKB).
- Record durable project knowledge there (build/test commands, conventions, gotchas, decisions) using the standard file tools; create the directory if needed.
- Keep %s a concise index; put details in topic files in the same directory and reference them from %s.
- Update it when you learn something that would save time in a future session; remove stale entries.

## %s

%s`, dir, autoMemoryFile, autoMemoryMaxBytes/1024, autoMemoryFile, autoMemoryFile, autoMemoryFile, content)
}
