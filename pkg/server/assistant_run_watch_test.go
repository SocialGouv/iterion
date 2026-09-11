package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func TestIsSafeAssistantChatPause(t *testing.T) {
	t.Parallel()
	base := func() *store.Run {
		return &store.Run{Status: store.RunStatusPausedWaitingHuman, Checkpoint: &store.Checkpoint{NodeID: "chat"}}
	}
	tests := []struct {
		name   string
		mutate func(*store.Run)
		want   bool
	}{
		{name: "standby chat", mutate: func(*store.Run) {}, want: true},
		{name: "ask user mid turn", mutate: func(r *store.Run) { r.Checkpoint.NodeID = "copi"; r.Checkpoint.BackendPendingToolUseID = "tool-1" }},
		{name: "operator pause", mutate: func(r *store.Run) { r.Status = store.RunStatusPausedOperator }},
		{name: "running", mutate: func(r *store.Run) { r.Status = store.RunStatusRunning }},
		{name: "wrong human node", mutate: func(r *store.Run) { r.Checkpoint.NodeID = "approval" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(r)
			if got := isSafeAssistantChatPause(r, "chat"); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssistantWatchHealthEpisodeProjectionRedactsErrorAndFlagsOverdue(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	ep := runwatch.Episode{
		ID: "episode-1", State: runwatch.EpisodePending,
		NextAttemptAt: now.Add(-assistantWatchSweepInterval * 4),
		LastError:     "assistant_busy: command /secret/path/token=abc failed",
	}
	got := projectAssistantWatchEpisode(ep, now)
	if !got.Due || !got.Overdue {
		t.Fatalf("projection due/overdue = %t/%t, want true/true", got.Due, got.Overdue)
	}
	if got.LastErrorReason != "assistant_busy" {
		t.Fatalf("reason = %q, want assistant_busy", got.LastErrorReason)
	}
	if assistantWatchEpisodeAttention(got, ep.LastError) != "episode_overdue" {
		t.Fatalf("attention = %q, want episode_overdue", assistantWatchEpisodeAttention(got, ep.LastError))
	}
}

func TestAssistantWatchHeartbeatUsesInjectedClock(t *testing.T) {
	start := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	fake := clock.NewFakeClock(start)
	c := &assistantWatchCoordinator{clock: fake}
	c.markSweepStarted(start.Add(-time.Second))
	c.markSweepCompleted(start.Add(-time.Second/2), time.Second)
	if got := c.heartbeat(); got.Stale {
		t.Fatal("fresh heartbeat marked stale")
	}
	fake.Advance(3*assistantWatchSweepInterval + time.Nanosecond)
	if got := c.heartbeat(); !got.Stale {
		t.Fatal("old heartbeat not marked stale")
	}
}

func TestAssistantBudgetNearCap(t *testing.T) {
	t.Parallel()
	r := &store.Run{Budget: &store.RunBudget{MaxTokens: 1000}, Checkpoint: &store.Checkpoint{BudgetTokensUsed: 899}}
	if assistantBudgetNearCap(r) {
		t.Fatal("899/1000 must stay eligible")
	}
	r.Checkpoint.BudgetTokensUsed = 900
	if !assistantBudgetNearCap(r) {
		t.Fatal("90% budget must block automatic wake")
	}
}

func TestAssistantFailureFingerprintIgnoresVolatileTimestamp(t *testing.T) {
	t.Parallel()
	a := &assistantResolvedRun{FailingNode: "build", ErrorCode: "EXECUTION_FAILED", Error: "failed at 2026-08-29T10:00:00Z 10:00:01"}
	b := &assistantResolvedRun{FailingNode: "build", ErrorCode: "EXECUTION_FAILED", Error: "failed at 2026-08-30T11:00:00Z 11:00:01"}
	if assistantFailureFingerprint(a) != assistantFailureFingerprint(b) {
		t.Fatal("volatile timestamps changed fingerprint")
	}
}

func TestWatchIncludesUsesDeclaredOutcomeKinds(t *testing.T) {
	w := runwatch.Watch{Kinds: []string{trigger.KindRunFinished, trigger.KindRunFailed}}
	if !watchIncludes(w, trigger.KindRunFinished) || !watchIncludes(w, trigger.KindRunFailed) {
		t.Fatal("declared kinds were not selected")
	}
	if watchIncludes(w, trigger.KindRunCancelled) {
		t.Fatal("undeclared kind was selected")
	}
}

func TestSuccessfulRewindStatusDoesNotEnterTerminalWatchReconcile(t *testing.T) {
	// observeTerminalState only handles terminal statuses. A completed rewind
	// must therefore be paused_operator, not cancelled, so its active watch is
	// kept for the subsequent explicit resume and any later failure.
	if store.RunStatusPausedOperator.IsTerminal() {
		t.Fatal("paused_operator is terminal; a successful rewind would stop its watch")
	}
}

func TestAssistantWatchStopsOnlyForDefinitiveAssistantOutcomes(t *testing.T) {
	t.Parallel()
	for _, status := range []store.RunStatus{
		store.RunStatusQueued,
		store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusFailedResumable,
	} {
		if assistantWatchStopsForStatus(status) {
			t.Fatalf("recoverable status %s stops watch", status)
		}
	}
	for _, status := range []store.RunStatus{
		store.RunStatusFinished,
		store.RunStatusFailed,
		store.RunStatusCancelled,
	} {
		if !assistantWatchStopsForStatus(status) {
			t.Fatalf("definitive status %s keeps watch", status)
		}
	}
}

func TestSameAssistantWatchIntentIgnoresDeliveryProgressAndKindOrder(t *testing.T) {
	requested := runwatch.Watch{
		TenantID: "tenant", OwnerID: "owner", TargetRunID: "target",
		AssistantRunID: "assistant", Mode: runwatch.ModePropose,
		Kinds:       []string{trigger.KindRunFailed, trigger.KindRunFinished},
		MaxEpisodes: 5, CooldownSeconds: 30,
	}
	existing := requested
	existing.Kinds = []string{trigger.KindRunFinished, trigger.KindRunFailed}
	existing.DeliveredEpisodes = 3
	existing.MaxEpisodes = 20 // Deprecated wire field is not part of intent.
	if !sameAssistantWatchIntent(existing, requested) {
		t.Fatal("delivery progress and kind order must not change create intent")
	}
	existing.AssistantRunID = "another-assistant"
	if sameAssistantWatchIntent(existing, requested) {
		t.Fatal("a different assistant must remain a real conflict")
	}
}

func TestCreateAssistantWatchIsIdempotentAndReconfiguresTheSameAssistant(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "assistant", "assistant-workflow", store.RunStatusPausedWaitingHuman)

	post := func(body string) (int, []byte) {
		t.Helper()
		resp, err := http.Post(
			hs.URL+"/api/runs/target/assistant-watches",
			"application/json",
			bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, payload
	}

	body := `{"assistant_run_id":"assistant","mode":"diagnose"}`
	firstStatus, firstBody := post(body)
	if firstStatus != http.StatusOK {
		t.Fatalf("first create = %d %s", firstStatus, firstBody)
	}
	secondStatus, secondBody := post(body)
	if secondStatus != http.StatusOK {
		t.Fatalf("idempotent create = %d %s", secondStatus, secondBody)
	}
	var first, second runwatch.Watch
	if err := json.Unmarshal(firstBody, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondBody, &second); err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID {
		t.Fatalf("watch ids = %q then %q, want the existing watch", first.ID, second.ID)
	}

	changedStatus, changedBody := post(
		`{"assistant_run_id":"assistant","mode":"propose","kinds":["run.failed","run.stalled"],"max_episodes":20}`,
	)
	if changedStatus != http.StatusOK {
		t.Fatalf("reconfigured intent = %d %s, want 200", changedStatus, changedBody)
	}
	var changed runwatch.Watch
	if err := json.Unmarshal(changedBody, &changed); err != nil {
		t.Fatal(err)
	}
	if changed.ID != first.ID || changed.Mode != runwatch.ModePropose || changed.MaxEpisodes != 0 || len(changed.Kinds) != 2 || changed.Kinds[1] != trigger.KindRunStalled {
		t.Fatalf("watch was not reconfigured in place: %+v", changed)
	}

	seedRun(t, srv, "other-assistant", "assistant-workflow", store.RunStatusPausedWaitingHuman)
	transferStatus, transferBody := post(
		`{"assistant_run_id":"other-assistant","mode":"propose","kinds":["run.failed","run.stalled"],"max_episodes":20}`,
	)
	if transferStatus != http.StatusOK {
		t.Fatalf("paused assistant handoff = %d %s, want 200", transferStatus, transferBody)
	}
	var transferred runwatch.Watch
	if err := json.Unmarshal(transferBody, &transferred); err != nil {
		t.Fatal(err)
	}
	if transferred.ID != first.ID || transferred.AssistantRunID != "other-assistant" || transferred.Mode != runwatch.ModePropose {
		t.Fatalf("handoff did not preserve watch identity and update owner: %+v", transferred)
	}
}

func TestCreateAssistantWatchRejectsRunningIncumbent(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target-running-incumbent", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "incumbent-running", "assistant-workflow", store.RunStatusRunning)
	seedRun(t, srv, "incoming-paused", "assistant-workflow", store.RunStatusPausedWaitingHuman)

	post := func(body string) (int, []byte) {
		t.Helper()
		resp, err := http.Post(
			hs.URL+"/api/runs/target-running-incumbent/assistant-watches",
			"application/json", bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, payload
	}
	if status, body := post(`{"assistant_run_id":"incumbent-running","mode":"diagnose"}`); status != http.StatusOK {
		t.Fatalf("seed watch = %d %s", status, body)
	}
	if status, body := post(`{"assistant_run_id":"incoming-paused","mode":"propose"}`); status != http.StatusConflict {
		t.Fatalf("running incumbent handoff = %d %s, want 409", status, body)
	}
}

func TestCreateAssistantWatchFullResumePolicyPreservesDeliveryLedger(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target-ledger", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "assistant-ledger", "assistant-workflow", store.RunStatusPausedWaitingHuman)

	agedUpdatedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	lastDeliveredAt := agedUpdatedAt.Add(-time.Minute)
	seeded := runwatch.Watch{
		ID:             "watch-ledger",
		TargetRunID:    "target-ledger",
		AssistantRunID: "assistant-ledger",
		Mode:           runwatch.ModePropose,
		Kinds: []string{
			trigger.KindRunPaused,
			trigger.KindRunFailed,
			trigger.KindRunStalled,
			trigger.KindRunFinished,
		},
		State:                runwatch.WatchActive,
		MaxEpisodes:          17,
		DeliveredEpisodes:    3,
		CooldownSeconds:      0,
		LastDeliveredAt:      &lastDeliveredAt,
		LastObservedEventSeq: 2,
		CreatedAt:            agedUpdatedAt.Add(-time.Hour),
		UpdatedAt:            agedUpdatedAt,
	}
	if err := srv.assistantWatches.CreateWatch(context.Background(), seeded); err != nil {
		t.Fatalf("seed watch: %v", err)
	}

	resp, err := http.Post(
		hs.URL+"/api/runs/target-ledger/assistant-watches",
		"application/json",
		bytes.NewBufferString(`{"assistant_run_id":"assistant-ledger","mode":"propose","kinds":["run.paused","run.failed","run.stalled","run.finished"],"cooldown_seconds":0}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idempotent full-policy create = %d %s, want 200", resp.StatusCode, body)
	}
	var got runwatch.Watch
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != seeded.ID || got.DeliveredEpisodes != seeded.DeliveredEpisodes || got.LastObservedEventSeq != seeded.LastObservedEventSeq {
		t.Fatalf("delivery ledger changed: got %+v, seeded %+v", got, seeded)
	}
	if got.LastDeliveredAt == nil || !got.LastDeliveredAt.Equal(lastDeliveredAt) {
		t.Fatalf("last delivered at = %v, want %v", got.LastDeliveredAt, lastDeliveredAt)
	}
	if !got.UpdatedAt.Equal(agedUpdatedAt) {
		t.Fatalf("updated at = %v, want no-write retry to preserve %v", got.UpdatedAt, agedUpdatedAt)
	}
}

func TestCreateAssistantWatchAcceptsExplicitStalledKind(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target-stalled", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "assistant-stalled", "assistant-workflow", store.RunStatusPausedWaitingHuman)

	resp, err := http.Post(
		hs.URL+"/api/runs/target-stalled/assistant-watches",
		"application/json",
		bytes.NewBufferString(`{"assistant_run_id":"assistant-stalled","mode":"diagnose","kinds":["run.stalled"]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var watch runwatch.Watch
	if err := json.NewDecoder(resp.Body).Decode(&watch); err != nil {
		t.Fatal(err)
	}
	if len(watch.Kinds) != 1 || watch.Kinds[0] != trigger.KindRunStalled {
		t.Fatalf("kinds = %v", watch.Kinds)
	}
	if watch.LastObservedEventSeq < -1 {
		t.Fatalf("cursor = %d, want >= -1", watch.LastObservedEventSeq)
	}
}

func TestCreateAssistantWatchAcceptsExplicitPausedKind(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target-paused", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "assistant-paused", "assistant-workflow", store.RunStatusPausedWaitingHuman)

	resp, err := http.Post(
		hs.URL+"/api/runs/target-paused/assistant-watches",
		"application/json",
		bytes.NewBufferString(`{"assistant_run_id":"assistant-paused","mode":"diagnose","kinds":["run.paused"]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var watch runwatch.Watch
	if err := json.NewDecoder(resp.Body).Decode(&watch); err != nil {
		t.Fatal(err)
	}
	if len(watch.Kinds) != 1 || watch.Kinds[0] != trigger.KindRunPaused {
		t.Fatalf("kinds = %v", watch.Kinds)
	}
}

func TestCreateDescendantWatchWidensAndReturnsCoveringAncestor(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "tree-root", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "tree-child", "target-workflow", store.RunStatusRunning)
	seedRun(t, srv, "tree-assistant", "assistant-workflow", store.RunStatusPausedWaitingHuman)
	child, err := srv.runs.LoadRunCtx(context.Background(), "tree-child")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = "tree-root"
	if err := srv.runs.RunStore().SaveRun(context.Background(), child); err != nil {
		t.Fatal(err)
	}

	post := func(target, body string) []byte {
		t.Helper()
		resp, err := http.Post(
			hs.URL+"/api/runs/"+target+"/assistant-watches",
			"application/json",
			bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("post %s = %d %s", target, resp.StatusCode, payload)
		}
		return payload
	}

	rootPayload := post("tree-root", `{"assistant_run_id":"tree-assistant","mode":"diagnose","kinds":["run.failed","run.cancelled"],"cooldown_seconds":300}`)
	var root assistantWatchResponse
	if err := json.Unmarshal(rootPayload, &root); err != nil {
		t.Fatal(err)
	}
	childPayload := post("tree-child", `{"assistant_run_id":"tree-assistant","mode":"propose","kinds":["run.paused","run.failed","run.stalled","run.finished"]}`)
	var covered assistantWatchResponse
	if err := json.Unmarshal(childPayload, &covered); err != nil {
		t.Fatal(err)
	}
	if covered.ID != root.ID || covered.TargetRunID != "tree-root" || covered.CoveredRunID != "tree-child" {
		t.Fatalf("coverage response = %+v, root = %+v", covered, root)
	}
	if covered.Mode != runwatch.ModePropose || covered.CooldownSeconds != 300 {
		t.Fatalf("merged mode/cooldown = %s/%d, want propose/300", covered.Mode, covered.CooldownSeconds)
	}
	wantKinds := []string{
		trigger.KindRunPaused,
		trigger.KindRunFailed,
		trigger.KindRunStalled,
		trigger.KindRunFinished,
		trigger.KindRunCancelled,
	}
	if len(covered.Kinds) != len(wantKinds) {
		t.Fatalf("merged kinds = %v, want five-kind union %v", covered.Kinds, wantKinds)
	}
	for i := range wantKinds {
		if covered.Kinds[i] != wantKinds[i] {
			t.Fatalf("merged kinds = %v, want %v", covered.Kinds, wantKinds)
		}
	}
	if exact, err := srv.assistantWatches.ListActiveByTarget(context.Background(), "", "tree-child"); err != nil || len(exact) != 0 {
		t.Fatalf("nested watches = %+v err=%v, want none", exact, err)
	}

	// Repeating the same child request is idempotent and remains a coverage
	// response rather than creating a second ledger.
	repeatedPayload := post("tree-child", `{"assistant_run_id":"tree-assistant","mode":"propose","kinds":["run.paused","run.failed","run.stalled","run.finished"]}`)
	var repeated assistantWatchResponse
	if err := json.Unmarshal(repeatedPayload, &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ID != root.ID || repeated.CoveredRunID != "tree-child" {
		t.Fatalf("repeated coverage = %+v", repeated)
	}

	resp, err := http.Get(hs.URL + "/api/runs/tree-child/assistant-watches")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed []assistantWatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != root.ID || listed[0].CoveredRunID != "tree-child" {
		t.Fatalf("child coverage listing = %+v", listed)
	}
}
