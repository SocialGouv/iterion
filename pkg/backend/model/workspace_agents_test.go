package model

// The `agent` tool's subagent_type may name a file-backed agent:
// `<workspace>/.claude/agents/<name>.md`, the files a plugin's
// `contributes: agents` mirrors there and claude_code discovers natively
// (#1714). The resolver lives iterion-side (ARB-83): model.NewSubagentRunner
// is the only consumer of the spawn a workflow drives. Its frontmatter is
// honoured where it narrows (`tools:` intersects the node's effective set,
// never widens it) and refused where it would re-route the node's spend
// (`model:` — #1717). Containment is os.Root at the workspace root: a name
// cannot spell traversal, and a symlink out of the workspace is refused.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeWorkspaceAgent drops one agent file into the workspace's
// `.claude/agents/`, the same place mirrorPluginContributions writes a
// plugin's.
func writeWorkspaceAgent(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, ".claude", "agents")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}
}

// TestLookupWorkspaceAgentResolvesADeclaredAgent: the frontmatter's
// `description:` and `tools:` are read, the body is the agent's prompt, and
// Claude Code's tool spellings fold onto the node's tool names (`Read` is
// `read_file`). Red when nothing reads the file.
func TestLookupWorkspaceAgentResolvesADeclaredAgent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "auditor", "---\ndescription: Audits a change for regressions\ntools: Read, Grep\n---\nYou are the auditor. Report findings only.\n")
	a, found, err := LookupWorkspaceAgent(dir, "auditor", 0)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found {
		t.Fatal("a declared agent file was not found")
	}
	if a.Description != "Audits a change for regressions" {
		t.Errorf("description = %q", a.Description)
	}
	if a.Body != "You are the auditor. Report findings only." {
		t.Errorf("body = %q", a.Body)
	}
	if got := strings.Join(a.Tools, ","); got != "grep,read_file" {
		t.Errorf("tools = %q, want the sorted, translated [grep,read_file]", got)
	}
	if a.Model != "" {
		t.Errorf("model = %q, want empty (none declared)", a.Model)
	}
}

// TestLookupWorkspaceAgentReadsToolsAsAList: the YAML-list spelling of
// `tools:` parses like the comma-separated one.
func TestLookupWorkspaceAgentReadsToolsAsAList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "listed", "---\ntools:\n  - read_file\n  - bash\n---\nbody\n")
	a, found, err := LookupWorkspaceAgent(dir, "listed", 0)
	if err != nil || !found {
		t.Fatalf("lookup: found=%t err=%v", found, err)
	}
	if got := strings.Join(a.Tools, ","); got != "bash,read_file" {
		t.Errorf("tools = %q, want [bash,read_file]", got)
	}
}

// TestAWorkspaceAgentNameCannotSpellTraversal: the name is a single path
// segment by construction — `..`, separators and dot-runs are refused before
// any I/O, so the model's own subagent_type cannot walk out of
// `.claude/agents/`. os.Root backs the guarantee; the charset keeps the name
// from expressing what the root would refuse.
func TestAWorkspaceAgentNameCannotSpellTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.md"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret", "..", "a/b", `..\secret`, "."} {
		if _, found, err := LookupWorkspaceAgent(dir, name, 0); err == nil || found {
			t.Errorf("LookupWorkspaceAgent(%q) = found=%t err=%v — a traversal spelling must be refused", name, found, err)
		}
	}
}

// TestAWorkspaceAgentSymlinkedOutOfTheWorkspaceIsRefused: a repository under
// review ships `.claude/agents/leak.md` as a symlink to a host file. The read
// goes through os.Root, so the answer is a refusal, never the target.
func TestAWorkspaceAgentSymlinkedOutOfTheWorkspaceIsRefused(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege Windows sessions lack")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, ".claude", "agents", "leak.md")); err != nil {
		t.Fatal(err)
	}
	if _, found, err := LookupWorkspaceAgent(dir, "leak", 0); err == nil || found {
		t.Errorf("a symlink out of the workspace resolved: found=%t err=%v", found, err)
	}
}

