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
// own native mechanism, or refuses a policy it has none for (SupportOf). The
// repository's settings, skills, commands and hooks are not governed here:
// they carry the engine's own mirrored skills and plugin contributions, and
// keep loading under every policy.
//
// The package is a leaf so the DSL compiler can import SupportOf for C185 and
// C187.
package ambient

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// Support is how one backend handles one policy.
type Support uint8

const (
	// Unmeasured: nothing has measured what the backend loads natively
	// against this policy. It keeps its own conventions; the compiler warns
	// on a declared value (C185) and the engine logs one at dispatch.
	Unmeasured Support = iota
	// Honoured: the backend translates the policy into its own mechanism.
	Honoured
	// Refused: the backend is known to load, whatever iterion passes it,
	// instruction files the policy excludes. A declared value is refused, at
	// compile time (C187) and at dispatch, rather than ignored; the default
	// is applied as far as the backend can.
	Refused
)

// verdict is one backend's answer to one policy.
type verdict struct {
	support Support
	// reason says, for a refusal, what the backend loads regardless.
	reason string
}

var allHonoured = [4]verdict{Workspace: {support: Honoured}, None: {support: Honoured}, Operator: {support: Honoured}, All: {support: Honoured}}

const (
	kimiWalk = "kimi reads every AGENTS.md from the repository root down to the working directory, and has no switch that skips them"

	opencodeOperatorFiles = "opencode reads its global AGENTS.md and the files its global configuration lists under `instructions`, and opencode 2 walks for AGENTS.md past the repository root up to the home directory; none of it can be switched off"
	opencodeNestedFiles   = "opencode adds the AGENTS.md of every directory it reads a file in, whatever its configuration says"

	grokProjectFiles = "grok reads AGENTS.md, CLAUDE.md and the .grok, .claude and .cursor rules from the repository root down to the working directory whenever it trusts the folder, and it trusts a repository that carries no hook or MCP configuration without asking"
)

// backendVerdicts records how each backend handles each policy, indexed by
// Policy. A backend absent from it is Unmeasured for every policy. The names
// are the delegate.Backend* values, spelled literally to keep this package a
// leaf.
//
// Honoured is earned by a measurement against the real CLI (the clilive tests
// in pkg/backend/delegate). A refusal needs less: a policy is refused as soon
// as the backend's own loader is seen to have no switch for it, since refusing
// can never hand a node a file its policy excludes.
var backendVerdicts = map[string][4]verdict{
	"claude_code": allHonoured,
	"claw":        allHonoured,
	"codex":       allHonoured,
	"pi":          allHonoured,
	// kimi-code 2.1.1: $KIMI_CODE_HOME/AGENTS.md and ~/.agents/AGENTS.md
	// are the operator's, the walk from the git root down to the working
	// directory is the workspace's (TestCLILiveKimiAmbientPolicies).
	// Workspace serves kimi a home without its AGENTS.md (kimi.go).
	"kimi": {
		Workspace: {support: Honoured},
		None:      {support: Refused, reason: kimiWalk},
		Operator:  {support: Refused, reason: kimiWalk},
		All:       {support: Honoured},
	},
	// opencode 1.19.0 and 2.0.26, read from source: All is its native
	// loading, minus the Claude-Code compatibility layer iterion pins off
	// (OPENCODE_DISABLE_CLAUDE_CODE).
	"opencode": {
		Workspace: {support: Refused, reason: opencodeOperatorFiles},
		None:      {support: Refused, reason: opencodeNestedFiles},
		Operator:  {support: Refused, reason: opencodeNestedFiles},
		All:       {support: Honoured},
	},
	// Grok Build, read from source (xai-org/grok-build@2bdd1d6). What it
	// reads from GROK_HOME and the vendor homes has not been measured, and
	// under permission: deny it runs with a private HOME.
	"grok": {
		None:     {support: Refused, reason: grokProjectFiles},
		Operator: {support: Refused, reason: grokProjectFiles},
	},
}

// SupportOf reports how a backend handles a policy and, for a refusal, why.
func SupportOf(backend string, p Policy) (Support, string) {
	verdicts, ok := backendVerdicts[backend]
	if !ok || int(p) >= len(verdicts) {
		return Unmeasured, ""
	}
	v := verdicts[p]
	return v.support, v.reason
}

// Enforces reports whether a backend applies a policy.
func Enforces(backend string, p Policy) bool {
	s, _ := SupportOf(backend, p)
	return s == Honoured
}

// EnforcingBackends lists, sorted, the backends that apply a policy.
func EnforcingBackends(p Policy) []string {
	var out []string
	for b := range backendVerdicts {
		if Enforces(b, p) {
			out = append(out, b)
		}
	}
	slices.Sort(out)
	return out
}

// HonouredBy lists, in Values order, the policies a backend applies.
func HonouredBy(backend string) []string {
	var out []string
	for _, v := range Values {
		if p, _ := Parse(v); Enforces(backend, p) {
			out = append(out, v)
		}
	}
	return out
}

// Declared reports whether a policy resolved from source (ResolveSourced) was
// chosen by someone rather than defaulted. "" counts as the default, so a task
// built outside the executor reads as undeclared.
func Declared(source string) bool { return source != "" && source != "default" }

// Capability names a declared policy the way a refusal shows it:
// "ambient_context: none (from node)".
func Capability(p Policy, source string) string {
	return fmt.Sprintf("ambient_context: %s (from %s)", p, source)
}

// Remedy is what a refusal of p on backend tells the operator: what the
// backend loads regardless (reason), and the ways out.
func Remedy(backend string, p Policy, reason string) string {
	out := "drop the declared value"
	if honoured := HonouredBy(backend); len(honoured) > 0 {
		out = "declare a policy it applies (" + strings.Join(honoured, ", ") + ")"
	}
	return fmt.Sprintf("%s; %s, or run the node on a backend that applies %s (%s) — ADR-119",
		reason, out, p, strings.Join(EnforcingBackends(p), ", "))
}

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
