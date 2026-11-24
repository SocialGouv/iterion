package lane

import (
	"encoding/json"
	"testing"
)

func TestLaneEventNamesSerialization(t *testing.T) {
	cases := []struct {
		name     LaneEventName
		expected string
	}{
		{EventStarted, "lane.started"},
		{EventReady, "lane.ready"},
		{EventPromptMisdelivery, "lane.prompt_misdelivery"},
		{EventBlocked, "lane.blocked"},
		{EventRed, "lane.red"},
		{EventGreen, "lane.green"},
		{EventCommitCreated, "lane.commit.created"},
		{EventPrOpened, "lane.pr.opened"},
		{EventMergeReady, "lane.merge.ready"},
		{EventFinished, "lane.finished"},
		{EventFailed, "lane.failed"},
		{EventReconciled, "lane.reconciled"},
		{EventMerged, "lane.merged"},
		{EventSuperseded, "lane.superseded"},
		{EventClosed, "lane.closed"},
		{EventBranchStaleAgainstMain, "branch.stale_against_main"},
	}

	for _, tc := range cases {
		data, err := json.Marshal(tc.name)
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.name, err)
		}
		var got string
		json.Unmarshal(data, &got)
		if got != tc.expected {
			t.Errorf("LaneEventName %s serialized to %q, want %q", tc.name, got, tc.expected)
		}
	}
}

func TestFailureClassesSerialization(t *testing.T) {
	cases := []struct {
		fc       LaneFailureClass
		expected string
	}{
		{FailurePromptDelivery, "prompt_delivery"},
		{FailureTrustGate, "trust_gate"},
		{FailureBranchDivergence, "branch_divergence"},
		{FailureCompile, "compile"},
		{FailureTest, "test"},
		{FailurePluginStartup, "plugin_startup"},
		{FailureMcpStartup, "mcp_startup"},
		{FailureMcpHandshake, "mcp_handshake"},
		{FailureGatewayRouting, "gateway_routing"},
		{FailureToolRuntime, "tool_runtime"},
		{FailureInfra, "infra"},
	}

	for _, tc := range cases {
		data, err := json.Marshal(tc.fc)
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.fc, err)
		}
		var got string
		json.Unmarshal(data, &got)
		if got != tc.expected {
			t.Errorf("LaneFailureClass %s serialized to %q, want %q", tc.fc, got, tc.expected)
		}
	}
}

func TestBlockedAndFailedEventsReuseBlockerDetails(t *testing.T) {
	blocker := &LaneEventBlocker{
		FailureClass: FailureMcpStartup,
		Detail:       "broken server",
	}

	blocked := Blocked("2026-04-04T00:00:00Z", blocker)
	failed := Failed("2026-04-04T00:00:01Z", blocker)

	if blocked.Event != EventBlocked {
		t.Errorf("blocked.Event = %s", blocked.Event)
	}
	if blocked.Status != StatusBlocked {
		t.Errorf("blocked.Status = %s", blocked.Status)
	}
	if blocked.FailureClass == nil || *blocked.FailureClass != FailureMcpStartup {
		t.Errorf("blocked.FailureClass = %v", blocked.FailureClass)
	}
	if failed.Event != EventFailed {
		t.Errorf("failed.Event = %s", failed.Event)
	}
	if failed.Status != StatusFailed {
		t.Errorf("failed.Status = %s", failed.Status)
	}
	if failed.Detail == nil || *failed.Detail != "broken server" {
		t.Errorf("failed.Detail = %v", failed.Detail)
	}
}

func TestCommitEventsCarryWorktreeAndSupersessionMetadata(t *testing.T) {
	wt := "wt-a"
	cc := "abc123"
	detail := "commit created"
	event := CommitCreated(
		"2026-04-04T00:00:00Z",
		&detail,
		LaneCommitProvenance{
			Commit:          "abc123",
			Branch:          "feature/provenance",
			Worktree:        &wt,
			CanonicalCommit: &cc,
			Lineage:         []string{"abc123"},
		},
	)

	var eventJSON map[string]interface{}
	data, _ := json.Marshal(event)
	json.Unmarshal(data, &eventJSON)

	if eventJSON["event"] != "lane.commit.created" {
		t.Errorf("event = %v", eventJSON["event"])
	}
	dataField, ok := eventJSON["data"].(map[string]interface{})
	if !ok {
		t.Fatal("data field missing or wrong type")
	}
	if dataField["branch"] != "feature/provenance" {
		t.Errorf("data.branch = %v", dataField["branch"])
	}
	if dataField["worktree"] != "wt-a" {
		t.Errorf("data.worktree = %v", dataField["worktree"])
	}
}

