package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/config"
)

const (
	// defaultMemoryMaxBytes caps the combined injected memory content.
	// Raised from the historical 20KB because walk-up + imports legitimately
	// grow the corpus.
	defaultMemoryMaxBytes = 48 * 1024
	// defaultMaxImportDepth bounds recursive @path import hops (Claude Code
	// parity: 5).
	defaultMaxImportDepth = 5
)

// MemoryOptions configures CLAUDE.md memory discovery and loading.
type MemoryOptions struct {
	// WalkUp includes CLAUDE.md files from the workDir's ancestor
	// directories (root-most first), like Claude Code.
	WalkUp bool
	// Imports expands @path references inside memory files, recursively.
	Imports bool
	// MaxBytes caps the combined content (<=0 → defaultMemoryMaxBytes).
	MaxBytes int
	// MaxImportDepth bounds import recursion (<=0 → defaultMaxImportDepth).
	MaxImportDepth int
}

// DefaultMemoryOptions returns the Claude Code-parity defaults (walk-up and
// imports enabled).
func DefaultMemoryOptions() MemoryOptions {
	return MemoryOptions{WalkUp: true, Imports: true}
}

// AncestorClaudeMdPaths returns existing CLAUDE.md paths from startDir up to
// the filesystem root. startDir's file comes first; files higher up follow in
// order, so callers can let leaves win on conflicts.
func AncestorClaudeMdPaths(startDir string) ([]string, error) {
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for {
		candidate := filepath.Join(abs, "CLAUDE.md")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			paths = append(paths, candidate)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return paths, nil
}

// memoryCandidate is a discovered memory file with its display label.
type memoryCandidate struct {
	label string
	path  string
}

// memoryCandidates returns the ordered list of memory files to load:
// user-global first, then (with WalkUp) workDir's ancestors root-most first,
// then the project files — most-specific instructions end up last, closest to
// the conversation. Paths are absolute-cleaned and deduplicated.
func memoryCandidates(workDir string, opts MemoryOptions) []memoryCandidate {
	homeDir, _ := os.UserHomeDir()

	var candidates []memoryCandidate
	if homeDir != "" {
		candidates = append(candidates, memoryCandidate{
			"User global (~/.claude/CLAUDE.md)",
			filepath.Join(homeDir, ".claude", "CLAUDE.md"),
		})
	}

	if opts.WalkUp {
		if parent := filepath.Dir(workDir); parent != workDir {
			ancestors, _ := AncestorClaudeMdPaths(parent)
			// AncestorClaudeMdPaths is leaf → root; inject root-most first.
			for i := len(ancestors) - 1; i >= 0; i-- {
				candidates = append(candidates, memoryCandidate{
					fmt.Sprintf("Ancestor (%s)", ancestors[i]),
					ancestors[i],
				})
			}
		}
	}

	candidates = append(candidates,
		memoryCandidate{"Project root (CLAUDE.md)", filepath.Join(workDir, "CLAUDE.md")},
		memoryCandidate{"Project config (.claude/CLAUDE.md)", filepath.Join(workDir, ".claude", "CLAUDE.md")},
	)

	seen := make(map[string]bool, len(candidates))
	deduped := candidates[:0]
	for _, c := range candidates {
		if abs, err := filepath.Abs(c.path); err == nil {
			c.path = filepath.Clean(abs)
		}
		if seen[c.path] {
			continue
		}
		seen[c.path] = true
		deduped = append(deduped, c)
	}
	return deduped
}

// LoadMemory discovers and loads CLAUDE.md memory files per opts, returning
// the concatenated content and the mtime map (path → mtime ns) of every file
// actually read — roots and imports — for cache revalidation.
func LoadMemory(workDir string, opts MemoryOptions) (string, map[string]int64) {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMemoryMaxBytes
	}
	maxDepth := opts.MaxImportDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxImportDepth
	}

	mtimes := make(map[string]int64)
	var parts []string
	totalBytes := 0

	for _, c := range memoryCandidates(workDir, opts) {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		if info, err := os.Stat(c.path); err == nil {
			mtimes[c.path] = info.ModTime().UnixNano()
		}

		// Frontmatter is config, not instructions — strip it from injection.
		_, body, fmErr := config.ParseFrontmatter(data)
		text := string(data)
		if fmErr == nil {
			text = string(body)
		}

		section := fmt.Sprintf("## %s\n\n%s", c.label, text)

		if opts.Imports {
			visited := map[string]bool{c.path: true}
			for _, imp := range expandImports(text, filepath.Dir(c.path), maxDepth, visited, mtimes) {
				section += "\n\n" + imp
			}
		}

		remaining := maxBytes - totalBytes
		if remaining <= 0 {
			break
		}
		if len(section) > remaining {
			section = section[:remaining] + "\n... (truncated)"
		}
		parts = append(parts, section)
		totalBytes += len(section)
	}

	if len(parts) == 0 {
		return "", mtimes
	}
	return strings.Join(parts, "\n\n---\n\n"), mtimes
}

