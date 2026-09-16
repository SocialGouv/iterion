package runtime

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Every arm of a simulation is off on an engine nobody asked to simulate,
// and on when asked.
func TestAProductionEngineSimulatesNothing(t *testing.T) {
	if New(&ir.Workflow{}, nil, nil).Simulating() {
		t.Fatal("an engine built without WithSimulation simulates")
	}
	for name, s := range map[string]Simulation{
		"humans":  {AnswerHumans: true},
		"events":  {EventsArrive: true},
		"answers": {AnswersArrive: true},
	} {
		if !New(&ir.Workflow{}, nil, nil, WithSimulation(s)).Simulating() {
			t.Fatalf("the %s arm does not read as simulating", name)
		}
	}
	if New(&ir.Workflow{}, nil, nil, WithSimulation(Simulation{})).Simulating() {
		t.Fatal("the zero simulation reads as simulating")
	}
}

// No production launch passes WithSimulation: the option is pkg/dryrun's
// alone. The sweep reads the source of every package that builds an
// engine — a launch path that gained the option would simulate a real run.
func TestNoProductionPackagePassesWithSimulation(t *testing.T) {
	root := filepath.Join("..", "..")
	skipDirs := map[string]bool{"vendor": true, "node_modules": true, ".git": true, ".works": true, ".repos": true, ".claude": true, "web": true}
	var launchers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if skipDirs[d.Name()] || rel == filepath.Join("pkg", "runtime") || rel == filepath.Join("pkg", "dryrun") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(src, []byte("runtime.New(")) {
			launchers = append(launchers, rel)
		}
		if bytes.Contains(src, []byte("WithSimulation(")) {
			t.Errorf("%s passes WithSimulation: a production launch would simulate", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The sweep must have read the launchers, or it proves nothing: the
	// CLI, the runner, the dispatcher and the run view all build an engine.
	if len(launchers) < 4 {
		t.Fatalf("the sweep read %d engine launchers (%v): it did not cover the tree", len(launchers), launchers)
	}
}