func TestDedupeSupersededCommitEvents(t *testing.T) {
	wta := "wt-a"
	wtb := "wt-b"
	oldDetail := "old"
	newDetail := "new"
	cc := "canon123"
	superseded := "new123"

	retained := DedupeSupersededCommitEvents([]LaneEvent{
		CommitCreated(
			"2026-04-04T00:00:00Z",
			&oldDetail,
			LaneCommitProvenance{
				Commit:          "old123",
				Branch:          "feature/provenance",
				Worktree:        &wta,
				CanonicalCommit: &cc,
				SupersededBy:    &superseded,
				Lineage:         []string{"old123", "new123"},
			},
		),
		CommitCreated(
			"2026-04-04T00:00:01Z",
			&newDetail,
			LaneCommitProvenance{
				Commit:          "new123",
				Branch:          "feature/provenance",
				Worktree:        &wtb,
				CanonicalCommit: &cc,
				Lineage:         []string{"old123", "new123"},
			},
		),
	})

	if len(retained) != 1 {
		t.Fatalf("expected 1 event, got %d", len(retained))
	}
	if retained[0].Detail == nil || *retained[0].Detail != "new" {
		t.Errorf("retained detail = %v, want 'new'", retained[0].Detail)
	}
}

func TestLaneEventJSONRoundTrip(t *testing.T) {
	e := Started("2026-04-04T00:00:00Z")
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LaneEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != EventStarted {
		t.Errorf("decoded.Event = %s", decoded.Event)
	}
	if decoded.Status != StatusRunning {
		t.Errorf("decoded.Status = %s", decoded.Status)
	}
	if decoded.EmittedAt != "2026-04-04T00:00:00Z" {
		t.Errorf("decoded.EmittedAt = %s", decoded.EmittedAt)
	}
}

// --- Typed LaneEventData tests ---

func TestTypedDataCommitProvenanceMarshalRoundTrip(t *testing.T) {
	wt := "wt-a"
	cc := "canon123"
	detail := "commit"
	event := CommitCreated("2026-04-04T00:00:00Z", &detail, LaneCommitProvenance{
		Commit:          "abc123",
		Branch:          "feature/typed",
		Worktree:        &wt,
		CanonicalCommit: &cc,
	})

	// Verify TypedData is set.
	if event.TypedData == nil {
		t.Fatal("TypedData should be set")
	}
	cpd, ok := event.TypedData.(CommitProvenanceData)
	if !ok {
		t.Fatalf("expected CommitProvenanceData, got %T", event.TypedData)
	}
	if cpd.Commit != "abc123" {
		t.Errorf("commit = %q", cpd.Commit)
	}

	// Marshal and unmarshal.
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LaneEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	// Verify typed data is reconstructed.
	if decoded.TypedData == nil {
		t.Fatal("TypedData should be reconstructed from JSON")
	}
	cpd2, ok := decoded.TypedData.(CommitProvenanceData)
	if !ok {
		t.Fatalf("expected CommitProvenanceData, got %T", decoded.TypedData)
	}
	if cpd2.Branch != "feature/typed" {
		t.Errorf("branch = %q", cpd2.Branch)
	}
}

func TestTypedDataRawJSONBackwardCompat(t *testing.T) {
	wt := "wt-a"
	detail := "test"
	event := CommitCreated("2026-04-04T00:00:00Z", &detail, LaneCommitProvenance{
		Commit:   "abc",
		Branch:   "main",
		Worktree: &wt,
	})

	// RawJSON() should return valid JSON.
	raw := event.TypedData.RawJSON()
	if !json.Valid(raw) {
		t.Fatalf("RawJSON() returned invalid JSON: %s", raw)
	}

	// It should contain expected fields.
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["commit"] != "abc" {
		t.Errorf("commit = %v", parsed["commit"])
	}
}

