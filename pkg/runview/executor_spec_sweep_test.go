package runview

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// executorSpecSitesMissing returns every non-test file in the repository that
// builds an ExecutorSpec without setting `field`, skipping the allowlist.
//
// One traversal, several fields: each field of this spec that must be decided
// at EVERY construction site gets its own test over this sweep. Sites are
// reported by repo-relative path, so a failure names the file to fix.
func executorSpecSitesMissing(t *testing.T, field string, exempt map[string]string) []string {
	t.Helper()

	repoRoot := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A directory carrying its own .git is a NESTED CHECKOUT — a git
			// worktree or a sibling clone an operator keeps on disk. Its files
			// belong to another tree (none are tracked here), and its older
			// copies would report as offenders of a rule they predate. Detect
			// them by that marker rather than by directory name: where someone
			// parks their checkouts is their business, not this test's.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		if _, ok := exempt[rel]; ok {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		if executorSpecMissesField(file, field) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return offenders
}

// Whether a run is sandboxed decides where its MCP servers may be started: in
// the container the workflow declared, or beside the launcher — an operator's
// machine, or the cloud runner pod that holds the platform's credentials. The
// executor predicts that from two tiers the launch surface passes, and the
// engine resolves it from the same two.
//
// An ExecutorSpec that passes neither is indistinguishable, field by field,
// from one whose surface looked and found no sandbox — and that ambiguity is
// not theoretical: when this guard was written FIVE of the nine construction
// sites passed no tier at all (both resumes, both subbot runners, the
// dispatcher), so a resumed run predicted "no sandbox" while its engine
// started one. SandboxTiersKnown is the explicit answer, and this guard is why
// the tenth site cannot quietly omit it.
//
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryExecutorConstructionAnswersTheSandboxQuestion(t *testing.T) {
	exempt := map[string]string{}

	offenders := executorSpecSitesMissing(t, "SandboxTiersKnown", exempt)
	if len(offenders) > 0 {
		t.Errorf("these build an executor without saying whether they know the run's sandbox tiers, so the MCP "+
			"start policy cannot tell \"this surface looked and there is no sandbox\" from \"this surface did not "+
			"look\": %v\nSet SandboxTiersKnown (true with the tiers the engine will receive; false to stay "+
			"fail-closed until the engine settles the sandbox), or add the file to this test's exempt map with a "+
			"reason.", offenders)
	}
}

// The prediction is the launch surface's best answer before the engine has
// started anything, and it is allowed to be permissive only when the surface
// actually knows the tiers. The asymmetry is the whole point: a wrong
// "sandboxed" costs a warning, a wrong "not sandboxed" starts a
// workflow-controlled process beside the launcher.
func TestThePredictedStartPolicyIsPermissiveOnlyWhenTheSurfaceKnows(t *testing.T) {
	sandboxed := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: "auto"}}
	plain := &ir.Workflow{}

	for _, tc := range []struct {
		name string
		spec ExecutorSpec
		want mcp.StartPolicy
	}{
		{"a surface that did not look stays closed", ExecutorSpec{Workflow: plain}, mcp.StartPolicyUnknown},
		{"a surface that did not look is closed even for a sandboxed workflow",
			ExecutorSpec{Workflow: sandboxed}, mcp.StartPolicyUnknown},
		{"knows, and the workflow sandboxes", ExecutorSpec{Workflow: sandboxed, SandboxTiersKnown: true},
			mcp.StartOperatorServersOnly},
		{"knows, and nothing sandboxes", ExecutorSpec{Workflow: plain, SandboxTiersKnown: true},
			mcp.StartAllServers},
		{"knows, and the operator's override neutralises the workflow's block",
			ExecutorSpec{Workflow: sandboxed, SandboxOverride: "none", SandboxTiersKnown: true},
			mcp.StartAllServers},
		{"knows, and the global default sandboxes a workflow that declares nothing",
			ExecutorSpec{Workflow: plain, SandboxDefault: "auto", SandboxTiersKnown: true},
			mcp.StartOperatorServersOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := predictedStartPolicy(tc.spec); got != tc.want {
				t.Errorf("predictedStartPolicy = %v, want %v", got, tc.want)
			}
		})
	}
}
