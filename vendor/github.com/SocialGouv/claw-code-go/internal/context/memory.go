package context

import (
	"fmt"
	"io"
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
//
// Memory comes from three origins a host can keep or leave out:
//
//   - the user scope: ~/.claude/CLAUDE.md;
//   - the workspace: workDir's own files, plus — below Root, with WalkUp — the
//     "inner" ancestors between workDir and Root;
//   - everything above it: the "outer" ancestors, strictly above Root.
//
// The scope fields (SkipUser, Root, SkipWorkspace, SkipOuter,
// ClaudeCodeLayout) all default to the unscoped behaviour: leave them zero and
// the loader reads exactly what it read before they existed.
type MemoryOptions struct {
	// WalkUp includes CLAUDE.md files from the workDir's ancestor
	// directories (root-most first), like Claude Code. Inner and outer
	// ancestors both need it.
	WalkUp bool
	// Imports expands @path references inside memory files, recursively.
	Imports bool
	// MaxBytes caps the combined content (<=0 → defaultMemoryMaxBytes).
	MaxBytes int
	// MaxImportDepth bounds import recursion (<=0 → defaultMaxImportDepth).
	MaxImportDepth int

	// SkipUser leaves out the user scope: ~/.claude/CLAUDE.md and, under
	// ClaudeCodeLayout, ~/.claude/rules/**/*.md.
	//
	// It removes the scope, not the file: when the same file is also a
	// directory's own .claude/ — HOME is an ancestor of workDir, or is Root
	// or workDir itself — it is loaded there, under that directory's scope
	// (SkipOuter / SkipWorkspace). Claude Code behaves the same way: a
	// workspace under $HOME still loads ~/.claude/CLAUDE.md as an ancestor's.
	// Pair SkipUser with SkipOuter to keep the operator's files out.
	SkipUser bool
	// Root is the workspace boundary, an absolute directory (a relative one is
	// resolved against the working directory, like workDir). Ancestors
	// strictly above workDir, up to and including Root, are inner; ancestors
	// strictly above Root are outer. Empty means every ancestor is outer.
	//
	// A Root that is neither workDir nor one of its ancestors cannot bound
	// anything, so it is treated as unset rather than guessed at. Directories
	// are compared by identity, not by spelling: a symlinked path to Root, or
	// another case on a case-insensitive filesystem, still matches.
	//
	// A Root that bounds the workspace also bounds what its files may pull
	// in: an @import of a workspace-scope file (workDir's or an inner
	// ancestor's) must resolve inside Root, transitively, or it is skipped —
	// a repository's CLAUDE.md cannot read the operator's machine into the
	// prompt. Imports of user-scope and outer-ancestor files stay free, and
	// with no Root all imports keep today's freedom. A workspace-scope file
	// that is itself a symlink resolving outside Root is skipped, and so is
	// one confined whose read loses a swap race with the check.
	//
	// Two limits are known: a Root spelling that does not resolve — a
	// dangling symlink — confines nothing, indistinguishable from an unset
	// one; and hardlinks defeat path-based confinement, since the linked
	// file lives inside the boundary too.
	Root string
	// SkipWorkspace leaves out workDir's own files and the inner ancestors.
	SkipWorkspace bool
	// SkipOuter leaves out the outer ancestors. With no Root every ancestor
	// is outer, so SkipOuter then drops all of them.
	SkipOuter bool
	// ClaudeCodeLayout loads the Claude Code memory layout instead of the
	// CLAUDE.md files alone: in every included directory (workDir and the
	// included ancestors) also <dir>/.claude/CLAUDE.md and
	// <dir>/.claude/rules/**/*.md, and — unless SkipUser — the user rules
	// ~/.claude/rules/**/*.md. Rules load in the order of their relative
	// path. A rule whose frontmatter scopes it with `paths` is conditional in
	// Claude Code and is left out: a static prompt cannot say when it applies.
	// Other rules have their frontmatter stripped, and their @imports expand.
	//
	// Rules may be symlinks, to files or to directories. In the workspace's
	// rules — workDir's and the inner ancestors' — a symlink is followed only
	// while it stays inside the workspace (Root, or workDir when there is
	// none), since a repository must not steer the loader at the rest of the
	// machine. The user scope and the outer ancestors are the operator's own
	// and are followed freely.
	ClaudeCodeLayout bool
}

// DefaultMemoryOptions returns the Claude Code-parity defaults (walk-up and
// imports enabled).
func DefaultMemoryOptions() MemoryOptions {
	return MemoryOptions{WalkUp: true, Imports: true}
}

// dirChain returns dir followed by each of its parents up to the filesystem
// root.
func dirChain(dir string) []string {
	chain := []string{dir}
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return chain
		}
		chain = append(chain, parent)
		dir = parent
	}
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
	for _, dir := range dirChain(abs) {
		candidate := filepath.Join(dir, "CLAUDE.md")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			paths = append(paths, candidate)
		}
	}
	return paths, nil
}

