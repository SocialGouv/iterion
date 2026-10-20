package model

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// clawAmbientMaxBytes lifts claw-code-go's 48 KiB memory cap: the cap
// truncates from the END of the corpus, and the workspace's own files load
// last, so a repository with a large CLAUDE.md would lose exactly its own
// instructions to the operator's rules under `all`. 4 MiB is past any real
// instruction corpus; the model's own context limit is the honest bound.
// The whole block shares this one bound: the AGENTS.md fallback below
// receives what the walk leaves of it.
const clawAmbientMaxBytes = 4 << 20

// clawAmbientSystemContext renders the project-instructions block the node's
// ambient-context policy (ADR-119) allows, with claw-code-go's own loader —
// walk-up and imports, `.claude/rules`, workspace imports confined to the
// boundary — plus the AGENTS.md files that walk leaves unloaded
// (clawAgentsMdParts), read under the same boundary discipline. Empty for
// `none`, whose contract is nothing but the prompt. The caller appends it as
// one more cacheable system block; the container runner takes the same path,
// since it rebuilds the Task and runs the same Execute.
func clawAmbientSystemContext(task delegate.Task) string {
	if task.WorkDir == "" || !task.AmbientContext.IncludesWorkspace() && !task.AmbientContext.IncludesOperator() {
		return ""
	}
	cfg := clawrt.MinimalPromptConfig()
	cfg.ProjectInstructions = true
	cfg.MemoryWalkUp = true
	cfg.MemoryImports = true
	cfg.MemoryMaxBytes = clawAmbientMaxBytes
	cfg.MemoryClaudeCodeLayout = true
	root := ambient.RepoRoot(task.WorkDir)
	switch task.AmbientContext {
	case ambient.Operator:
		// The main checkout of a nested worktree is the same repository: the
		// boundary moves up to it, so its files stay repository content.
		cfg.MemoryRoot = ambient.OperatorBoundary(root)
		cfg.MemorySkipWorkspace = true
	case ambient.All:
		// Native parity: the walk runs to the filesystem root, the user
		// scope included. No Root: the walk — and the fallback with it — is
		// unconfined, the operator's explicit choice.
	default:
		cfg.MemoryRoot = root
		cfg.MemorySkipUser = true
		cfg.MemorySkipOuter = true
	}
	mem := clawrt.BuildSystemContext(filepath.Clean(task.WorkDir), cfg)
	if !task.AmbientContext.IncludesWorkspace() {
		// Without the workspace origin the fallback has nothing to add: its
		// files are the repository's, and `operator` deliberately leaves them
		// out.
		return mem
	}
	// One stated bound for the whole block: the fallback receives what the
	// walk leaves of clawAmbientMaxBytes, measured on the walk's output — an
	// upper edge on its consumption — so the block stays within the bound
	// the ADR states, plus at most a truncation notice and one part
	// separator.
	return clawJoinMemoryParts(mem, clawAgentsMdParts(task, cfg.MemoryRoot, clawAmbientMaxBytes-len(mem)))
}

// clawAgentsMdParts renders the AGENTS.md files of the workspace scope that
// the CLAUDE.md walk leaves unloaded — claude code's fallback, which reads
// AGENTS.md in a directory only when no CLAUDE.md lives there. The shadow
// rule is measured on the session root alone (ADR-120); the per-directory
// form is iterion's generalization, the CLI's own walk-up shape unmeasured.
// The memory walk and every other harness name what claw now names: codex
// reads AGENTS.md from the root down, pi prefers it per directory, and
// claude_code falls back to it where the CLI found no CLAUDE.md.
//
// Root-most first among its own parts, in the loader's render shape; at the
// block level the parts follow the walk's whole output, so a parent's
// AGENTS.md renders after a child's CLAUDE.md — most-general-first holds
// within each pass, not across them (ADR-119). The body is injected
// import-free: @path references reach the node as text, where the same file
// as CLAUDE.md expands them; teaching the loader's expansion machinery
// AGENTS.md is the claw-code-go follow-up (its .works/ worktree, then
// scripts/bump-claw.sh). Bounded by what the walk leaves of the block's one
// budget, and read under the walk's own confinement: where the walk bounds
// its workspace scope, an AGENTS.md — or a shadowing CLAUDE.md — that is
// itself a link out of the boundary is not there, the loader's refusal of
// workspace files that leave the repository; where the walk is unconfined
// (`all`), the fallback is unconfined with it.
//
// The workspace scope only, even under `all`: the operator's own AGENTS.md
// is a loading behaviour iterion has not measured, while the walk's user
// scope already serves the operator's ~/.claude memory (ADR-119 records the
// measured surface).
func clawAgentsMdParts(task delegate.Task, boundary string, budget int) []string {
	workDir, err := filepath.Abs(task.WorkDir)
	if err != nil {
		return nil
	}
	root := ambient.RepoRoot(workDir)
	// The walk's workspace boundary, resolved once — resolved, because the
	// reads and shadow lookups below compare against the real tree. A
	// boundary that cannot be resolved still confines: nothing can be shown
	// to be inside a boundary that is not there (the loader fail-closes the
	// same way).
	confined := boundary != ""
	boundaryReal := ""
	if confined {
		if real, err := filepath.EvalSymlinks(boundary); err == nil {
			boundaryReal = real
		}
	}
	// The scope's directories, workDir first; a directory off the root's tree
	// (RepoRoot's contract makes that unreachable) stops the chain instead of
	// letting the walk leave the repository. Rendered root-most first below;
	// the block-level placement is the comment above's stated divergence.
	sep := string(filepath.Separator)
	var chain []string
	for dir := workDir; ; {
		chain = append(chain, dir)
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(dir, strings.TrimSuffix(root, sep)+sep) {
			break
		}
		dir = parent
	}
	var parts []string
	for i := len(chain) - 1; i >= 0; i-- {
		section, ok := clawAgentsMdSection(chain[i], budget, confined, boundaryReal)
		if ok {
			parts = append(parts, section)
			budget -= len(section)
			if budget <= 0 {
				break
			}
		}
	}
	return parts
}

