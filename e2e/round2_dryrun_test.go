package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dryrun"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// `iterion validate --exec` on catalog bots: the dry run answers every node
// with a schema shape; a `file` field's shape is "" (dryrun/synth.go:55).
func TestR2DryRunOnCatalogBots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, strict := range []string{"", "true"} {
		t.Setenv("ITERION_STRICT_OUTPUT_VALIDATION", strict)
		for _, rel := range []string{"../examples/human-file-upload.bot"} {
			u, err := unit.LoadDir(rel)
			if err != nil {
				t.Fatal(err)
			}
			cr := ir.Compile(u.Merged)
			if cr.HasErrors() {
				t.Fatalf("compile %s", rel)
			}
			abs, _ := filepath.Abs(rel)
			rep, err := dryrun.Run(context.Background(), cr.Workflow, dryrun.Options{Path: abs, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("dryrun: %v", err)
			}
			for _, p := range rep.Passes {
				if p.Bias && p.Status != "finished" || strings.Contains(p.Failure, "SCHEMA_VALIDATION") {
					t.Errorf("dry-run file shape rejected in strict=%q: %s", strict, p.Failure)
				}
				t.Logf("%s pass bias=%v status=%s failure=%.220q", rel, p.Bias, p.Status, p.Failure)
			}
			t.Logf("%s clean=%v failing=%v", rel, rep.Clean(), rep.Failing())
		}
	}
}
