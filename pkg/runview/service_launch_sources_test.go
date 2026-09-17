package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sourceCapturingPublisher records what the cloud branch handed it.
type sourceCapturingPublisher struct {
	lastCompiled *CompiledSource
}

func (p *sourceCapturingPublisher) SubmitLaunch(_ context.Context, _ string, _ LaunchSpec, _ *ir.Workflow, cs *CompiledSource) (int, error) {
	p.lastCompiled = cs
	return 1, nil
}
func (p *sourceCapturingPublisher) CancelRun(context.Context, string) error { return nil }
func (p *sourceCapturingPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *sourceCapturingPublisher) SubmitResume(context.Context, ResumeSpec, *ir.Workflow, *CompiledSource) error {
	return nil
}

// The cloud branch hands the publisher the FILES its compile read, not just
// their identity hash.
//
// Nothing downstream can recover them: the queue message carries the compiled
// IR and the hash, and a runner pod has no filesystem the launch ever touched.
// So a launch that forwards the hash alone is a run that can never answer
// `rewind --auto` — which is the whole of #1226.
func TestLaunch_CloudPathForwardsTheCompiledUnitNotJustItsHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := "import \"lib/nodes.bot\"\n\nworkflow sourced:\n  entry: survey\n  survey -> done\n"
	fragment := "agent survey:\n  model: \"claude-opus-5\"\n"
	botPath := filepath.Join(dir, "main.bot")
	if err := os.WriteFile(botPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "nodes.bot"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}

	pub := &sourceCapturingPublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath}); err != nil {
		t.Fatalf("cloud-path Launch: %v", err)
	}

	cs := pub.lastCompiled
	if cs == nil {
		t.Fatal("the publisher was handed nothing to record: a cloud run cannot be rewound automatically")
	}
	if cs.Hash == "" {
		t.Error("the compile's identity hash was dropped")
	}
	if cs.Files[cs.Main] != main {
		t.Errorf("the main's text was not forwarded: Main=%q, Files has %d entries", cs.Main, len(cs.Files))
	}
	// The fragment matters as much as the main: an edit lives in either, and
	// a unit recorded main-only is one `rewind --auto` refuses outright
	// (ErrRewindUnitSourcesIncomplete).
	if len(cs.Files) != 2 {
		t.Fatalf("the unit was forwarded as %d file(s), want 2 — main plus its fragment", len(cs.Files))
	}
	var fragmentSeen bool
	for path, text := range cs.Files {
		if path != cs.Main && text == fragment {
			fragmentSeen = true
		}
	}
	if !fragmentSeen {
		t.Errorf("the fragment's text was not forwarded: files = %v", compiledFilePaths(cs.Files))
	}
}

func compiledFilePaths(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