// clawAgentsMdSection renders dir's AGENTS.md in the memory walk's section
// shape — a heading naming the file and its path, then the body — capped at
// the remaining budget the way the walk truncates its own sections. ok is
// false when the budget is spent, a CLAUDE.md of the walk's candidate set
// shadows the file, or the file is not there — not there meaning also, where
// the walk confines its workspace scope, a link resolving outside the
// boundary, the walk's own refusal of its files.
func clawAgentsMdSection(dir string, budget int, confined bool, boundaryReal string) (section string, ok bool) {
	if budget <= 0 {
		return "", false
	}
	for _, shadow := range []string{"CLAUDE.md", filepath.Join(".claude", "CLAUDE.md")} {
		if clawShadowPresent(filepath.Join(dir, shadow), confined, boundaryReal) {
			return "", false
		}
	}
	path := filepath.Join(dir, "AGENTS.md")
	var data []byte
	if confined {
		d, ok := clawReadWithin(path, boundaryReal)
		if !ok {
			return "", false
		}
		data = d
	} else {
		d, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		data = d
	}
	section = fmt.Sprintf("## AGENTS.md (%s)\n\n%s", path, clawStripFrontmatter(data))
	if len(section) > budget {
		section = section[:budget] + "\n... (truncated)"
	}
	return section, true
}

// clawShadowPresent reports whether path carries a CLAUDE.md the walk would
// load — present as a regular file and, where the walk confines its
// workspace scope, resolved inside the boundary. A link out of the workspace
// shadows nothing, exactly as the walk refuses to load it.
func clawShadowPresent(path string, confined bool, boundaryReal string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	return !confined || clawResolvesWithin(path, boundaryReal)
}

// clawResolvesWithin reports whether path, with its symlinks resolved, is
// boundaryReal or lies below it — the loader's resolvesWithin. An empty
// boundary admits nothing.
func clawResolvesWithin(path, boundaryReal string) bool {
	if boundaryReal == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return clawWithin(real, boundaryReal)
}

// clawWithin reports whether real, an already-resolved path, is dir or lies
// below it — the loader's within. An empty dir admits nothing.
func clawWithin(real, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// clawReadWithin reads path, swearing the bytes came from inside boundaryReal
// — the loader's readWithin: resolved once and opened at its resolution, the
// opened file compared by device and inode with what the resolution pointed
// at, so a file swapped in between drops the content instead of leaking it.
// A workspace-scope AGENTS.md that is itself a link out of the workspace is
// not loaded, like any workspace file of the walk.
func clawReadWithin(path, boundaryReal string) ([]byte, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !clawWithin(resolved, boundaryReal) {
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

// clawStripFrontmatter drops a leading YAML frontmatter block — the memory
// walk strips it from every file it injects, frontmatter being configuration
// rather than instructions. The accepted shape is the walk's parser's: an
// opening `---` line and a closing `---` line, one leading newline of the
// body dropped; anything else, an unterminated block included, is kept
// verbatim.
func clawStripFrontmatter(data []byte) string {
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return string(data)
	}
	closeIdx := bytes.Index(rest, []byte("---\n"))
	if closeIdx < 0 {
		if bytes.HasSuffix(rest, []byte("---")) {
			closeIdx = len(rest) - 3
		} else {
			return string(data)
		}
	}
	body := rest[closeIdx+3:]
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	}
	return string(body)
}

// clawJoinMemoryParts joins the walk's output and the fallback sections with
// the walk's own part separator, empty parts dropped, so the block reads as
// one project-instructions body whatever loaded. The fallback's parts land
// after the walk's output — the block-level order the ADR states.
func clawJoinMemoryParts(mem string, parts []string) string {
	if len(parts) == 0 {
		return mem
	}
	all := make([]string, 0, len(parts)+1)
	if mem != "" {
		all = append(all, mem)
	}
	return strings.Join(append(all, parts...), "\n\n---\n\n")
}
