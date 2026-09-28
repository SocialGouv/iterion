package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Under `go test`, a skill mirror whose destination is the test process's
// cwd — the package directory — would write into the developer's checkout
// (pkg/runner/.claude, pkg/runview/.claude, seen 2026-09-24): the engine
// under test has no explicit workDir. The guard fails that run loudly,
// naming the remedy, instead of letting the mirror write (#1803).
func init() {
	SetMirrorCwdGuardForTests(func(workDir string) error {
		wd, err := filepath.Abs(workDir)
		if err != nil {
			return nil
		}
		cwd, err := os.Getwd()
		if err != nil {
			return nil
		}
		if filepath.Clean(wd) == filepath.Clean(cwd) {
			return fmt.Errorf("the skill mirror's destination is the TEST process cwd (%s): the engine under test has no explicit workDir — set runtime.WithWorkDir(t.TempDir()) so the mirror writes into the test's own directory (#1803)", wd)
		}
		return nil
	})
}

// The armed guard refuses a mirror whose destination is the package
// directory — this test is its live witness: without the guard check in
// the mirrors, the refusal disappears and this goes red (#1803).
func TestTheMirrorCwdGuardRefusesThePackageDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = mirrorPluginContributions(cwd, &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "s.md", Content: []byte("x")}}}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "TEST process cwd") {
		t.Fatalf("the armed guard did not refuse a package-dir mirror: %v", err)
	}
	_, _, _, err = mirrorLibrarySkills(cwd, t.TempDir(), &ir.Workflow{
		Name:    "w",
		Entry:   "done",
		Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges:   []*ir.Edge{},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "TEST process cwd") {
		t.Fatalf("the armed guard did not refuse a package-dir library mirror: %v", err)
	}
}
