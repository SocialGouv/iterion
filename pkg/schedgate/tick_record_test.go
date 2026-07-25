package schedgate

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewTickRecordAndJSONRoundtrip(t *testing.T) {
	at := time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	rec := NewTickRecord(SurfaceHostCron, "weekly", at, TickSkippedOverlap)
	rec.BlockingRunID = "r1"
	rec.Reason = "blocked by live run r1"

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back TickRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Schema != TickSchemaVersion {
		t.Fatalf("Schema = %d, want %d", back.Schema, TickSchemaVersion)
	}
	if back.Decision != TickSkippedOverlap || back.BlockingRunID != "r1" || !back.At.Equal(at) {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
}

func TestApplyGuard(t *testing.T) {
	rec := NewTickRecord(SurfaceTrigger, "sub1", time.Now(), TickGuardBlocked)
	rec.ApplyGuard(GuardResult{Kind: GuardBlocked, ExitCode: 3, Stdout: "out", StderrTail: "err", Duration: time.Second})
	if rec.GuardExit == nil || *rec.GuardExit != 3 {
		t.Fatalf("GuardExit = %v, want 3", rec.GuardExit)
	}
	if rec.StdoutTail != "out" || rec.StderrTail != "err" {
		t.Fatalf("tails = (%q, %q)", rec.StdoutTail, rec.StderrTail)
	}

	// GuardError has no meaningful exit code — pointer stays nil.
	rec2 := NewTickRecord(SurfaceCloud, "sb1", time.Now(), TickGuardError)
	rec2.ApplyGuard(GuardResult{Kind: GuardError, ExitCode: -1})
	if rec2.GuardExit != nil {
		t.Fatalf("GuardExit = %v, want nil on GuardError", rec2.GuardExit)
	}

	// GuardOK keeps stdout out of the tail fields (it goes to vars, and
	// may be large structured input — the audit only needs failures).
	rec3 := NewTickRecord(SurfaceHostCron, "s", time.Now(), TickFired)
	rec3.ApplyGuard(GuardResult{Kind: GuardOK, ExitCode: 0, Stdout: "payload"})
	if rec3.StdoutTail != "" {
		t.Fatalf("StdoutTail = %q, want empty on GuardOK", rec3.StdoutTail)
	}
	if rec3.GuardExit == nil || *rec3.GuardExit != 0 {
		t.Fatalf("GuardExit = %v, want 0", rec3.GuardExit)
	}
}

func TestToAuditMeta(t *testing.T) {
	at := time.Now().UTC()
	rec := NewTickRecord(SurfaceCloud, "sb1", at, TickFired)
	rec.ScheduleName = "nightly"
	rec.BotID = "bots/sec-audit"
	rec.Cron = "0 2 * * 1"
	rec.RunID = "r9"
	exit := 0
	rec.GuardExit = &exit
	rec.GuardDuration = 1500 * time.Millisecond

	m := rec.ToAuditMeta()
	for k, want := range map[string]any{
		"schema":            TickSchemaVersion,
		"surface":           "cloud",
		"schedule_id":       "sb1",
		"schedule_name":     "nightly",
		"bot_id":            "bots/sec-audit",
		"cron":              "0 2 * * 1",
		"decision":          "fired",
		"run_id":            "r9",
		"guard_exit":        0,
		"guard_duration_ms": int64(1500),
	} {
		if got := m[k]; got != want {
			t.Fatalf("meta[%q] = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}
	if _, ok := m["blocking_run_id"]; ok {
		t.Fatalf("empty fields must be omitted from meta: %v", m)
	}
}
