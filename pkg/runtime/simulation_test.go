package runtime

import (
	"bytes"
	"errors"
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
	for _, dir := range []string{"pkg/cli", "pkg/runview", "pkg/runner", "pkg/dispatcher", "pkg/server", "pkg/benchmark", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(src, []byte("WithSimulation(")) {
				t.Errorf("%s passes WithSimulation: a production launch would simulate", path)
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
}