// scopedAncestors returns the directories strictly above workDir, root-most
// first, split at root: inner ends at root (inclusive), outer is everything
// above it. An unset root, or one that is not workDir or an ancestor of it,
// leaves every ancestor outer.
func scopedAncestors(workDir, root string) (outer, inner []string) {
	chain := dirChain(workDir)[1:] // parent first
	n := innerAncestors(workDir, chain, root)
	for i := len(chain) - 1; i >= n; i-- {
		outer = append(outer, chain[i])
	}
	for i := n - 1; i >= 0; i-- {
		inner = append(inner, chain[i])
	}
	return outer, inner
}

// innerAncestors returns how many of chain (workDir's ancestors, parent first)
// lie at or below root: the index of root in chain plus one. It is 0 when root
// is unset, is workDir itself (nothing is above workDir and inside it), or is
// no ancestor of workDir.
func innerAncestors(workDir string, chain []string, root string) int {
	if root == "" {
		return 0
	}
	// Spelling first: the common case needs no filesystem access.
	for i, dir := range chain {
		if dir == root {
			return i + 1
		}
	}
	// Then identity, so a symlinked or differently cased spelling of the same
	// directory still bounds the walk. Matching by prefix instead would take
	// /srv/repo2 for a child of /srv/repo.
	rootInfo, err := os.Stat(root)
	if err != nil {
		return 0
	}
	for i, dir := range chain {
		if sameDir(dir, rootInfo) {
			return i + 1
		}
	}
	return 0
}

func sameDir(dir string, want os.FileInfo) bool {
	info, err := os.Stat(dir)
	return err == nil && os.SameFile(info, want)
}

