package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestScheduleAdmissionFencesRunPersistence(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := launchRollbackPublisher(t, st)
	p.pluginSources = nil
	published := false
	p.publishRun = func(context.Context, *queue.RunMessage) error { published = true; return nil }
	lost := errors.New("schedule admission stolen")
	checked := false
	ctx := store.WithLaunchPersistenceGuard(context.Background(), func(context.Context, func(context.Context) error) error { checked = true; return lost })
	_, err = p.SubmitLaunch(ctx, "stale-schedule", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"})
	if !errors.Is(err, lost) || !checked || published {
		t.Fatalf("err=%v checked=%v published=%v", err, checked, published)
	}
	if r, err := st.LoadRun(context.Background(), "stale-schedule"); err == nil {
		t.Fatalf("stale admission persisted run %+v", r)
	}
}
