package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// presetBundle writes a one-preset bundle and opens it.
func presetBundle(t *testing.T, file, content string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(dir, "presets")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.PresetsDir == "" {
		t.Fatal("PresetsDir not set by OpenDir")
	}
	return b
}

// presetSecretWorkflow declares a value secret `tok` with the given grants
// beside an agent — the model task the selected preset's body is appended
// to.
func presetSecretWorkflow(sinks []string) *ir.Workflow {
	return &ir.Workflow{
		Secrets: map[string]*ir.Secret{"tok": {Name: "tok", As: "value", Sinks: sinks}},
		Nodes:   map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}},
	}
}

// The merge is the one point every path that folds a bundle's presets crosses,
// so it is where a prompt body whose `{{secrets.X}}` reaches a sink its secret
// withholds is said — naming the preset FILE, since a frontmatter `name:`
// detaches the id from the page. The merge still folds the preset in: the
// reference renders as written and the operator's choice stands.
func TestMergingABundlesPresetsSaysWhichPresetReachesAnUngrantedSink(t *testing.T) {
	b := presetBundle(t, "sre.md", "---\nname: sre\n---\nDeploy with {{secrets.tok}} when asked.")

	var out bytes.Buffer
	wf := presetSecretWorkflow([]string{secretsink.Command})
	MergeBundlePresets(wf, b, "sre", iterlog.New(iterlog.LevelWarn, &out))
	said := out.String()
	for _, want := range []string{
		"presets" + string(filepath.Separator) + "sre.md",
		string(ir.DiagSecretSinkUngranted),
		"{{secrets.tok}}",
		"reaches the prompt sink",
		"add `prompt`",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the merge did not say %q:\n%s", want, said)
		}
	}
	if p, ok := wf.Presets["sre"]; !ok || p.Prompt == "" {
		t.Errorf("the merge dropped the preset it reported on: %+v", wf.Presets)
	}

	// The grant given, the merge says nothing — and a secret with no `sinks:`
	// grants every sink, so it says nothing there either.
	for _, sinks := range [][]string{{secretsink.Prompt, secretsink.Agent}, nil} {
		var quiet bytes.Buffer
		MergeBundlePresets(presetSecretWorkflow(sinks), b, "sre", iterlog.New(iterlog.LevelWarn, &quiet))
		if strings.Contains(quiet.String(), string(ir.DiagSecretSinkUngranted)) {
			t.Errorf("the merge reports C323 with the prompt sink granted (sinks: %v):\n%s", sinks, quiet.String())
		}
	}
}

// presetLines is what the merge said about preset bodies: one line per
// finding, the loader's own lines left out.
func presetLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "runtime: bundle preset") && strings.Contains(l, "[C") {
			lines = append(lines, l)
		}
	}
	return lines
}

// The launch speaks about what it renders: the selected preset's body, once.
// A bundle's other presets are folded in and rendered by nothing, so a stale
// reference in one of them is `iterion validate`'s error, not a line on
// every launch; and the engine's backstop merge, finding the preset already
// folded with the same body by the command's own pass, says nothing again.
func TestTheMergeSaysASelectedPresetsFindingOnce(t *testing.T) {
	b := presetBundle(t, "sre.md", "---\nname: sre\n---\nDeploy with {{secrets.tok}} when asked.")
	if err := os.WriteFile(filepath.Join(b.PresetsDir, "ops.md"), []byte("---\nname: ops\n---\nPage with {{secrets.tok}} too."), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	wf := presetSecretWorkflow([]string{secretsink.Command})
	logger := iterlog.New(iterlog.LevelWarn, &out)
	MergeBundlePresets(wf, b, "sre", logger)
	said := presetLines(out.String())
	if len(said) != 1 || !strings.Contains(said[0], "sre.md") {
		t.Fatalf("the launch of preset sre said %d line(s), want one naming sre.md alone:\n%s", len(said), out.String())
	}
	for _, name := range []string{"sre", "ops"} {
		if p, ok := wf.Presets[name]; !ok || p.Prompt == "" {
			t.Errorf("preset %q was not folded in: %+v", name, wf.Presets)
		}
	}

	// The backstop pass over the same workflow: already folded, already said.
	MergeBundlePresets(wf, b, "sre", logger)
	if again := presetLines(out.String()); len(again) != 1 {
		t.Errorf("a re-merge into the same workflow said the finding again (%d lines):\n%s", len(again), out.String())
	}

	// No preset selected: no body is rendered, nothing to say.
	var none bytes.Buffer
	MergeBundlePresets(presetSecretWorkflow([]string{secretsink.Command}), b, "", iterlog.New(iterlog.LevelWarn, &none))
	if said := presetLines(none.String()); len(said) != 0 {
		t.Errorf("a launch that selects no preset said %d line(s) about preset bodies:\n%s", len(said), none.String())
	}
}
