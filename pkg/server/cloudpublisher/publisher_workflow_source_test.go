package cloudpublisher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func sourceTestPublisher(t *testing.T) (*Publisher, *store.FilesystemRunStore, context.Context) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	return p, st, store.WithIdentity(context.Background(), "team-a", "u1")
}

// The queued run document is the ONLY place a cloud run's source can be
// recorded: the queue message carries the compiled IR and the identity hash,
// never the text, and the runner's engine writes the pair only when it has one
// — which on a pod it never does. Without this stamp `rewind --auto` answers
// ErrRewindNoSourceRecorded on every cloud run (#1226).
func TestSubmitLaunchRecordsWhatTheRunWillExecute(t *testing.T) {
	const main = "workflow wf:\n  entry: survey\n  survey -> done\n"

	t.Run("a single-file bot records its main alone", func(t *testing.T) {
		p, st, ctx := sourceTestPublisher(t)
		_, err := p.SubmitLaunch(ctx, "run-single", runview.LaunchSpec{FilePath: "wf.bot", Source: main},
			&ir.Workflow{Name: "wf"},
			&runview.CompiledSource{Hash: "h1", Main: "wf.bot", Files: map[string]string{"wf.bot": main}})
		if err != nil {
			t.Fatalf("SubmitLaunch: %v", err)
		}
		r, err := st.LoadRun(ctx, "run-single")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if r.WorkflowSource != main {
			t.Errorf("workflow_source = %q, want the text the launch compiled", r.WorkflowSource)
		}
		// A bot of one file records no file list, exactly as a local launch
		// does — the reader falls back to parsing WorkflowSource alone.
		if len(r.WorkflowSources) != 0 {
			t.Errorf("workflow_sources = %v, want none for a single-file bot", r.WorkflowSources)
		}
	})

	t.Run("a bot in several files records every file, its main FIRST", func(t *testing.T) {
		const (
			unitMain = "import \"lib/nodes.bot\"\n\nworkflow wf:\n  entry: survey\n  survey -> done\n"
			fragment = "agent survey:\n  model: \"claude-opus-5\"\n"
		)
		// On disk, not inline: the publisher refuses an inline source that
		// imports, because its fragments did not travel with it — so a unit
		// launch is a FilePath launch, and the fixture has to be one too.
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		botPath := filepath.Join(dir, "main.bot")
		if err := os.WriteFile(botPath, []byte(unitMain), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lib", "nodes.bot"), []byte(fragment), 0o644); err != nil {
			t.Fatal(err)
		}
		p, st, ctx := sourceTestPublisher(t)
		_, err := p.SubmitLaunch(ctx, "run-unit", runview.LaunchSpec{FilePath: botPath},
			&ir.Workflow{Name: "wf"},
			&runview.CompiledSource{Hash: "h2", Main: "main.bot", Files: map[string]string{
				"main.bot":      unitMain,
				"lib/nodes.bot": fragment,
			}})
		if err != nil {
			t.Fatalf("SubmitLaunch: %v", err)
		}
		r, err := st.LoadRun(ctx, "run-unit")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if len(r.WorkflowSources) != 2 {
			t.Fatalf("workflow_sources has %d file(s), want the whole unit", len(r.WorkflowSources))
		}
		// Position 0 is not cosmetic: resolveAutoPivotForRun loads the
		// recorded unit with `unit.LoadMap(files, run.WorkflowSources[0].Path)`,
		// so a list that leads with a fragment makes the reader treat that
		// fragment as the main and diff the wrong file.
		if got := r.WorkflowSources[0].Path; got != "main.bot" {
			t.Errorf("workflow_sources[0] = %q, want the main — the reader takes index 0 as the unit's main", got)
		}
		if r.WorkflowSources[0].Text != unitMain || r.WorkflowSources[1].Text != fragment {
			t.Error("the recorded texts are not the ones the launch compiled")
		}
		if r.WorkflowSource != unitMain {
			t.Errorf("workflow_source = %q, want the main's text", r.WorkflowSource)
		}
	})

	t.Run("a unit past the cap records nothing, and still launches", func(t *testing.T) {
		p, st, ctx := sourceTestPublisher(t)
		huge := main + strings.Repeat("# padding\n", 200_000) // > 1 MiB
		_, err := p.SubmitLaunch(ctx, "run-huge", runview.LaunchSpec{FilePath: "wf.bot", Source: huge},
			&ir.Workflow{Name: "wf"},
			&runview.CompiledSource{Hash: "h3", Main: "wf.bot", Files: map[string]string{"wf.bot": huge}})
		if err != nil {
			t.Fatalf("a launch past the source cap must still be queued: %v", err)
		}
		r, err := st.LoadRun(ctx, "run-huge")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if r.WorkflowSource != "" || len(r.WorkflowSources) != 0 {
			t.Errorf("a run past the cap recorded %d bytes and %d file(s); past it nothing is recorded, and only auto-rewind is lost",
				len(r.WorkflowSource), len(r.WorkflowSources))
		}
	})

	t.Run("a launch with no compile result still queues", func(t *testing.T) {
		p, st, ctx := sourceTestPublisher(t)
		if _, err := p.SubmitLaunch(ctx, "run-nil", runview.LaunchSpec{FilePath: "wf.bot", Source: main},
			&ir.Workflow{Name: "wf"}, nil); err != nil {
			t.Fatalf("SubmitLaunch with no compiled source: %v", err)
		}
		r, err := st.LoadRun(ctx, "run-nil")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		if r.WorkflowSource != "" {
			t.Errorf("workflow_source = %q, want empty", r.WorkflowSource)
		}
	})
}
