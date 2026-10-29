package bots

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// #2650 — rgaa-audit keeps its run directory under ${PROJECT_SCRATCH_DIR}
// ("everything intermediate stays OUT of the audited tree", by design) and
// its adjudicate/refute agents read their briefs from there with read_file.
// On claw that read refused every path outside the workspace — run 01a15f60
// read 0 briefs and burned 19 batches. The fix is the engine-side read
// root: the run scratch joins the claw read scope. This test is the
// contract between the bot's layout and that seam:
//
//   - the bot's run_dir default must stay under ${PROJECT_SCRATCH_DIR}
//     (the demo workaround — run_dir inside the workspace — is proscribed:
//     it costs resumability, only the banked scratch survives a teardown);
//   - a brief at the bot's own layout (<run_dir>/<run id>/briefs/<id>.md)
//     reads through a claw registry whose scope carries the scratch root;
//   - mutation: drop the root (the pre-fix scope) and the same read rouges
//     on "outside workspace", exactly as the lost run saw it.
//
// A real claw NODE reading the brief is the live layer's job; what CI can
// pin deterministically is the seam that node's tools go through.
func TestRgaaAuditScratchBriefsAreClawReadable(t *testing.T) {
	wf := compilePlanPhaseBot(t, "rgaa-audit")

	v, ok := wf.Vars["run_dir"]
	if !ok || !v.HasDefault {
		t.Fatal("rgaa-audit declares no run_dir default")
	}
	def := fmt.Sprint(v.Default)
	if !strings.HasPrefix(def, "${PROJECT_SCRATCH_DIR}/") {
		t.Fatalf("run_dir default = %q: the run directory must stay under ${PROJECT_SCRATCH_DIR} — "+
			"moving it into the workspace is the proscribed demo workaround (it breaks resume, #2650)", def)
	}

	// The bot's layout: prepare joins run_dir with the run id, the worklist
	// hands agents <RUN>/briefs and <RUN>/items.
	workspace := t.TempDir()
	scratch := t.TempDir()
	brief := filepath.Join(scratch, "rgaa-audit", "run-01a15f60", "briefs", "1.1.md")
	if err := os.MkdirAll(filepath.Dir(brief), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brief, []byte("# 1.1 Images — tests\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scope := tool.NewReadScope(workspace)
	reg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(reg, scope, nil); err != nil {
		t.Fatal(err)
	}
	read, err := reg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	readBrief := func() (string, error) {
		return read.Execute(context.Background(), []byte(`{"path": "`+brief+`"}`))
	}

	// Mutation / witness: the pre-fix scope (no scratch root) refuses the
	// brief with the exact error the 19 lost batches hit.
	if _, err := readBrief(); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("without the scratch root, read_file(%q) must refuse with 'outside workspace', got %v", brief, err)
	}

	// The engine's SetScratchDir push, replayed.
	scope.AddExtraRoot(scratch)
	out, err := readBrief()
	if err != nil {
		t.Fatalf("with the scratch root, read_file(%q) refused: %v", brief, err)
	}
	if !strings.Contains(out, "1.1 Images") {
		t.Fatalf("read_file returned the wrong brief content: %q", out)
	}
}