func TestStaleBranchEventKindString(t *testing.T) {
	tests := []struct {
		kind StaleBranchEventKind
		want string
	}{
		{StaleBranchFresh, "fresh"},
		{StaleBranchStale, "stale"},
		{StaleBranchDiverged, "diverged"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestStaleBranchEventJSONRoundTrip(t *testing.T) {
	sbe := StaleBranchEvent{
		Kind:   StaleBranchStale,
		Detail: "3 commits behind main",
		Behind: 3,
		Fixes:  []string{"fix: timeout", "fix: null ptr"},
	}

	data, err := json.Marshal(sbe)
	if err != nil {
		t.Fatal(err)
	}

	var decoded StaleBranchEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Kind != StaleBranchStale {
		t.Errorf("Kind = %q, want 'stale'", decoded.Kind)
	}
	if decoded.Behind != 3 {
		t.Errorf("Behind = %d, want 3", decoded.Behind)
	}
	if len(decoded.Fixes) != 2 {
		t.Errorf("Fixes count = %d, want 2", len(decoded.Fixes))
	}
}

func TestBranchStaleEventHelper(t *testing.T) {
	sbe := StaleBranchEvent{
		Kind:   StaleBranchDiverged,
		Detail: "diverged: 2 ahead, 1 behind",
		Ahead:  2,
		Behind: 1,
	}
	event := BranchStale("2026-04-04T00:00:00Z", sbe)

	if event.Event != EventBranchStaleAgainstMain {
		t.Errorf("Event = %s", event.Event)
	}
	if event.Status != StatusBlocked {
		t.Errorf("Status = %s", event.Status)
	}
	if event.TypedData == nil {
		t.Fatal("TypedData should be set")
	}
	sbed, ok := event.TypedData.(StaleBranchEventData)
	if !ok {
		t.Fatalf("expected StaleBranchEventData, got %T", event.TypedData)
	}
	if sbed.Kind != StaleBranchDiverged {
		t.Errorf("Kind = %q", sbed.Kind)
	}

	// Marshal round-trip.
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded LaneEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TypedData == nil {
		t.Fatal("TypedData should be reconstructed")
	}
	sbed2, ok := decoded.TypedData.(StaleBranchEventData)
	if !ok {
		t.Fatalf("expected StaleBranchEventData, got %T", decoded.TypedData)
	}
	if sbed2.Ahead != 2 || sbed2.Behind != 1 {
		t.Errorf("ahead=%d behind=%d", sbed2.Ahead, sbed2.Behind)
	}
}

func TestRawEventDataWrapsArbitraryJSON(t *testing.T) {
	raw := RawEventData{Raw: json.RawMessage(`{"custom":"field","value":42}`)}
	if !json.Valid(raw.RawJSON()) {
		t.Fatal("RawJSON() should return valid JSON")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(raw.RawJSON(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["custom"] != "field" {
		t.Errorf("custom = %v", parsed["custom"])
	}
}

func TestLaneEventUnmarshalUnknownDataType(t *testing.T) {
	// An event with unknown event type should wrap data as RawEventData.
	input := `{"event":"lane.ready","status":"ready","emittedAt":"2026-04-04T00:00:00Z","data":{"foo":"bar"}}`
	var event LaneEvent
	if err := json.Unmarshal([]byte(input), &event); err != nil {
		t.Fatal(err)
	}
	if event.TypedData == nil {
		t.Fatal("TypedData should be set for unknown data types")
	}
	raw, ok := event.TypedData.(RawEventData)
	if !ok {
		t.Fatalf("expected RawEventData, got %T", event.TypedData)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw.RawJSON(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["foo"] != "bar" {
		t.Errorf("foo = %v", parsed["foo"])
	}
}

func TestFinished_CompressesDetail(t *testing.T) {
	// Install a test compressor that uppercases the input.
	origCompressor := DetailCompressor
	DetailCompressor = func(s string) string {
		return "COMPRESSED:" + s[:10]
	}
	defer func() { DetailCompressor = origCompressor }()

	detail := "This is a very long detail string that should be compressed"
	ev := Finished("2026-01-01T00:00:00Z", &detail)

	if ev.Detail == nil {
		t.Fatal("expected non-nil detail")
	}
	if *ev.Detail != "COMPRESSED:This is a " {
		t.Errorf("expected compressed detail, got %q", *ev.Detail)
	}
}

func TestFinished_NilDetailUnchanged(t *testing.T) {
	origCompressor := DetailCompressor
	DetailCompressor = func(s string) string { return "SHOULD NOT BE CALLED" }
	defer func() { DetailCompressor = origCompressor }()

	ev := Finished("2026-01-01T00:00:00Z", nil)
	if ev.Detail != nil {
		t.Errorf("expected nil detail, got %v", ev.Detail)
	}
}

func TestFinished_EmptyDetailNotCompressed(t *testing.T) {
	origCompressor := DetailCompressor
	called := false
	DetailCompressor = func(s string) string { called = true; return s }
	defer func() { DetailCompressor = origCompressor }()

	empty := ""
	ev := Finished("2026-01-01T00:00:00Z", &empty)
	if called {
		t.Error("compressor should not be called for empty detail")
	}
	if ev.Detail == nil || *ev.Detail != "" {
		t.Errorf("expected empty detail, got %v", ev.Detail)
	}
}

func TestFinished_NoCompressorPassthrough(t *testing.T) {
	origCompressor := DetailCompressor
	DetailCompressor = nil
	defer func() { DetailCompressor = origCompressor }()

	detail := "some detail"
	ev := Finished("2026-01-01T00:00:00Z", &detail)
	if ev.Detail == nil || *ev.Detail != "some detail" {
		t.Errorf("expected passthrough detail, got %v", ev.Detail)
	}
}
