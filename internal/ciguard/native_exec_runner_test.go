package ciguard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The native override must be behind the same trust guards as ARC, not a
// shortcut that lets fork or dependency-bot content choose a private runner.
func TestNativeExecRunnersPreserveTrustBoundaryAndProveMounts(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	type shellDefaults struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	}
	var wf struct {
		Defaults shellDefaults `yaml:"defaults"`
		Jobs     map[string]struct {
			RunsOn   string        `yaml:"runs-on"`
			Defaults shellDefaults `yaml:"defaults"`
			Steps    []struct {
				Name     string `yaml:"name"`
				Shell    string `yaml:"shell"`
				Run      string `yaml:"run"`
				If       string `yaml:"if"`
				Continue bool   `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Defaults.Run.Shell != "" {
		t.Fatal("workflow default shell changes native preflight execution contract")
	}
	canonical := wf.Jobs["golangci"].RunsOn
	suffix := "|| 'arc-runners' }}"
	if !strings.HasSuffix(canonical, suffix) {
		t.Fatal("ARC routing shape changed; re-evaluate native trust boundary")
	}
	expected := strings.TrimSuffix(canonical, suffix) + "|| vars.CI_NATIVE_EXEC_RUNNER || 'ubuntu-latest' }}"
	for _, id := range []string{"test-unit", "race-shard"} {
		job, ok := wf.Jobs[id]
		if !ok {
			t.Fatal("missing native test job", id)
		}
		if job.Defaults.Run.Shell != "" {
			t.Errorf("%s default shell changes native preflight execution contract", id)
		}
		if job.RunsOn != expected {
			t.Errorf("%s can lose its native mount capability or bypass the shared fork/bot/off guards", id)
		}
		found := 0
		prerequisiteIndex, testIndex := -1, -1
		for index, step := range job.Steps {
			if (id == "test-unit" && step.Name == "Unit tests") || (id == "race-shard" && step.Name == "Race tests (shard ${{ matrix.shard }} of 3)") {
				testIndex = index
			}
			if step.Name != "Native exec environment prerequisites" {
				continue
			}
			found++
			prerequisiteIndex = index
			if step.If != "" || step.Continue || step.Shell != "" {
				t.Errorf("%s can skip or forgive the mount prerequisite", id)
			}
			if strings.TrimSpace(step.Run) != "sudo apt-get update\nsudo apt-get install -y bubblewrap build-essential\nbwrap --version\nbwrap --die-with-parent --ro-bind / / --dev /dev --proc /proc /bin/true" {
				t.Errorf("%s must execute the reviewed native prerequisite block unchanged", id)
			}
		}
		if found != 1 || testIndex < 0 || prerequisiteIndex >= testIndex {
			t.Errorf("%s must have exactly one native mount preflight before its test step", id)
		}
	}
}