// TestAnUnknownWorkspaceAgentNameIsAbsentNotAnError: no file is "not found",
// never a failure — a workspace contributes agents optionally, and the
// builtin subagent-type behaviour carries on below.
func TestAnUnknownWorkspaceAgentNameIsAbsentNotAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, found, err := LookupWorkspaceAgent(dir, "ghost", 0); err != nil || found {
		t.Errorf("no .claude/agents at all: found=%t err=%v", found, err)
	}
	writeWorkspaceAgent(t, dir, "auditor", "body\n")
	if _, found, err := LookupWorkspaceAgent(dir, "ghost", 0); err != nil || found {
		t.Errorf("a missing name beside a present one: found=%t err=%v", found, err)
	}
}

// TestAnOversizedWorkspaceAgentIsRefusedAtTheCeiling: the file is a checkout's
// untrusted input read inside a tool call; past the byte ceiling the read is
// refused rather than held.
func TestAnOversizedWorkspaceAgentIsRefusedAtTheCeiling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "fat", strings.Repeat("x", 64))
	if _, found, err := LookupWorkspaceAgent(dir, "fat", 16); !errors.Is(err, ErrWorkspaceAgentTooLarge) || found {
		t.Errorf("over the ceiling: found=%t err=%v, want ErrWorkspaceAgentTooLarge", found, err)
	}
	if _, found, err := LookupWorkspaceAgent(dir, "fat", 0); err != nil || !found {
		t.Errorf("unbounded read of the same file: found=%t err=%v", found, err)
	}
}

// TestANamedWorkspaceAgentRunsOnItsOwnPrompt: `subagent_type: auditor` with
// `.claude/agents/auditor.md` in the node's workspace resolves the file: the
// body is the child's system prompt (not the generic one) and the result says
// where the agent came from. Red when the runner never consults the
// workspace — the shape that left a plugin-contributed agent inert on claw.
func TestANamedWorkspaceAgentRunsOnItsOwnPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "auditor", "---\ndescription: Audits a change\n---\nAUDIT EVERYTHING. Report findings only.\n")
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file")},
		WorkDir: dir,
	})
	out, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "auditor"})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if got := client.system(t, 1); got != "AUDIT EVERYTHING. Report findings only." {
		t.Errorf("the child's system prompt = %q, want the agent file's body", got)
	}
	if !strings.Contains(out, `"workspace_agent": "auditor"`) {
		t.Errorf("the result does not name the resolved workspace agent: %s", out)
	}
	// No `tools:` key in the file: the child keeps the node's effective set.
	if offered := client.offered(t, 1); len(offered) != 1 || offered[0] != "read_file" {
		t.Errorf("an agent without tools: was offered %v, want the node's [read_file]", offered)
	}
}

// TestAWorkspaceAgentsToolsOnlyEverNarrowTheNodesSet is the intersection
// witness, both directions: a file asking for MORE than the node holds does
// not widen the child, and a file asking for less narrows it — with Claude
// Code spellings (`Read`, `Bash`) landing on the node's own tool names. Red
// when the frontmatter set replaces (or ignores) the node's instead of
// intersecting it.
func TestAWorkspaceAgentsToolsOnlyEverNarrowTheNodesSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "auditor", "---\ntools: Read, Bash\n---\nbody\n")
	run, client := parentLoopRunner(t, "")

	// The node holds read_file alone; the file asks for read_file AND bash.
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file")},
		WorkDir: dir,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "auditor"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := client.offered(t, 1); len(offered) != 1 || offered[0] != "read_file" {
		t.Errorf("the file asked past the node's set and the child was offered %v", offered)
	}

	// The node holds both; the file's `tools: Read, Bash` keeps both...
	ctx = WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file"), parentTool("bash")},
		WorkDir: dir,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "auditor"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := strings.Join(client.offered(t, 2), ","); offered != "bash,read_file" && offered != "read_file,bash" {
		t.Errorf("tools: Read, Bash under a [read_file, bash] node offered %v", offered)
	}
	// ...while `tools: Read` narrows the child to read_file.
	writeWorkspaceAgent(t, dir, "reader", "---\ntools: Read\n---\nbody\n")
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "reader"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := client.offered(t, 3); len(offered) != 1 || offered[0] != "read_file" {
		t.Errorf("tools: Read under a [read_file, bash] node offered %v", offered)
	}
}

