package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/jsonl"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gateFixture wires a manifest + store + runRunFn stub for RunScheduleRun
// gate tests. The stub records the RunOptions it received instead of
// executing a real engine run.
type gateFixture struct {
	manifestPath string
	storeDir     string
	store        store.RunStore
	captured     []RunOptions
}

func newGateFixture(t *testing.T, entry ScheduleEntry) *gateFixture {
	t.Helper()
	dir := t.TempDir()
	f := &gateFixture{
		manifestPath: filepath.Join(dir, "schedules.yaml"),
		storeDir:     filepath.Join(dir, "store"),
	}
	entry.StoreDir = f.storeDir
	if entry.Workdir == "" {
		entry.Workdir = dir
	}
	m := &ScheduleManifest{Version: 1, Schedules: []ScheduleEntry{entry}}
	if err := saveScheduleManifest(f.manifestPath, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	s, err := store.New(f.storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	f.store = s

	prev := runRunFn
	runRunFn = func(_ context.Context, opts RunOptions, _ *Printer) error {
		f.captured = append(f.captured, opts)
		return nil
	}
	t.Cleanup(func() { runRunFn = prev })
	return f
}

// seedScheduledRun creates a run stamped with the schedule provenance
// in the fixture store, with the given status.
func (f *gateFixture) seedScheduledRun(t *testing.T, id, scheduleID string, status store.RunStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.CreateRun(ctx, id, "demo", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := f.store.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	r.Source = &store.RunSource{Kind: store.RunSourceKindSchedule, ScheduleID: scheduleID}
	if err := f.store.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
}

func (f *gateFixture) auditRecords(t *testing.T) []schedgate.TickRecord {
	t.Helper()
	recs, err := jsonl.ReadLines[schedgate.TickRecord](tickAuditPath(f.manifestPath))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return recs
}

func (f *gateFixture) run(t *testing.T, name string) (string, error) {
	t.Helper()
	p, buf := testPrinter()
	err := RunScheduleRun(context.Background(), p, ScheduleRunOptions{
		ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: f.manifestPath},
		Name:                  name,
	})
	return buf.String(), err
}

func TestScheduleRun_OverlapSkipsAndAuditsBlockingRun(t *testing.T) {
	f := newGateFixture(t, ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: "demo.bot"})
	f.seedScheduledRun(t, "r_live", "weekly", store.RunStatusRunning)

	out, err := f.run(t, "weekly")
	if err != nil {
		t.Fatalf("skip must exit 0 (no cron mailspam), got %v", err)
	}
	if len(f.captured) != 0 {
		t.Fatalf("RunRun must not be called on overlap skip, got %d calls", len(f.captured))
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "r_live") {
		t.Fatalf("skip output must name the blocking run: %q", out)
	}

	recs := f.auditRecords(t)
	if len(recs) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Decision != schedgate.TickSkippedOverlap || rec.BlockingRunID != "r_live" ||
		rec.Surface != schedgate.SurfaceHostCron || rec.ScheduleID != "weekly" {
		t.Fatalf("audit record mismatch: %+v", rec)
	}
}

func TestScheduleRun_TerminalRunsDoNotBlock(t *testing.T) {
	f := newGateFixture(t, ScheduleEntry{Name: "weekly", Cron: "0 2 * * 1", Bot: "demo.bot"})
	f.seedScheduledRun(t, "r_done", "weekly", store.RunStatusFinished)
	f.seedScheduledRun(t, "r_other", "another-schedule", store.RunStatusRunning)

	if _, err := f.run(t, "weekly"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.captured) != 1 {
		t.Fatalf("terminal + other-schedule runs must not block: %d calls", len(f.captured))
	}
}

func TestScheduleRun_AllowMaxConcurrent(t *testing.T) {
	entry := ScheduleEntry{Name: "par", Cron: "* * * * *", Bot: "demo.bot", Overlap: "allow", MaxConcurrent: 3}
	f := newGateFixture(t, entry)
	f.seedScheduledRun(t, "r1", "par", store.RunStatusRunning)
	f.seedScheduledRun(t, "r2", "par", store.RunStatusRunning)

	if _, err := f.run(t, "par"); err != nil {
		t.Fatalf("run under cap: %v", err)
	}
	if len(f.captured) != 1 {
		t.Fatalf("2 live < max 3 must fire, got %d calls", len(f.captured))
	}

	f.seedScheduledRun(t, "r3", "par", store.RunStatusRunning)
	if _, err := f.run(t, "par"); err != nil {
		t.Fatalf("run at cap: %v", err)
	}
	if len(f.captured) != 1 {
		t.Fatalf("3 live >= max 3 must skip, got %d calls", len(f.captured))
	}
	recs := f.auditRecords(t)
	last := recs[len(recs)-1]
	if last.Decision != schedgate.TickSkippedOverlap {
		t.Fatalf("last audit = %s, want skipped_overlap", last.Decision)
	}
}

func TestScheduleRun_GuardPassInjectsStdoutIntoVar(t *testing.T) {
	entry := ScheduleEntry{Name: "guarded", Cron: "* * * * *", Bot: "demo.bot",
		Guard: "echo v42", GuardVar: "greeting"}
	f := newGateFixture(t, entry)

	if _, err := f.run(t, "guarded"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.captured) != 1 {
		t.Fatalf("guard pass must fire, got %d calls", len(f.captured))
	}
	got := f.captured[0]
	if got.Vars["greeting"] != "v42\n" {
		t.Fatalf("Vars[greeting] = %q, want %q", got.Vars["greeting"], "v42\n")
	}

	recs := f.auditRecords(t)
	if len(recs) != 1 || recs[0].Decision != schedgate.TickFired || recs[0].RunID == "" {
		t.Fatalf("fired audit mismatch: %+v", recs)
	}
	if recs[0].RunID != got.RunID {
		t.Fatalf("audit RunID %q != launched RunID %q", recs[0].RunID, got.RunID)
	}
}

func TestScheduleRun_GuardNonZeroBlocks(t *testing.T) {
	entry := ScheduleEntry{Name: "noop", Cron: "* * * * *", Bot: "demo.bot", Guard: "exit 3"}
	f := newGateFixture(t, entry)

	out, err := f.run(t, "noop")
	if err != nil {
		t.Fatalf("guard block must exit 0, got %v", err)
	}
	if len(f.captured) != 0 {
		t.Fatalf("RunRun must not be called on guard block")
	}
	if !strings.Contains(out, "guard exited 3") {
		t.Fatalf("output: %q", out)
	}
	recs := f.auditRecords(t)
	if len(recs) != 1 || recs[0].Decision != schedgate.TickGuardBlocked {
		t.Fatalf("audit: %+v", recs)
	}
	if recs[0].GuardExit == nil || *recs[0].GuardExit != 3 {
		t.Fatalf("GuardExit = %v, want 3", recs[0].GuardExit)
	}
}

func TestScheduleRun_GuardTimeoutIsGuardError(t *testing.T) {
	entry := ScheduleEntry{Name: "slow", Cron: "* * * * *", Bot: "demo.bot",
		Guard: "sleep 5", GuardTimeout: "50ms"}
	f := newGateFixture(t, entry)

	if _, err := f.run(t, "slow"); err != nil {
		t.Fatalf("guard error must exit 0: %v", err)
	}
	if len(f.captured) != 0 {
		t.Fatalf("RunRun must not be called on guard error")
	}
	recs := f.auditRecords(t)
	if len(recs) != 1 || recs[0].Decision != schedgate.TickGuardError {
		t.Fatalf("audit: %+v", recs)
	}
	if recs[0].Error == "" {
		t.Fatalf("guard_error record must carry the error text")
	}
}

func TestScheduleRun_StampsScheduleProvenance(t *testing.T) {
	f := newGateFixture(t, ScheduleEntry{Name: "prov", Cron: "* * * * *", Bot: "demo.bot"})

	if _, err := f.run(t, "prov"); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := f.captured[0]
	if got.RunID == "" {
		t.Fatalf("RunID must be pre-minted for audit correlation")
	}
	if got.Source == nil || got.Source.Kind != store.RunSourceKindSchedule || got.Source.ScheduleID != "prov" {
		t.Fatalf("Source = %+v, want schedule provenance", got.Source)
	}
}

func TestScheduleAudit_FiltersByName(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "schedules.yaml")
	path := tickAuditPath(manifest)
	mk := func(id string, d schedgate.TickDecision) schedgate.TickRecord {
		return schedgate.NewTickRecord(schedgate.SurfaceHostCron, id, time.Now(), d)
	}
	for _, r := range []schedgate.TickRecord{
		mk("weekly", schedgate.TickFired),
		mk("nightly", schedgate.TickGuardBlocked),
		mk("weekly", schedgate.TickSkippedOverlap),
	} {
		if err := jsonl.AppendJSON(path, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	p, buf := testPrinter()
	err := RunScheduleAudit(p, ScheduleAuditOptions{
		ScheduleCommonOptions: ScheduleCommonOptions{ManifestPath: manifest},
		Name:                  "weekly",
	})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "fired") || !strings.Contains(out, "skipped_overlap") {
		t.Fatalf("weekly rows missing: %q", out)
	}
	if strings.Contains(out, "guard_blocked") {
		t.Fatalf("nightly row leaked through the --name filter: %q", out)
	}
}
