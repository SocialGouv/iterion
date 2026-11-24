package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #2650 — on the claw backend, read_file / glob / grep refused any path
// outside the workspace, so a bot that keeps its run directory under
// ${PROJECT_SCRATCH_DIR} (out of the audited tree BY DESIGN) lost every
// brief on claw while claude_code's unbounded Read reached them (rgaa-audit
// run 01a15f60: 0 briefs read, 19 batches lost). The run scratch is now a
// READ-ONLY root the host attaches to the scope; the read tools accept it,
// writes stay workspace-only, and the credential exclusions hold under
// every root.
//
// Each "before" assertion below is the ticket's witness: against a
// workspace-only scope (the pre-fix behaviour) the same call fails with
// "outside workspace".

// briefFixture plants the rgaa-audit-shaped brief under a scratch dir that
// is deliberately OUTSIDE the workspace, and returns both paths.
func briefFixture(t *testing.T) (workspace, scratch, brief string) {
	t.Helper()
	workspace = t.TempDir()
	scratch = t.TempDir()
	if strings.HasPrefix(scratch, workspace) {
		t.Fatal("fixture invariant broken: the scratch must lie outside the workspace")
	}
	dir := filepath.Join(scratch, "rgaa-audit", "run-01", "briefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	brief = filepath.Join(dir, "brief-lot-01.md")
	if err := os.WriteFile(brief, []byte("# Brief lot 01\ncritère 1.1 — images\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace, scratch, brief
}

func TestReadScopeScratchBriefIsReadableOnlyOnceAttached(t *testing.T) {
	workspace, scratch, brief := briefFixture(t)
	scope := NewReadScope(workspace)

	// Before: the pre-fix refusal, verbatim.
	if _, err := executeWorkspaceReadFile(map[string]any{"path": brief}, scope); err == nil ||
		!strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("without the scratch root, read_file(%q) must refuse with 'outside workspace', got %v", brief, err)
	}

	scope.AddExtraRoot(scratch)

	out, err := executeWorkspaceReadFile(map[string]any{"path": brief}, scope)
	if err != nil {
		t.Fatalf("with the scratch root attached, read_file(%q) refused: %v", brief, err)
	}
	if !strings.Contains(out, "critère 1.1") {
		t.Fatalf("read_file returned the wrong content: %q", out)
	}
}

func TestReadScopeGlobAndGrepReachTheScratch(t *testing.T) {
	workspace, scratch, brief := briefFixture(t)
	scope := NewReadScope(workspace)
	ctx := context.Background()

	globIn := map[string]any{"path": filepath.Join(scratch, "rgaa-audit"), "pattern": "**/*.md"}
	if _, err := executeWorkspaceGlob(ctx, globIn, scope); err == nil ||
		!strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("without the scratch root, glob must refuse with 'outside workspace', got %v", err)
	}
	grepIn := map[string]any{"path": scratch, "pattern": "critère"}
	if _, err := executeWorkspaceGrep(ctx, grepIn, scope); err == nil ||
		!strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("without the scratch root, grep must refuse with 'outside workspace', got %v", err)
	}

	scope.AddExtraRoot(scratch)

	out, err := executeWorkspaceGlob(ctx, globIn, scope)
	if err != nil || !strings.Contains(out, "brief-lot-01.md") {
		t.Fatalf("glob over the scratch root: out=%q err=%v", out, err)
	}
	out, err = executeWorkspaceGrep(ctx, grepIn, scope)
	if err != nil || !strings.Contains(out, brief+":2:") {
		t.Fatalf("grep over the scratch root: out=%q err=%v", out, err)
	}
}

func TestReadScopeLeavesTheWriteToolsUntouched(t *testing.T) {
	workspace, scratch, _ := briefFixture(t)
	scope := NewReadScope(workspace)

	reg := NewRegistry()
	if err := RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	write, err := reg.Resolve("write_file")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scratch, "rgaa-audit", "run-01", "briefs", "note.md")
	input := []byte(`{"path": "` + strings.ReplaceAll(target, `\`, `\\`) + `", "content": "x"}`)

	// The scope widens READS only: write_file's contract (relative paths
	// rebase onto the workspace, absolute paths pass through) is the same
	// before and after the scratch root attaches — attaching it twice must
	// change nothing, and nothing here is a write CONTAINMENT promise
	// (claw's write tools never had one; claude_code's Write has none
	// either — parity).
	if _, err := write.Execute(context.Background(), input); err != nil {
		t.Fatalf("write_file(%q) before the root: %v", target, err)
	}
	scope.AddExtraRoot(scratch)
	if _, err := write.Execute(context.Background(), input); err != nil {
		t.Fatalf("write_file(%q) after the root: the read scope must not change writes: %v", target, err)
	}
}

func TestReadScopeKeepsTheCredentialExclusionUnderTheScratch(t *testing.T) {
	workspace, scratch, _ := briefFixture(t)
	scope := NewReadScope(workspace)
	scope.AddExtraRoot(scratch)

	secret := filepath.Join(scratch, "rgaa-audit", "run-01", ".env")
	if err := os.WriteFile(secret, []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeWorkspaceReadFile(map[string]any{"path": secret}, scope); err == nil ||
		!strings.Contains(err.Error(), "credential") {
		t.Fatalf(".env under the scratch root must stay excluded, got %v", err)
	}
}

func TestReadScopeStillRefusesEverywhereElse(t *testing.T) {
	workspace, scratch, _ := briefFixture(t)
	scope := NewReadScope(workspace)
	scope.AddExtraRoot(scratch)

	elsewhere := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(elsewhere, []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := executeWorkspaceReadFile(map[string]any{"path": elsewhere}, scope); err == nil ||
		!strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("a path under neither root must still refuse, got %v", err)
	}
	// Relative paths still anchor at the workspace, never at an extra root.
	if err := os.WriteFile(filepath.Join(workspace, "in-ws.txt"), []byte("ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := executeWorkspaceReadFile(map[string]any{"path": "in-ws.txt"}, scope); err != nil || out != "ws\n" {
		t.Fatalf("relative read_file must keep resolving against the workspace: out=%q err=%v", out, err)
	}
	if _, err := executeWorkspaceReadFile(map[string]any{"path": "rgaa-audit/run-01/briefs/brief-lot-01.md"}, scope); err == nil {
		t.Fatal("a relative path must not fall through to the scratch root")
	}
}

func TestReadScopeAddExtraRootIsIdempotentAndCanonical(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	scope := NewReadScope(workspace)
	scope.AddExtraRoot(scratch)
	scope.AddExtraRoot(scratch)
	scope.AddExtraRoot("")
	scope.AddExtraRoot(workspace) // the workspace is already a root
	if got := scope.ExtraRoots(); len(got) != 1 {
		t.Fatalf("ExtraRoots = %v, want exactly the scratch dir once", got)
	}
	// A symlinked spelling of the same root must not stack a second entry.
	link := filepath.Join(t.TempDir(), "scratch-link")
	if err := os.Symlink(scratch, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	scope.AddExtraRoot(link)
	if got := scope.ExtraRoots(); len(got) != 1 {
		t.Fatalf("ExtraRoots after symlink add = %v, want the canonical root once", got)
	}
}
