package cloudpublisher

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// A launch that fails after its run row is persisted must not leave that row
// `queued`: nothing backs it — no queue message was ever published — so no
// runner will ever pick it up, and every list shows a run that is waiting for
// nothing. 93 such rows accumulated in ~2h during the 2026-08-26 incident,
// one per failed webhook launch (#537). The contract, on both store twins:
// after a failed SubmitLaunch the row is either absent or terminal, and a
// terminal row names the launch failure with a typed code.

// unlistablePluginSources is a plugin-source store whose listing fails — the
// shape of a contribution failure inside the launch path (a store outage, or
// the resolver refusing). The embedded nil interface makes any other call
// panic, which is the point: only ListEnabledByTenant may be reached.
type unlistablePluginSources struct{ pluginsource.Store }

func (unlistablePluginSources) ListEnabledByTenant(context.Context, string) ([]pluginsource.PluginSource, error) {
	return nil, errors.New("plugin_sources collection unavailable")
}

func launchRollbackPublisher(t *testing.T, st store.RunStore) *Publisher {
	t.Helper()
	return &Publisher{
		store:      st,
		logger:     iterlog.New(iterlog.LevelError, io.Discard),
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
		pluginSources: &pluginsource.Resolver{
			Store:   unlistablePluginSources{},
			Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()},
		},
	}
}

func assertNoOrphanQueuedRow(t *testing.T, ctx context.Context, st store.RunStore, runID string) *store.Run {
	t.Helper()
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		return nil // no row at all is the cleanest outcome
	}
	if r.Status == store.RunStatusQueued {
		t.Fatalf("run %s is `queued` after a failed launch — no queue message backs it, nothing will ever pick it up", runID)
	}
	return r
}

func runLaunchRollbackSuite(t *testing.T, st store.RunStore) {
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}

	t.Run("a contribution failure leaves no queued row", func(t *testing.T) {
		p := launchRollbackPublisher(t, st)
		if _, err := p.SubmitLaunch(ctx, "run-contrib-fail", spec, wf, "hash"); err == nil {
			t.Fatal("SubmitLaunch succeeded with an unlistable plugin-source store")
		}
		if r := assertNoOrphanQueuedRow(t, ctx, st, "run-contrib-fail"); r != nil {
			if r.FailureCode != store.FailureLaunchFailed || !strings.Contains(r.Error, "plugin_sources collection unavailable") {
				t.Fatalf("a persisted row must name the launch failure: status=%s code=%q err=%q", r.Status, r.FailureCode, r.Error)
			}
		}
	})

	t.Run("a publish failure marks the row failed with a typed code", func(t *testing.T) {
		p := launchRollbackPublisher(t, st)
		p.pluginSources = nil
		p.publishRun = func(context.Context, *queue.RunMessage) error { return errors.New("nats: no responders") }
		if _, err := p.SubmitLaunch(ctx, "run-publish-fail", spec, wf, "hash"); err == nil {
			t.Fatal("SubmitLaunch succeeded with a failing publisher")
		}
		r := assertNoOrphanQueuedRow(t, ctx, st, "run-publish-fail")
		if r == nil {
			t.Fatal("the row was persisted before publish; it must survive as a terminal, explained failure rather than vanish")
		}
		if r.Status != store.RunStatusFailed {
			t.Fatalf("status = %s, want failed", r.Status)
		}
		if r.FailureCode != store.FailureLaunchFailed {
			t.Fatalf("failure_code = %q, want %s — the row must say it never left the launch path", r.FailureCode, store.FailureLaunchFailed)
		}
		if !strings.Contains(r.Error, "nats: no responders") {
			t.Fatalf("run.Error = %q, want the publish failure named", r.Error)
		}
	})

	t.Run("a successful launch stays queued", func(t *testing.T) {
		p := launchRollbackPublisher(t, st)
		p.pluginSources = nil
		if _, err := p.SubmitLaunch(ctx, "run-ok", spec, wf, "hash"); err != nil {
			t.Fatalf("SubmitLaunch: %v", err)
		}
		r, err := st.LoadRun(ctx, "run-ok")
		if err != nil || r.Status != store.RunStatusQueued || r.FailureCode != "" {
			t.Fatalf("a launched run must read queued with no failure code: %+v (%v)", r, err)
		}
	})
}

func TestSubmitLaunch_FailureLeavesNoOrphanQueuedRow_File(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runLaunchRollbackSuite(t, st)
}

// The Mongo twin: same contract on the cloud store the incident happened on.
// Gated like every other Mongo suite (ITERION_TEST_MONGO_URI).
func TestSubmitLaunch_FailureLeavesNoOrphanQueuedRow_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set — skipping Mongo launch-rollback suite")
	}
	dbName := "iterion_launch_rollback_" + time.Now().UTC().Format("20060102150405.000000000")
	dbName = strings.ReplaceAll(dbName, ".", "_")
	st, err := mongostore.New(context.Background(), mongostore.Config{URI: uri, Database: dbName, Blob: nopBlobClient{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = st.DB().Drop(ctx)
		_ = st.Close(ctx)
	})
	runLaunchRollbackSuite(t, st)
}

// nopBlobClient satisfies the Mongo store's blob dependency. A launch that
// fails writes no artifact, so only the lifecycle calls are answered; any
// other method reaching the embedded nil interface panics, which is the
// point — nothing in this suite may touch blob storage.
type nopBlobClient struct{ blob.Client }

func (nopBlobClient) Ping(context.Context) error { return nil }
func (nopBlobClient) Close() error               { return nil }
