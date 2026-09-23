package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gateLaunch's monthly run-quota increment IS the metering, so a surface that
// is admitted and then abandons the launch without creating any run spends one
// unit permanently. On the three operator surfaces below the symptom is a 402
// monthly_run_quota_exceeded with no runs to account for it: an ordinary 400 on
// a malformed body, or a 404 on a run id that does not exist, each cost a slot.
//
// The release is a `defer` cancelled at the LAST statement before the run
// service is asked, and that position is the whole point. One statement later —
// after Launch/Resume returned — the handler would release the slot of a run
// that DID start: spawnRun persists the run document and can still fail
// afterwards (the budget-override write), and a cloud publish reports failure
// after the runner already claimed the message (cloudpublisher's own rollback
// exists for that case). An under-count lets an org exceed its paid quota,
// which is worse than the leak. So the tests below assert BOTH directions: the
// pre-call returns release, and a failure OUT OF the run service does not.
func newQuotaReleaseServer(t *testing.T) (*Server, *orgusage.MemoryCounter, context.Context, *store.FilesystemRunStore) {
	t.Helper()
	s := newOrgTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
	return s, counter, ctx, rs
}

func meteredRuns(t *testing.T, c *orgusage.MemoryCounter) int {
	t.Helper()
	u, err := c.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return u.Runs
}

// notAWorkflow compiles nowhere, so a launch/resume carrying it as inline
// source reaches the run service and fails INSIDE it — which is what makes it
// a witness for the "keeps its slot" direction rather than for the other one.
const notAWorkflow = "this is not a workflow\n"