// expandImports resolves @path references in text (relative to baseDir),
// returning one "### Imported: <path>" block per readable target, depth-first
// with cycle protection. Unreadable targets are silently skipped. Every file
// read is recorded in mtimes.
func expandImports(text, baseDir string, depth int, visited map[string]bool, mtimes map[string]int64) []string {
	if depth <= 0 {
		return nil
	}
	var blocks []string
	for _, ref := range scanImportRefs(text) {
		path := resolveImportPath(ref, baseDir)
		if path == "" || visited[path] {
			continue
		}
		visited[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil {
			mtimes[path] = info.ModTime().UnixNano()
		}
		content := string(data)
		blocks = append(blocks, fmt.Sprintf("### Imported: %s\n\n%s", path, content))
		blocks = append(blocks, expandImports(content, filepath.Dir(path), depth-1, visited, mtimes)...)
	}
	return blocks
}

// scanImportRefs extracts @path tokens from markdown text, skipping fenced
// code blocks and inline code spans. A reference is a whitespace-delimited
// token starting with "@" followed by a path-like first character.
func scanImportRefs(text string) []string {
	var refs []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, field := range strings.Fields(stripInlineCode(line)) {
			if len(field) < 2 || field[0] != '@' {
				continue
			}
			ref := strings.TrimRight(field[1:], ".,;:!?)\"'")
			if ref == "" || !isPathLike(ref) {
				continue
			}
			refs = append(refs, ref)
		}
	}
	return refs
}

// stripInlineCode removes `...` spans so an @token inside inline code is not
// treated as an import.
func stripInlineCode(line string) string {
	segments := strings.Split(line, "`")
	var sb strings.Builder
	for i, seg := range segments {
		if i%2 == 0 {
			sb.WriteString(seg)
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// isPathLike reports whether an import reference looks like a file path
// rather than an @mention (e.g. "@anthropic-ai/claude-code" or "@user").
// Accepted forms: absolute (/...), home (~/...), explicit relative (./ ../),
// or a bare relative path containing a path separator or a dot extension.
func isPathLike(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "/"), strings.HasPrefix(ref, "~/"),
		strings.HasPrefix(ref, "./"), strings.HasPrefix(ref, "../"):
		return true
	case strings.HasPrefix(ref, "@"):
		// "@@..." — not a path.
		return false
	default:
		// Bare relative: require a separator or an extension dot to avoid
		// matching npm-scope-style mentions and emails.
		return strings.ContainsRune(ref, '/') || strings.Contains(filepath.Base(ref), ".")
	}
}

// resolveImportPath turns an import reference into a cleaned absolute path,
// resolving "~/" against the home directory and relative refs against baseDir.
func resolveImportPath(ref, baseDir string) string {
	switch {
	case strings.HasPrefix(ref, "~/"):
		homeDir, err := os.UserHomeDir()
		if err != nil || homeDir == "" {
			return ""
		}
		return filepath.Clean(filepath.Join(homeDir, ref[2:]))
	case filepath.IsAbs(ref):
		return filepath.Clean(ref)
	default:
		return filepath.Clean(filepath.Join(baseDir, ref))
	}
}

// MemoryCandidateMtimes returns path → mtime ns for the discovery candidates
// that currently exist (roots only — import mtimes come from LoadMemory).
// Used by the assembler to notice created/deleted memory files.
func MemoryCandidateMtimes(workDir string, opts MemoryOptions) map[string]int64 {
	result := make(map[string]int64)
	for _, c := range memoryCandidates(workDir, opts) {
		if info, err := os.Stat(c.path); err == nil {
			result[c.path] = info.ModTime().UnixNano()
		}
	}
	return result
}
