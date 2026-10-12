// Package ambient is iterion's switch for what an agent node inherits from its
// surroundings besides its prompt (ADR-119). Two origins exist:
//
//   - the workspace: the instruction files of the repository the node works
//     in (CLAUDE.md, .claude/rules/, AGENTS.md …), from the working directory
//     up to the repository root, never above it;
//   - the operator: the operator's personal agent setup (the agent home:
//     ~/.claude or $CLAUDE_CONFIG_DIR, $CODEX_HOME, pi's agent directory) and
//     the instruction files of the directories above the repository root.
//
// The DSL field `ambient_context: none | workspace | operator | all` picks
// which origins a node receives; every backend translates the Policy into its
// own native mechanism. The repository's settings, skills, commands and hooks
// are not governed here: they carry the engine's own mirrored skills and
// plugin contributions, and keep loading under every policy.
//
// The package is a leaf so the DSL compiler can import Enforces for C185.
package ambient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PolicyEnv sets the process-wide default policy when neither a run override,
// a node nor the workflow declares one.
const PolicyEnv = "ITERION_AMBIENT_CONTEXT"

// Policy is the resolved ambient-context setting of a node. The zero value is
// Workspace, the default, so a task built without a declared policy gets the
// default rather than an accidental "everything".
type Policy uint8

const (
	// Workspace gives the node the repository's instruction files only.
	Workspace Policy = iota
	// None gives the node nothing but its prompt.
	None
	// Operator gives the node the operator's setup and the instruction files
	// above the repository root, without the repository's own.
	Operator
	// All gives the node both origins, which is what a native harness started
	// by hand in the same directory would load.
	All
)

// Values lists the canonical spellings, in the order the docs present them.
var Values = []string{"none", "workspace", "operator", "all"}

func (p Policy) String() string {
	switch p {
	case None:
		return "none"
	case Operator:
		return "operator"
	case All:
		return "all"
	}
	return "workspace"
}

// IncludesWorkspace reports whether the repository's instruction files reach
// the node.
func (p Policy) IncludesWorkspace() bool { return p == Workspace || p == All }

// IncludesOperator reports whether the operator's setup, and the instruction
// files above the repository root, reach the node.
func (p Policy) IncludesOperator() bool { return p == Operator || p == All }

// Parse maps a DSL, CLI or environment spelling to a Policy. Matching is
// case-insensitive and ignores surrounding space. ok is false for anything
// else, the empty string included.
func Parse(s string) (Policy, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return None, true
	case "workspace":
		return Workspace, true
	case "operator":
		return Operator, true
	case "all":
		return All, true
	}
	return Workspace, false
}

// Validate rejects a value typed by hand on the CLI or the launch API. The
// empty string means "not set" and is accepted. The DSL gets the same
// guarantee from the compiler (C184).
func Validate(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if _, ok := Parse(s); !ok {
		return fmt.Errorf("invalid ambient context %q: expected one of %s", s, strings.Join(Values, ", "))
	}
	return nil
}

// ResolveSourced picks the effective policy from iterion's precedence chain,
// highest priority first: run override > node > workflow > environment >
// Workspace. It also returns the winning level ("run_override" | "node" |
// "workflow" | "env" | "default"), which the studio's settings-provenance
// caption shows.
//
// An unrecognised value never wins: the compiler and Validate reject one
// before it reaches a node or a run override, and an invalid environment
// value is a machine default that must not abort a run it was merely present
// for. The executor logs that case once (InvalidEnv).
func ResolveSourced(override, node, workflow, envDefault string) (Policy, string) {
	levels := []struct {
		value  string
		source string
	}{
		{override, "run_override"},
		{node, "node"},
		{workflow, "workflow"},
		{envDefault, "env"},
	}
	for _, l := range levels {
		if p, ok := Parse(l.value); ok {
			return p, l.source
		}
	}
	return Workspace, "default"
}