// TestAWorkspaceAgentShadowingABuiltinKeepsBothNarrowings: a file named like
// a builtin type (`explore`) wins the prompt, but the child stays inside the
// builtin's allowlist AND the file's — each is a narrowing, and a narrowing
// never widens. Red when the file's set replaces the builtin's: `tools:
// read_file, bash` would then hand `explore` a shell it never had.
func TestAWorkspaceAgentShadowingABuiltinKeepsBothNarrowings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "explore", "---\ntools: read_file, bash\n---\nshadow body\n")
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file"), parentTool("bash")},
		WorkDir: dir,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "explore"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := client.offered(t, 1); len(offered) != 1 || offered[0] != "read_file" {
		t.Errorf("explore shadowed by a file asking for bash offered %v — the builtin narrowing was dropped", offered)
	}
	if got := client.system(t, 1); got != "shadow body" {
		t.Errorf("the shadowing file's body did not serve: %q", got)
	}
}

// TestAWorkspaceAgentDeclaringModelIsRefused: `model:` would re-route the
// node's spend on a contributed file's word, and the value cannot pass
// claw-code-go today (#1717) — so the delegation is refused, naming the key,
// the file and the ticket, and no child conversation starts. Red when the
// key is silently ignored, the shape where a repository picks the model a
// node bills.
func TestAWorkspaceAgentDeclaringModelIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "pricey", "---\nmodel: claude-opus-4-6\n---\nbody\n")
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file")},
		WorkDir: dir,
	})
	_, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "pricey"})
	if !errors.Is(err, ErrWorkspaceAgentModel) {
		t.Fatalf("a model:-declaring agent was not refused: %v", err)
	}
	for _, want := range []string{"pricey", "claude-opus-4-6", "#1717"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if got := client.callCount(); got != 0 {
		t.Errorf("the refused delegation still started %d child call(s)", got)
	}
}

// TestAWorkspaceAgentTraversalNameNeverResolves: through the runner this
// time — the refusal reaches the delegating model as a tool error rather
// than a spawned child.
func TestAWorkspaceAgentTraversalNameNeverResolves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file")},
		WorkDir: dir,
	})
	if _, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "../settings"}); err == nil {
		t.Fatal("a traversal-spelled subagent_type spawned a child")
	}
	if got := client.callCount(); got != 0 {
		t.Errorf("the refused delegation started %d child call(s)", got)
	}
}

// TestAnUnknownSubagentTypeKeepsTheBuiltinShape: no file by that name — the
// delegation is exactly what it was before the resolver existed: the node's
// whole effective set and the generic prompt. The resolver adds a surface; it
// must not tax the one that was there.
func TestAnUnknownSubagentTypeKeepsTheBuiltinShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "auditor", "body\n")
	run, client := parentLoopRunner(t, "")
	ctx := WithParentLoop(context.Background(), ParentLoop{
		Model:   "test/parent-model",
		Tools:   []GenerationTool{parentTool("read_file"), parentTool("bash")},
		WorkDir: dir,
	})
	out, err := run(ctx, map[string]any{"description": "probe", "prompt": "go", "subagent_type": "ghost"})
	if err != nil {
		t.Fatalf("subagent: %v", err)
	}
	if offered := strings.Join(client.offered(t, 1), ","); !strings.Contains(offered, "read_file") || !strings.Contains(offered, "bash") {
		t.Errorf("an unknown type under a workspace carrying agents was offered %v", offered)
	}
	if strings.Contains(out, "workspace_agent") {
		t.Errorf("an unknown type reported a workspace agent: %s", out)
	}
}
