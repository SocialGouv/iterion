package runview

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestBuildDiagnostic_ProjectsResumableFailureAndRetryEvidence(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "diagnostic-retry"
	if _, err := st.CreateRun(ctx, runID, "workflow", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	retryAt := time.Now().Add(time.Hour).UTC()
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.RetryState = &store.RunRetryState{
		RetryAfter: &retryAt,
		Reason:     "usage_window",
		Code:       string(store.FailureUsageLimitBlocked),
		Attempts:   2,
	}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := st.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailedResumable,
		"provider quota is exhausted", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("UpdateRunStatusCoded: %v", err)
	}
	for _, event := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventRunFailed, RunID: runID, NodeID: "render", Data: map[string]any{
			"error": "provider quota is exhausted", "code": string(store.FailureUsageLimitBlocked), "resumable": true,
		}},
		{Type: store.EventRunRetryScheduled, RunID: runID, NodeID: "render", Data: map[string]any{
			"code": string(store.FailureUsageLimitBlocked), "retry_after": retryAt.Format(time.RFC3339), "attempt": 2,
		}},
	} {
		if _, err := st.AppendEvent(ctx, runID, event); err != nil {
			t.Fatalf("AppendEvent(%s): %v", event.Type, err)
		}
	}

	diagnostic, err := BuildDiagnostic(ctx, st, runID)
	if err != nil {
		t.Fatalf("BuildDiagnostic: %v", err)
	}
	if diagnostic.Version != DiagnosticVersion {
		t.Fatalf("version = %d, want %d", diagnostic.Version, DiagnosticVersion)
	}
	if diagnostic.Outcome != DiagnosticBlocked || !diagnostic.Recoverable {
		t.Fatalf("outcome/recoverable = %q/%v, want blocked/true", diagnostic.Outcome, diagnostic.Recoverable)
	}
	if diagnostic.NextAction != DiagnosticActionWait {
		t.Fatalf("next_action = %q, want wait", diagnostic.NextAction)
	}
	if diagnostic.FailureCode != store.FailureUsageLimitBlocked {
		t.Fatalf("failure_code = %q, want %q", diagnostic.FailureCode, store.FailureUsageLimitBlocked)
	}
	if diagnostic.NodeID != "render" {
		t.Fatalf("node_id = %q, want render", diagnostic.NodeID)
	}
	if len(diagnostic.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(diagnostic.Evidence))
	}
	if got := diagnostic.Evidence[1].Details["retry_after"]; got != retryAt.Format(time.RFC3339) {
		t.Fatalf("retry_after evidence = %q, want %q", got, retryAt.Format(time.RFC3339))
	}
}

func TestBuildDiagnostic_ResumeStartsANewEvidenceEpisode(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "diagnostic-episodes"
	if _, err := st.CreateRun(ctx, runID, "workflow", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailed,
		"second failure", store.FailureSchemaValidation); err != nil {
		t.Fatalf("UpdateRunStatusCoded: %v", err)
	}
	for _, event := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventRunFailed, RunID: runID, NodeID: "first", Data: map[string]any{
			"error": "first failure", "code": string(store.FailureAuthFailed),
		}},
		{Type: store.EventRunResumed, RunID: runID},
		{Type: store.EventRunFailed, RunID: runID, NodeID: "second", Data: map[string]any{
			"error": "second failure", "code": string(store.FailureSchemaValidation),
		}},
	} {
		if _, err := st.AppendEvent(ctx, runID, event); err != nil {
			t.Fatalf("AppendEvent(%s): %v", event.Type, err)
		}
	}

	diagnostic, err := BuildDiagnostic(ctx, st, runID)
	if err != nil {
		t.Fatalf("BuildDiagnostic: %v", err)
	}
	if diagnostic.NodeID != "second" || diagnostic.Message != "second failure" {
		t.Fatalf("current failure = node %q/message %q, want second/second failure", diagnostic.NodeID, diagnostic.Message)
	}
	if len(diagnostic.Evidence) != 1 || diagnostic.Evidence[0].NodeID != "second" {
		t.Fatalf("evidence = %+v, want only the post-resume failure", diagnostic.Evidence)
	}
	if diagnostic.FailureCode != store.FailureSchemaValidation {
		t.Fatalf("failure_code = %q, want %q", diagnostic.FailureCode, store.FailureSchemaValidation)
	}
}

