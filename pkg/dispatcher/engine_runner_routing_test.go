package dispatcher

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// recordingLauncher is a RunLauncher that records the LaunchSpec it was
// handed and launches nothing.
type recordingLauncher struct {
	specs []runview.LaunchSpec
}

func (l *recordingLauncher) LaunchAndWait(_ context.Context, spec runview.LaunchSpec) error {
	l.specs = append(l.specs, spec)
	return nil
}

// TestEngineRunnerPromotedMainBotKeepsTheLaunchAuthority: under
// ITERION_DISPATCH_VIA_SERVICE, a dispatcher configured with a bare
// `<bundle>/main.bot` still routes a fresh dispatch through the ADR-046
// launch authority — the promotion sets the bundle HANDLE on the runner,
// but the runner was configured with a FILE, and Service.Launch promotes
// that file itself. Gating on the handle would have moved every such
// config off the service in silence; this test is the routing itself:
// the stub launcher must be called, with the bundle's main.bot.
func TestEngineRunnerPromotedMainBotKeepsTheLaunchAuthority(t *testing.T) {
	tpl, ok := botscaffold.TemplateByID("multi-file")
	if !ok {
		t.Fatal("no multi-file template")
	}
	spec := tpl.Spec
	spec.Slug = "mf"
	spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
	dir := filepath.Join(t.TempDir(), spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	t.Setenv("ITERION_DISPATCH_VIA_SERVICE", "1")

	launcher := &recordingLauncher{}
	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	r, err := NewEngineRunner(filepath.Join(dir, "main.bot"), logger, WithRunLauncher(launcher))
	if err != nil {
		t.Fatalf("NewEngineRunner on the bare main.bot: %v", err)
	}
	defer r.Close()
	// A short deadline: were the dispatch to take the DIRECT path instead,
	// it would build a private engine and start the agent; the deadline
	// ends that promptly and the launcher stays uncalled — red either way.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = r.Dispatch(ctx, DispatchSpec{
		RunID:         "run-promoted-0001",
		WorkspacePath: t.TempDir(),
		StoreDir:      t.TempDir(),
		Issue:         &IssueRef{ID: "native:run-promoted-0001", Identifier: "run-promoted-0001", Title: "promoted"},
	})
	if len(launcher.specs) != 1 {
		t.Fatalf("the launch authority was called %d times for a promoted main.bot config, want 1", len(launcher.specs))
	}
	if got := launcher.specs[0].FilePath; !strings.HasSuffix(got, filepath.Join("mf", "main.bot")) {
		t.Fatalf("the launch authority was handed %q, want the bundle's main.bot", got)
	}
}