func TestLaunchSurfacesReleaseTheMeteredSlotOnlyBeforeTheRunServiceIsAsked(t *testing.T) {
	t.Run("launch: a malformed body releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		r := httptest.NewRequest("POST", "/api/runs", strings.NewReader("{ this is not json")).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleLaunchRun(w, r)
		if w.Code != 400 {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a 400 that created no run, want 0 — a client looping a bad request would otherwise burn the org's month one call at a time", got)
		}
	})

	t.Run("launch: an unlaunchable request releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		r := httptest.NewRequest("POST", "/api/runs", strings.NewReader(`{}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleLaunchRun(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "required") {
			t.Fatalf("status = %d body = %s, want the 400 that refuses a body naming no workflow", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a refused launch, want 0", got)
		}
	})

	t.Run("launch: a failure OUT OF the run service keeps the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		body, _ := json.Marshal(map[string]any{"source": notAWorkflow})
		r := httptest.NewRequest("POST", "/api/runs", strings.NewReader(string(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleLaunchRun(w, r)
		// The message proves WHICH stage refused. The `launch: ` PREFIX of the
		// error field is written only after s.runs.Launch returned — a bare
		// Contains would also match the pre-call "delegated launch: …" arm,
		// and a guard that cannot fail is worse than none. Without this the
		// sub-test could silently become a second pre-call witness and stop
		// separating the marker's position from its presence.
		if !errorFieldHasPrefix(t, w.Body.Bytes(), "launch: ") {
			t.Fatalf("the request did not reach the run service (status %d, body %s) — this row cannot witness the under-count direction", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 1 {
			t.Fatalf("monthly runs = %d after a launch that reached the run service, want 1 — an error out of Launch does NOT prove no run started (spawnRun persists the doc and can fail after it; a cloud publish reports failure after the message landed), and refunding there lets the org exceed its paid quota", got)
		}
	})

	t.Run("resume: a missing run id releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		r := httptest.NewRequest("POST", "/api/runs//resume", strings.NewReader(`{}`)).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleResumeRun(w, r)
		if w.Code != 400 {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a resume refused before the run service, want 0", got)
		}
	})

	t.Run("resume: an unknown run releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		r := httptest.NewRequest("POST", "/api/runs/nope/resume", strings.NewReader(`{}`)).WithContext(ctx)
		r.SetPathValue("id", "nope")
		w := httptest.NewRecorder()
		s.handleResumeRun(w, r)
		if w.Code != 404 {
			t.Fatalf("status = %d, want 404 (body: %s)", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a 404, want 0", got)
		}
	})

	// The three rows above land on the FIRST returns of each handler's covered
	// block, so the marker could slide most of the way down and nothing would
	// redden. These land on the LAST ones, bracketing it: an invalid timeout
	// is checked after the bot resolution and the path resolution on launch,
	// and after the source resolution on resume.
	t.Run("launch: a late pre-call return still releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		body, _ := json.Marshal(map[string]any{"source": notAWorkflow, "timeout": "nope"})
		r := httptest.NewRequest("POST", "/api/runs", strings.NewReader(string(body))).WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleLaunchRun(w, r)
		if !strings.Contains(w.Body.String(), "invalid timeout") {
			t.Fatalf("this row must land on the timeout check, the last pre-call return: status %d body %s", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a late pre-call refusal, want 0 — the marker has slid up", got)
		}
	})

	t.Run("resume: a late pre-call return still releases the slot", func(t *testing.T) {
		s, counter, ctx, rs := newQuotaReleaseServer(t)
		seedUnresumableRun(t, rs, "run-late")
		body, _ := json.Marshal(map[string]any{"source": notAWorkflow, "timeout": "nope"})
		r := httptest.NewRequest("POST", "/api/runs/run-late/resume", strings.NewReader(string(body))).WithContext(ctx)
		r.SetPathValue("id", "run-late")
		w := httptest.NewRecorder()
		s.handleResumeRun(w, r)
		if !strings.Contains(w.Body.String(), "invalid timeout") {
			t.Fatalf("this row must land on the timeout check: status %d body %s", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a late pre-call refusal, want 0", got)
		}
	})

	// A resume refused by validateResumable — the routine race the WS handler
	// names ("the loser of that race did nothing wrong") — started nothing:
	// that error is raised before any compile, spawn or publish, so the unit
	// goes back even though the run service WAS asked.
	t.Run("resume: an unresumable run gives its slot back although the run service was asked", func(t *testing.T) {
		s, counter, ctx, rs := newQuotaReleaseServer(t)
		seedFinishedRun(t, rs, "run-done")
		body, _ := json.Marshal(map[string]any{"source": notAWorkflow})
		r := httptest.NewRequest("POST", "/api/runs/run-done/resume", strings.NewReader(string(body))).WithContext(ctx)
		r.SetPathValue("id", "run-done")
		w := httptest.NewRecorder()
		s.handleResumeRun(w, r)
		if !strings.Contains(w.Body.String(), "cannot be resumed") {
			t.Fatalf("this row must land on validateResumable: status %d body %s", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a refusal that provably started nothing, want 0 — every lost resume race would meter the studio's own chat", got)
		}
	})

	t.Run("ws answer: an unresumable run gives its slot back too", func(t *testing.T) {
		s, counter, ctx, rs := newQuotaReleaseServer(t)
		seedFinishedRun(t, rs, "run-done-ws")
		id, _ := auth.FromContext(ctx)
		c := &runConn{server: s, runID: "run-done-ws", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
			tenantID: id.TeamID, userID: id.UserID, identity: id}
		payload, _ := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}, "source": notAWorkflow})
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a3", Payload: payload})
		if got := firstWSError(t, c).Code; got != runNotResumableErrorCode {
			t.Fatalf("error code = %q, want %q", got, runNotResumableErrorCode)
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a lost answer race, want 0", got)
		}
	})

	t.Run("resume: a failure OUT OF the run service keeps the slot", func(t *testing.T) {
		s, counter, ctx, rs := newQuotaReleaseServer(t)
		seedUnresumableRun(t, rs, "run-keep")
		body, _ := json.Marshal(map[string]any{"source": notAWorkflow})
		r := httptest.NewRequest("POST", "/api/runs/run-keep/resume", strings.NewReader(string(body))).WithContext(ctx)
		r.SetPathValue("id", "run-keep")
		w := httptest.NewRecorder()
		s.handleResumeRun(w, r)
		if w.Code == 404 || w.Code == 400 && strings.Contains(w.Body.String(), "missing run id") {
			t.Fatalf("the request did not reach the run service (status %d, body %s)", w.Code, w.Body.String())
		}
		if got := meteredRuns(t, counter); got != 1 {
			t.Fatalf("monthly runs = %d after a resume that reached the run service, want 1 — a resume publish can report an error after the runner claimed the revision it published", got)
		}
	})

	t.Run("ws answer: an empty answer set releases the slot", func(t *testing.T) {
		s, counter, ctx, _ := newQuotaReleaseServer(t)
		id, _ := auth.FromContext(ctx)
		c := &runConn{server: s, runID: "run-x", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
			tenantID: id.TeamID, userID: id.UserID, identity: id}
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a1", Payload: json.RawMessage(`{"answers":{}}`)})
		if got := firstWSError(t, c).Code; got != "no_answers" {
			t.Fatalf("error code = %q, want no_answers — the row must land on a return BEFORE the run service", got)
		}
		if got := meteredRuns(t, counter); got != 0 {
			t.Fatalf("monthly runs = %d after a WS answer refused before the run service, want 0", got)
		}
	})

	t.Run("ws answer: a failure OUT OF the run service keeps the slot", func(t *testing.T) {
		s, counter, ctx, rs := newQuotaReleaseServer(t)
		seedUnresumableRun(t, rs, "run-ws")
		id, _ := auth.FromContext(ctx)
		c := &runConn{server: s, runID: "run-ws", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
			tenantID: id.TeamID, userID: id.UserID, identity: id}
		payload, _ := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}, "source": notAWorkflow})
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a2", Payload: payload})
		if got := firstWSError(t, c).Code; got == "no_answers" || got == "run_not_found" {
			t.Fatalf("the WS answer did not reach the run service (code %q)", got)
		}
		if got := meteredRuns(t, counter); got != 1 {
			t.Fatalf("monthly runs = %d after a WS answer that reached the run service, want 1", got)
		}
	})
}

// seedUnresumableRun parks a run the resume surfaces can LOAD (so they get past
// their 404) and whose recorded source cannot compile (so the run service is
// the stage that refuses).
func seedUnresumableRun(t *testing.T, rs *store.FilesystemRunStore, id string) {
	t.Helper()
	if _, err := rs.CreateRun(context.Background(), id, "probe", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	run, err := rs.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	run.WorkflowSource = notAWorkflow
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

// seedFinishedRun parks a run in a terminal state: the resume surfaces load it,
// then validateResumable refuses it — before any compile, spawn or publish.
func seedFinishedRun(t *testing.T, rs *store.FilesystemRunStore, id string) {
	t.Helper()
	if _, err := rs.CreateRun(context.Background(), id, "probe", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	run, err := rs.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFinished
	run.WorkflowSource = notAWorkflow
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
}

// errorFieldHasPrefix reads the JSON `error` field rather than the whole body,
// so a stage guard cannot be satisfied by a substring written somewhere else.
func errorFieldHasPrefix(t *testing.T, body []byte, prefix string) bool {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	return strings.HasPrefix(payload.Error, prefix)
}