func TestBuildDiagnostic_AllowListsEvidenceFields(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "diagnostic-evidence"
	if _, err := st.CreateRun(ctx, runID, "workflow", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailed,
		"invalid output", store.FailureSchemaValidation); err != nil {
		t.Fatalf("UpdateRunStatusCoded: %v", err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{
		Type: store.EventRunFailed, RunID: runID, NodeID: "validate", Data: map[string]any{
			"error":  "invalid output",
			"code":   string(store.FailureSchemaValidation),
			"detail": "field `items` exceeds its bound",
			"secret": "must never leave the event projection",
			"output": map[string]any{"password": "sensitive"},
		},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	diagnostic, err := BuildDiagnostic(ctx, st, runID)
	if err != nil {
		t.Fatalf("BuildDiagnostic: %v", err)
	}
	if len(diagnostic.Evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(diagnostic.Evidence))
	}
	details := diagnostic.Evidence[0].Details
	if details["detail"] == "" || details["error"] == "" || details["code"] == "" {
		t.Fatalf("expected diagnostic fields in %+v", details)
	}
	if _, ok := details["secret"]; ok {
		t.Fatalf("arbitrary secret field leaked into evidence: %+v", details)
	}
	if _, ok := details["output"]; ok {
		t.Fatalf("output payload leaked into evidence: %+v", details)
	}
}

func TestBuildDiagnostic_ProjectsNestedNodeOutputDetail(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "diagnostic-node-detail"
	if _, err := st.CreateRun(ctx, runID, "workflow", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{
		Type: store.EventNodeFinished, RunID: runID, NodeID: "validate", Data: map[string]any{
			"output": map[string]any{
				"detail": "field items exceeds its bound",
				"secret": "must not be projected",
			},
		},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	diagnostic, err := BuildDiagnostic(ctx, st, runID)
	if err != nil {
		t.Fatalf("BuildDiagnostic: %v", err)
	}
	if len(diagnostic.Evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(diagnostic.Evidence))
	}
	if got := diagnostic.Evidence[0].Details["detail"]; got != "field items exceeds its bound" {
		t.Fatalf("nested detail = %q", got)
	}
	if _, ok := diagnostic.Evidence[0].Details["secret"]; ok {
		t.Fatalf("nested arbitrary output leaked: %+v", diagnostic.Evidence[0].Details)
	}
}

func TestSnapshotBuilder_IncludesTheSameDiagnosticProjection(t *testing.T) {
	run := &store.Run{
		ID:          "snapshot-diagnostic",
		Status:      store.RunStatusFailed,
		FailureCode: store.FailureSchemaValidation,
		Error:       "invalid output",
	}
	builder := NewSnapshotBuilder(run)
	builder.Apply(&store.Event{
		Seq:    1,
		Type:   store.EventRunFailed,
		RunID:  run.ID,
		NodeID: "writer",
		Data: map[string]any{
			"error": "invalid output", "code": string(store.FailureSchemaValidation),
		},
	})

	snapshot := builder.Snapshot()
	if snapshot.Diagnostic == nil {
		t.Fatal("snapshot diagnostic is nil")
	}
	if snapshot.Diagnostic.Version != DiagnosticVersion || snapshot.Diagnostic.NodeID != "writer" {
		t.Fatalf("snapshot diagnostic = %+v", snapshot.Diagnostic)
	}
	if snapshot.Diagnostic.Outcome != DiagnosticFailed || snapshot.Diagnostic.NextAction != DiagnosticActionInspect {
		t.Fatalf("snapshot outcome/action = %q/%q, want failed/inspect", snapshot.Diagnostic.Outcome, snapshot.Diagnostic.NextAction)
	}
}