// InvalidEnv reports a non-empty environment value that ResolveSourced will
// ignore, so the executor can say so instead of failing silently.
func InvalidEnv(envDefault string) bool {
	if strings.TrimSpace(envDefault) == "" {
		return false
	}
	_, ok := Parse(envDefault)
	return !ok
}

// enforcingBackends lists the backends that translate every Policy into a
// native mechanism. A backend absent from it keeps its own conventions, so an
// explicit policy there is worth a compile-time warning (C185). The names are
// the delegate.Backend* values, spelled literally to keep this package a leaf.
var enforcingBackends = map[string]bool{
	"claude_code": true,
	"claw":        true,
	"codex":       true,
	"pi":          true,
}

// Enforces reports whether a backend applies the declared policy.
func Enforces(backend string) bool { return enforcingBackends[backend] }

// RepoRoot returns the root of the repository holding workDir: the nearest
// directory, workDir included, that carries a `.git` entry. That entry is a
// directory in a primary checkout and a file in a linked worktree or a
// submodule, so a run worktree is its own root. Outside any repository the
// root is workDir itself. The result is absolute and clean; symlinks are kept
// as given, and callers that match paths should also try the resolved form
// (see Forms).
func RepoRoot(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return filepath.Clean(workDir)
	}
	for dir := abs; ; {
		if isGitEntry(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		dir = parent
	}
}

// IsRepoRoot reports whether dir itself carries a `.git` entry — the
// boundary RepoRoot walks to. A walk that must not leave the repository
// (the bundle a workflow file belongs to) checks each level with this and
// stops at the first that answers true.
func IsRepoRoot(dir string) bool {
	return isGitEntry(filepath.Join(dir, ".git"))
}

// isGitEntry accepts a `.git` directory, or a `.git` file that points at one
// ("gitdir: …"). An empty placeholder file, which some sandboxes plant on
// paths they deny, is not a repository.
func isGitEntry(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return true
	}
	head := make([]byte, len("gitdir:"))
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	n, _ := f.Read(head)
	return string(head[:n]) == "gitdir:"
}

// OperatorBoundary is the directory above which instruction files count as
// the operator's. It is the repository root, except for a linked worktree
// nested inside its own main checkout (a run worktree under
// .iterion/worktrees/, a session's under .claude/worktrees/): the main
// checkout is the same repository, so its instruction files are repository
// content — never the operator's — and the boundary moves up to it.
func OperatorBoundary(root string) string {
	if main, ok := NestedWorktreeMain(root); ok {
		return main
	}
	return root
}

// NestedWorktreeMain reports the main checkout of root when root is a linked
// worktree that sits inside that checkout. It follows git's own pointers: the
// `.git` file's gitdir, then that gitdir's `commondir`. A worktree outside
// its main checkout, a submodule (no commondir) or a bare layout (the common
// dir is not `<main>/.git`) is not nested.
func NestedWorktreeMain(root string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return "", false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", false
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	rel, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(rel))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "", false
	}
	main := filepath.Dir(common)
	// git writes gitdir resolved while root may be spelled through a symlink
	// (macOS /tmp -> /private/tmp): compare the resolved forms.
	if !strings.HasPrefix(resolved(root), resolved(main)+string(filepath.Separator)) {
		return "", false
	}
	return main, true
}

// AncestorsAbove returns the directories strictly above root, nearest first,
// up to and including the filesystem root. root must be absolute.
func AncestorsAbove(root string) []string {
	var out []string
	for dir := filepath.Clean(root); ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
		}
		out = append(out, parent)
		dir = parent
	}
}

// Forms returns path and, when it differs, its symlink-resolved form. A
// harness may report a memory file under either spelling (on macOS /tmp is
// /private/tmp), so a path-based exclusion must name both.
func Forms(path string) []string {
	forms := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		forms = append(forms, resolved)
	}
	return forms
}

// resolved is path with its symlinks resolved, or path itself when that fails.
func resolved(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return filepath.Clean(path)
}
