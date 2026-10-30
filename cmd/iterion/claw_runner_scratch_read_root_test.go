package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The container half of #2650 (#2874): registerSandboxLocalTools registers
// the run scratch the launcher crossed on the task envelope
// (delegate.IOTask.ScratchDir) as an extra READ root of the in-container
// read tools, so a sandboxed claw node reads the briefs the run wrote under
// ${PROJECT_SCRATCH_DIR} — outside the workspace bind, where read_file
// otherwise refuses. The host half of the same root carries its own witness
// (pkg/runtime/scratch_read_root_launch_test.go); this one pins the runner
// side, which a deletion of the AddExtraRoot call in
// registerSandboxLocalTools must redden.
func TestRegisterSandboxLocalTools_ScratchDirIsAReadRoot(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()

	// The same relative name under BOTH roots, different content: a read
	// answered with the decoy proves the scope silently reopened the
	// workspace's homonym instead of the scratch file.
	const scratchBody = "SCRATCH-BRIEF\n"
	for _, plant := range []struct{ root, body string }{
		{workspace, "WORKSPACE-DECOY\n"},
		{scratch, scratchBody},
	} {
		dir := filepath.Join(plant.root, "briefs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lot-01.md"), []byte(plant.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	readFile := func(t *testing.T, reg *tool.Registry, path string) (string, error) {
		t.Helper()
		td, err := reg.Resolve("read_file")
		if err != nil {
			t.Fatalf("resolve read_file: %v", err)
		}
		raw, err := json.Marshal(map[string]any{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		return td.Execute(context.Background(), raw)
	}

	// The scope the proxy builds carries the scratch: the brief reads.
	reg := tool.NewRegistry()
	if err := registerSandboxLocalTools(reg, workspace, scratch, nil, 0); err != nil {
		t.Fatalf("registerSandboxLocalTools: %v", err)
	}
	brief := filepath.Join(scratch, "briefs", "lot-01.md")
	out, err := readFile(t, reg, brief)
	if err != nil {
		t.Fatalf("read_file(%q) refused: %v — the scratch dir the launcher crossed is not a read root", brief, err)
	}
	if !strings.Contains(out, scratchBody) {
		t.Fatalf("read_file(%q) = %q, want the scratch file, not the workspace's homonym", brief, out)
	}

	// The root narrows, not opens: a path under NEITHER root still refuses.
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(t, reg, outside); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("read_file(%q) = %v, want an outside-workspace refusal — the extra root must not widen to everything", outside, err)
	}

	// The control the mutation leans on: with NO scratch crossed (the
	// pre-#2650 envelope, or the AddExtraRoot call removed), the same brief
	// is outside every root and the read refuses.
	bare := tool.NewRegistry()
	if err := registerSandboxLocalTools(bare, workspace, "", nil, 0); err != nil {
		t.Fatalf("registerSandboxLocalTools without scratch: %v", err)
	}
	if _, err := readFile(t, bare, brief); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("without the scratch root, read_file(%q) = %v, want a refusal", brief, err)
	}
}
