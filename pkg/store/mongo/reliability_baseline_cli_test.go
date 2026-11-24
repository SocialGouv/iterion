package mongo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// This suite exercises pkg/cli's reliability baseline over the CLOUD run
// store (#1006 slice 1: the pilot runs in the cloud, so step 1 of
// docs/workflow-reliability-1006.md has to be takeable there).
//
// It lives in pkg/store/mongo rather than next to the code it drives
// because the mongo-conformance job runs an explicit package list that
// pkg/cli is not on, and that job sits at its fifteen-minute budget with
// no Go cache. A gated suite in pkg/cli would skip locally AND in CI —
// the silent green TestMongoGatedPackagesAreInTheCIJob exists to refuse —
// while adding the package would spend another minute of a budget two
// sibling jobs have already overrun. An external test package
// (mongo_test) is what lets this directory import pkg/cli at all.

// nopBlobClient satisfies the cloud store's blob dependency, which
// mongostore.New requires even though a baseline reads no artifact body.
// The embedded nil interface is the assertion: any blob call the baseline
// made would panic here instead of passing unnoticed.
type nopBlobClient struct{ blob.Client }

func (nopBlobClient) Ping(context.Context) error { return nil }
func (nopBlobClient) Close() error               { return nil }

func reliabilityBaselineStore(t *testing.T) (*mongostore.Store, string) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the cloud reliability baseline suite")
	}
	dbName := "iterion_reliability_baseline_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: nopBlobClient{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = ms.DB().Drop(drop)
		_ = ms.Close(drop)
	})
	return ms, dbName
}

func seedCloudRun(t *testing.T, ms *mongostore.Store, tenant, id string, status store.RunStatus) {
	t.Helper()
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	ctx = store.WithIdentity(ctx, tenant, "owner-"+tenant)
	if _, err := ms.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	if err := ms.UpdateRunStatus(ctx, id, status, ""); err != nil {
		t.Fatalf("UpdateRunStatus %s: %v", id, err)
	}
}

func cloudBaseline(t *testing.T, src cli.ReliabilityRunSource, runID string) cli.ReliabilityReport {
	t.Helper()
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	var buf bytes.Buffer
	if err := cli.RunReliabilityReport(ctx, cli.ReliabilityOptions{Source: src, RunID: runID}, &cli.Printer{W: &buf, Format: cli.OutputJSON}); err != nil {
		t.Fatalf("RunReliabilityReport: %v", err)
	}
	var got cli.ReliabilityReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return got
}

// TestReliabilityReport_BaselineOverAMongoStore is the witness for the
// cloud baseline: the counters come from the Mongo run store's own
// documents, and the source line names the database they came from.
//
// Without it, `iterion reliability report` could only ever measure the
// filesystem store next to a working directory — a developer's local
// runs — and the before/after comparison a promotion is decided on would
// be taken over a population the pilot never touched.
func TestReliabilityReport_BaselineOverAMongoStore(t *testing.T) {
	t.Setenv(reliability.EnvMode, "")
	t.Setenv(reliability.EnvContextPolicyAlias, "")
	ms, dbName := reliabilityBaselineStore(t)
	seedCloudRun(t, ms, "tenant-a", "cloud-done", store.RunStatusFinished)
	seedCloudRun(t, ms, "tenant-a", "cloud-paused", store.RunStatusPausedWaitingHuman)
	seedCloudRun(t, ms, "tenant-b", "cloud-other-tenant", store.RunStatusFailed)

	got := cloudBaseline(t, cli.NewMongoReliabilitySource(ms, dbName, ""), "")
	if got.Baseline == nil {
		t.Fatal("no baseline over the cloud store")
	}
	if got.Baseline.Total != 3 || got.Baseline.Finished != 1 || got.Baseline.Paused != 1 || got.Baseline.Failed != 1 {
		t.Fatalf("cluster-wide baseline = %+v, want the three runs in the store", *got.Baseline)
	}
	if !strings.Contains(got.Source, "mongo:"+dbName) {
		t.Fatalf("source = %q, want the mongo database it read", got.Source)
	}
	if got.StoreDir != "" {
		t.Fatalf("store_dir = %q, want empty: the cloud store is not a directory", got.StoreDir)
	}
}

// TestReliabilityReport_MongoBaselineIsTenantScoped pins the scope on BOTH
// halves of the walk. A listing scoped to one tenant whose loads were
// unscoped would pair one tenant's ids with another tenant's documents,
// and a per-tenant pilot would be decided on the wrong runs.
func TestReliabilityReport_MongoBaselineIsTenantScoped(t *testing.T) {
	t.Setenv(reliability.EnvMode, "")
	t.Setenv(reliability.EnvContextPolicyAlias, "")
	ms, dbName := reliabilityBaselineStore(t)
	seedCloudRun(t, ms, "tenant-a", "cloud-done", store.RunStatusFinished)
	seedCloudRun(t, ms, "tenant-a", "cloud-paused", store.RunStatusPausedWaitingHuman)
	seedCloudRun(t, ms, "tenant-b", "cloud-other-tenant", store.RunStatusFailed)

	scoped := cli.NewMongoReliabilitySource(ms, dbName, "tenant-a")
	got := cloudBaseline(t, scoped, "")
	if got.Baseline == nil || got.Baseline.Total != 2 || got.Baseline.Failed != 0 {
		t.Fatalf("tenant-scoped baseline = %+v, want tenant-a's two runs only", got.Baseline)
	}
	if !strings.Contains(got.Source, "tenant=tenant-a") {
		t.Fatalf("source = %q, want the tenant scope named", got.Source)
	}
	if len(got.Unreadable) != 0 {
		t.Fatalf("unreadable = %v: another tenant's run must be out of scope, not an unreadable candidate", got.Unreadable)
	}

	// The per-run question carries the same scope: tenant-a cannot read
	// tenant-b's run document through --run-id.
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	err := cli.RunReliabilityReport(
		ctx,
		cli.ReliabilityOptions{Source: scoped, RunID: "cloud-other-tenant"},
		&cli.Printer{W: &bytes.Buffer{}, Format: cli.OutputJSON},
	)
	if err == nil {
		t.Fatal("a tenant-scoped report answered about another tenant's run")
	}
}