// isSameDir reports whether two paths are the same directory, by spelling or
// by identity.
func isSameDir(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// importBoundary returns the directory an @import of a workspace-scope file
// must resolve in, or "" when imports are unconfined. Only an explicit Root
// that bounds the workspace — it is, or contains, workDir — confines them:
// with no Root, or a Root that is no ancestor of workDir, imports keep
// today's freedom. Imports reached transitively from a workspace file count,
// so the boundary follows the chain.
func importBoundary(workDir string, opts MemoryOptions) string {
	if opts.Root == "" {
		return ""
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	_, inner := scopedAncestors(workDir, opts.Root)
	if len(inner) > 0 {
		return inner[0]
	}
	if isSameDir(workDir, opts.Root) {
		return workDir
	}
	return ""
}

// memoryCandidate is a discovered memory file with its display label.
type memoryCandidate struct {
	label string
	path  string
	// rule marks a .claude/rules file: it is read through the rule
	// frontmatter reader, which can drop it as conditional.
	rule bool
	// workspace marks a workspace-scope file — workDir's or an inner
	// ancestor's. Its @imports (and it, when it is a symlink) are confined
	// to the workspace while a Root bounds it.
	workspace bool
}

// memoryCandidates returns the ordered list of memory files to load, most
// general first so the most specific instructions end up last, closest to the
// conversation: the user scope, then the outer ancestors root-most first, then
// the inner ancestors, then workDir. Within a directory: CLAUDE.md,
// .claude/CLAUDE.md, then its rules. Paths are absolute-cleaned and
// deduplicated, the first occurrence winning.
func memoryCandidates(workDir string, opts MemoryOptions) []memoryCandidate {
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}

	var candidates []memoryCandidate
	if !opts.SkipUser {
		candidates = appendUserCandidates(candidates, opts.ClaudeCodeLayout)
	}

	var outer, inner []string
	if opts.WalkUp {
		outer, inner = scopedAncestors(workDir, opts.Root)
	}
	if !opts.SkipOuter {
		// The operator's own directories: their rules' symlinks are free.
		for _, dir := range outer {
			candidates = appendAncestorCandidates(candidates, dir, opts.ClaudeCodeLayout, "")
		}
	}
	if !opts.SkipWorkspace {
		// The workspace, from Root (or workDir alone) down: what a repository
		// controls, and so where its rules' symlinks must stay.
		confine := workDir
		if len(inner) > 0 {
			confine = inner[0]
		}
		// Everything appended below is workspace scope: its files, and their
		// imports, are confined to the workspace while a Root bounds it.
		from := len(candidates)
		for _, dir := range inner {
			candidates = appendAncestorCandidates(candidates, dir, opts.ClaudeCodeLayout, confine)
		}
		candidates = appendProjectCandidates(candidates, workDir, opts.ClaudeCodeLayout, confine)
		for i := range candidates[from:] {
			candidates[from:][i].workspace = true
		}
	}

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

// appendUserCandidates adds the user scope: ~/.claude/CLAUDE.md, then — with
// the Claude Code layout — ~/.claude/rules. Without a home directory there is
// no user scope.
func appendUserCandidates(dst []memoryCandidate, layout bool) []memoryCandidate {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		return dst
	}
	userDir := filepath.Join(homeDir, ".claude")
	dst = append(dst, memoryCandidate{
		label: "User global (~/.claude/CLAUDE.md)",
		path:  filepath.Join(userDir, "CLAUDE.md"),
	})
	if !layout {
		return dst
	}
	for _, r := range discoverRules(filepath.Join(userDir, "rules"), "") {
		dst = append(dst, memoryCandidate{label: "User rule (~/.claude/rules/" + r.rel + ")", path: r.path, rule: true})
	}
	return dst
}

// appendAncestorCandidates adds an ancestor directory's memory files. An
// ancestor has always contributed its CLAUDE.md; .claude/CLAUDE.md and the
// rules belong to the Claude Code layout. confine bounds the symlinks in its
// rules (see discoverRules).
func appendAncestorCandidates(dst []memoryCandidate, dir string, layout bool, confine string) []memoryCandidate {
	claudeMd := filepath.Join(dir, "CLAUDE.md")
	dst = append(dst, memoryCandidate{label: fmt.Sprintf("Ancestor (%s)", claudeMd), path: claudeMd})
	if !layout {
		return dst
	}
	dotClaudeMd := filepath.Join(dir, ".claude", "CLAUDE.md")
	dst = append(dst, memoryCandidate{label: fmt.Sprintf("Ancestor (%s)", dotClaudeMd), path: dotClaudeMd})
	for _, r := range discoverRules(filepath.Join(dir, ".claude", "rules"), confine) {
		dst = append(dst, memoryCandidate{label: fmt.Sprintf("Ancestor rule (%s)", r.path), path: r.path, rule: true})
	}
	return dst
}

// appendProjectCandidates adds workDir's memory files. Its CLAUDE.md and
// .claude/CLAUDE.md have always loaded; the rules belong to the Claude Code
// layout. confine bounds the symlinks in its rules (see discoverRules).
func appendProjectCandidates(dst []memoryCandidate, workDir string, layout bool, confine string) []memoryCandidate {
	dst = append(dst,
		memoryCandidate{label: "Project root (CLAUDE.md)", path: filepath.Join(workDir, "CLAUDE.md")},
		memoryCandidate{label: "Project config (.claude/CLAUDE.md)", path: filepath.Join(workDir, ".claude", "CLAUDE.md")},
	)
	if !layout {
		return dst
	}
	for _, r := range discoverRules(filepath.Join(workDir, ".claude", "rules"), confine) {
		dst = append(dst, memoryCandidate{label: "Project rule (.claude/rules/" + r.rel + ")", path: r.path, rule: true})
	}
	return dst
}

// LoadMemory discovers and loads CLAUDE.md memory files per opts — and, under
// ClaudeCodeLayout, .claude/CLAUDE.md files and rules — returning the
// concatenated content and the mtime map (path → mtime ns) of every file
// actually read, imports included and conditional rules included (they are
// read to be recognised), for cache revalidation. A workspace-scope import
// that leaves the Root boundary, and a workspace-scope file that is a symlink
// out of it, are neither loaded nor recorded: they are not dependencies.
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

	// The workspace boundary, resolved once. A boundary that cannot be
	// resolved still confines: nothing can be shown to be inside a boundary
	// that is not there, so workspace imports and workspace files reached
	// through links are skipped (fail closed).
	boundary := importBoundary(workDir, opts)
	confined := false
	var boundaryReal string
	if boundary != "" {
		confined = true
		if real, err := filepath.EvalSymlinks(boundary); err == nil {
			boundaryReal = real
		}
	}

	candidates := memoryCandidates(workDir, opts)
	for i := range candidates {
		// Classified by location, not by the list that appended it: with
		// HOME inside Root, a repository-seeded ~/.claude/CLAUDE.md is a
		// workspace file, not the operator's, whichever label it carries.
		if confined && !candidates[i].workspace && resolvesWithin(candidates[i].path, boundaryReal) {
			candidates[i].workspace = true
		}
	}

	for _, c := range candidates {
		// A workspace-scope file that is itself a link out of the workspace
		// is not loaded. It stays a discovery candidate, so re-pointing the
		// link inside is still seen.
		var data []byte
		if c.workspace && confined {
			d, ok := readWithin(c.path, boundaryReal)
			if !ok {
				continue
			}
			data = d
		} else {
			d, err := os.ReadFile(c.path)
			if err != nil {
				continue
			}
			data = d
		}
		if info, err := os.Stat(c.path); err == nil {
			mtimes[c.path] = info.ModTime().UnixNano()
		}

		// Frontmatter is config, not instructions — strip it from injection.
		text := string(data)
		if c.rule {
			body, conditional := parseRule(data)
			if conditional {
				// Applies only while matching files are in play, which a
				// static prompt cannot express (see parseRule).
				continue
			}
			text = body
		} else if _, body, fmErr := config.ParseFrontmatter(data); fmErr == nil {
			text = string(body)
		}

		section := fmt.Sprintf("## %s\n\n%s", c.label, text)

		if opts.Imports {
			limit := ""
			if c.workspace && confined {
				limit = boundaryReal
			}
			visited := map[string]bool{c.path: true}
			for _, imp := range expandImports(text, filepath.Dir(c.path), maxDepth, visited, mtimes, limit, boundaryReal) {
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
//
// limit, when not empty, is a resolved directory imports must resolve inside:
// targets outside it are not loaded and leave no mtime behind. The boundary
// travels with the recursion by origin: a file loaded from a confined scope
// is confined, and so is one that resolves inside the boundary after being
// reached from a free scope — a workspace file imported by the operator's
// CLAUDE.md does not get to leave through its own imports.
func expandImports(text, baseDir string, depth int, visited map[string]bool, mtimes map[string]int64, limit, boundary string) []string {
	if depth <= 0 {
		return nil
	}
	var blocks []string
	for _, ref := range scanImportRefs(text) {
		path := resolveImportPath(ref, baseDir)
		if path == "" || visited[path] {
			continue
		}
		var data []byte
		if limit != "" {
			d, ok := readWithin(path, limit)
			if !ok {
				continue
			}
			data = d
		} else {
			d, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			data = d
		}
		visited[path] = true
		if info, err := os.Stat(path); err == nil {
			mtimes[path] = info.ModTime().UnixNano()
		}
		content := string(data)
		blocks = append(blocks, fmt.Sprintf("### Imported: %s\n\n%s", path, content))
		recLimit := limit
		if recLimit == "" && boundary != "" && resolvesWithin(path, boundary) {
			recLimit = boundary
		}
		blocks = append(blocks, expandImports(content, filepath.Dir(path), depth-1, visited, mtimes, recLimit, boundary)...)
	}
	return blocks
}

// readWithin reads path, swearing the bytes came from inside dir. The path is
// resolved once and opened at its resolution — a symlink flipped afterwards
// is never followed again — and the opened file is compared, by device and
// inode, with what the resolution pointed at, so a file swapped in between
// drops the content instead of leaking it.
//
// The window left is the one between the resolution and the open, where the
// swap is detected rather than prevented; closing it needs openat2 with
// RESOLVE_BENEATH, which the os package does not expose.
func readWithin(path, dir string) ([]byte, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !within(resolved, dir) {
		return nil, false
	}
	want, err := os.Stat(resolved)
	if err != nil || !want.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(opened, want) {
		return nil, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	return data, true
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
