package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Every launch that started a run, or may have, has its `run.launched`
// audit line, whatever launched it: the signed-in user behind POST
// /api/runs, the operator's "launch now" on a pipeline card, the synthetic
// webhook principal of an inbound delivery, the cloud schedule tick, the
// board dispatcher, the trigger spine. One tenant-scoped row per run, keyed
// by run id, naming the bot and the trigger — never the launch vars; a
// launch that errored yet may execute carries meta.outcome. The line is
// written at the one site
// every launch surface crosses (Server.launchRun), and the sweep below
// keeps that site the only one. A fork — the one other way a run comes to
// exist — writes `run.forked`, naming its parent.

// launchAuditSweepAllowed lists the files that may call a Launch method
// directly, with the reason.
var launchAuditSweepAllowed = map[string]string{
	"runs_launch_audit.go": "the audited choke point itself",
}

// TestLaunchAuditSweep_NoRawRunServiceLaunch: a launch surface that asks the
// run service directly is the one launch nobody audited. Every surface
// crosses Server.launchRun (or launchRunWith), or names itself here with
// its reason. The sweep matches the GESTURE — any `.Launch(` call,
// whatever the receiver is named (an alias of the service, a field of
// another name) — not the identifier `runs`; a Launch method of another
// type names its file here with the reason it is not a run launch.
func TestLaunchAuditSweep_NoRawRunServiceLaunch(t *testing.T) {
	raw := regexp.MustCompile(`\.Launch\(`)
	sweepServerFiles(t, func(name, body string) {
		if !raw.MatchString(body) {
			return
		}
		if _, ok := launchAuditSweepAllowed[name]; ok {
			return
		}
		t.Errorf("%s calls the run service's Launch directly — route it through Server.launchRun so the run gets its run.launched audit line, or add an allowlist entry with its reason", name)
	})
}

// newLaunchAuditServer is a cloud-shaped server carrying one org+team, the
// probe bot in its catalog, a run service whose publisher is the caller's,
// and an in-memory audit store — the oracle every witness below reads.
func newLaunchAuditServer(t *testing.T, pub runview.LaunchPublisher) (*Server, *store.FilesystemRunStore, context.Context) {
	t.Helper()
	s := newOrgTestServer(t)
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(boardGateProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	s.auditStore = audit.NewMemoryStore()
	return s, rs, ctx
}

// waitForLaunchAudit polls the tenant's audit until one run.launched row
// lands — the insert is best-effort and detached, so the assertion waits
// for the effect instead of racing it.
func waitForLaunchAudit(t *testing.T, st audit.Store, tenant string) audit.Event {
	t.Helper()
	return waitForTenantAudit(t, st, tenant, auditActionRunLaunched)[0]
}

func countLaunchAudit(t *testing.T, st audit.Store, tenant string) int {
	t.Helper()
	events, err := st.ListByTenant(context.Background(), tenant, audit.Page{Action: auditActionRunLaunched})
	if err != nil {
		t.Fatalf("list tenant audit: %v", err)
	}
	return len(events)
}

// TestLaunchAudit_HTTPLaunchIsAttributedToTheUser: POST /api/runs writes one
// run.launched row on the caller's team — the signed-in user as actor, the
// request's address and user-agent, the run id as target, the bot in meta.
func TestLaunchAudit_HTTPLaunchIsAttributedToTheUser(t *testing.T) {
	pub := &countingPublisher{}
	s, _, ctx := newLaunchAuditServer(t, pub)

	r := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"bot_id":"probe"}`)).WithContext(ctx)
	r.RemoteAddr = "203.0.113.7:4242"
	r.Header.Set("User-Agent", "iterion-cli/test")
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var out launchRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode launch response: %v", err)
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != "u1" || e.ActorKind != "user" {
		t.Errorf("actor = %s/%s, want user/u1 — the signed-in launcher", e.ActorKind, e.ActorID)
	}
	if e.Target != "run" || e.TargetID != out.RunID || out.RunID == "" {
		t.Errorf("target = %s/%q, want run/%q — the line is keyed by the run it describes", e.Target, e.TargetID, out.RunID)
	}
	if e.IP != "203.0.113.7" || e.UserAgent != "iterion-cli/test" {
		t.Errorf("ip=%q user_agent=%q: an HTTP launch records the request it served (203.0.113.7, iterion-cli/test)", e.IP, e.UserAgent)
	}
	if e.Meta["bot"] != "probe" || e.Meta["trigger"] != launchTriggerAPI {
		t.Errorf("meta = %v, want bot=probe trigger=api — a CLI client with no provenance of its own is an API launch", e.Meta)
	}
	if _, leaked := e.Meta["vars"]; leaked {
		t.Errorf("meta carries the launch vars: %v", e.Meta)
	}
	if _, failed := e.Meta["outcome"]; failed {
		t.Errorf("meta = %v: an accepted launch carries no outcome", e.Meta)
	}
}

// TestLaunchAudit_PipelineLaunchNowIsAttributedToTheOperator: the
// operator's "launch now" on a pipeline card — driven through its route
// handler — launches on a context detached from the request, which names
// nobody; the line is still the operator's, on their team, from their
// browser, as a studio launch.
func TestLaunchAudit_PipelineLaunchNowIsAttributedToTheOperator(t *testing.T) {
	var runID string
	pub := &countingPublisher{onLaunch: func(id string) { runID = id }}
	s, _, ctx := newLaunchAuditServer(t, pub)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	iss, err := board.Create(native.Issue{Title: "ticket", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline-board/tasks/"+iss.ID+"/launch", nil).WithContext(ctx)
	r.SetPathValue("id", iss.ID)
	r.RemoteAddr = "203.0.113.8:4242"
	r.Header.Set("User-Agent", "Mozilla/5.0 (studio)")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.handlePipelineBoardTaskLaunch(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch now = %d %s, want 202", rec.Code, rec.Body.String())
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != "u1" || e.ActorKind != "user" {
		t.Errorf("actor = %s/%s, want user/u1 — the operator who dropped the card, read off the request the detached launch served", e.ActorKind, e.ActorID)
	}
	if e.TenantID != "t1" || e.TargetID != runID || runID == "" {
		t.Errorf("tenant=%q target_id=%q, want t1/%q", e.TenantID, e.TargetID, runID)
	}
	if e.IP != "203.0.113.8" || e.UserAgent != "Mozilla/5.0 (studio)" {
		t.Errorf("ip=%q user_agent=%q, want the operator's request (203.0.113.8, Mozilla/5.0 (studio))", e.IP, e.UserAgent)
	}
	if e.Meta["trigger"] != launchTriggerStudio || e.Meta["bot"] != "probe" {
		t.Errorf("meta = %v, want trigger=studio bot=probe — a browser request with no provenance of its own is a studio launch", e.Meta)
	}
}

// TestLaunchAudit_AForkIsAttributedToTheOperator: a fork is a new run that
// never crosses the launch choke point; POST /api/runs/{id}/fork writes its
// own run.forked row — the operator as actor, the child as target, the
// parent in meta.
func TestLaunchAudit_AForkIsAttributedToTheOperator(t *testing.T) {
	pub := &countingPublisher{}
	s, rs, ctx := newLaunchAuditServer(t, pub)
	const parentID = "run-parent"
	if _, err := rs.CreateRun(ctx, parentID, "probe", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	parent, err := rs.LoadRun(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent.Status = store.RunStatusCancelled
	parent.Checkpoint = &store.Checkpoint{NodeID: "review"}
	if err := rs.SaveRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "review", TurnIndex: 0, Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "review/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/runs/"+parentID+"/fork", strings.NewReader(`{"node_id":"review"}`)).WithContext(ctx)
	r.SetPathValue("id", parentID)
	r.RemoteAddr = "203.0.113.5:4242"
	rec := httptest.NewRecorder()
	s.handleForkRun(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fork: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out runview.ForkResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode fork response: %v", err)
	}

	e := waitForTenantAudit(t, s.auditStore, "t1", auditActionRunForked)[0]
	if e.ActorID != "u1" || e.ActorKind != "user" {
		t.Errorf("actor = %s/%s, want user/u1", e.ActorKind, e.ActorID)
	}
	if e.Target != "run" || e.TargetID != out.NewRunID || out.NewRunID == "" {
		t.Errorf("target = %s/%q, want run/%q — the child run is the line's subject", e.Target, e.TargetID, out.NewRunID)
	}
	if e.Meta["parent_run_id"] != parentID || e.IP != "203.0.113.5" {
		t.Errorf("meta=%v ip=%q, want parent_run_id=%s and the operator's address", e.Meta, e.IP, parentID)
	}
}

// TestLaunchAudit_WebhookLaunchIsAttributedToTheDelivery: a run an inbound
// delivery launches — driven through the generic intake, the real launcher
// behind it — is the webhook principal's, from the address the intake
// resolved and the forge's user-agent, tied to the delivery row through
// meta.delivery_id.
func TestLaunchAudit_WebhookLaunchIsAttributedToTheDelivery(t *testing.T) {
	var runID string
	pub := &countingPublisher{onLaunch: func(id string) { runID = id }}
	s, rs, _ := newLaunchAuditServer(t, pub)
	s.cfg.Store = fakeActiveStore{RunStore: rs}
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	s.webhookCounter = webhooks.NewMemoryCounter()
	s.authLimiter = newAuthRateLimiter()
	cfg := webhooks.Config{ID: "gnw", TenantID: "t1", Provider: webhooks.ProviderGeneric, Enabled: true, BotIDs: []string{"probe"}}

	r := genReq(gitlabCtx(cfg), `{"bot":"probe","vars":{},"idempotency_key":"evt-audit-1","repo_url":"https://example.com/acme/repo.git","repo_ref":"main"}`)
	r.RemoteAddr = "203.0.113.9:4242"
	r.Header.Set("User-Agent", "probe-forge/1.0")
	rec := httptest.NewRecorder()
	s.handleGenericWebhook(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the delivery did not launch: code=%d body=%s", rec.Code, rec.Body.String())
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != "webhook:gnw" || e.ActorKind != "webhook" {
		t.Errorf("actor = %s/%s, want webhook/webhook:gnw", e.ActorKind, e.ActorID)
	}
	if e.TargetID != runID || runID == "" {
		t.Errorf("target_id = %q, want the launched run %q", e.TargetID, runID)
	}
	if e.IP != "203.0.113.9" || e.UserAgent != "probe-forge/1.0" {
		t.Errorf("ip=%q user_agent=%q, want the delivery's source address 203.0.113.9 and probe-forge/1.0", e.IP, e.UserAgent)
	}
	deliveryID, _ := e.Meta["delivery_id"].(string)
	if deliveryID == "" || e.Meta["trigger"] != store.RunSourceKindWebhook {
		t.Fatalf("meta = %v, want a delivery_id and trigger=webhook", e.Meta)
	}
	d, err := s.webhookDeliveries.Get(context.Background(), deliveryID)
	if err != nil {
		t.Fatalf("the delivery the line names (%s) is not on record: %v", deliveryID, err)
	}
	if d.RunID != runID {
		t.Errorf("delivery %s carries run %q, want %q — the line and the row name the same run", deliveryID, d.RunID, runID)
	}
}

// TestLaunchAudit_ScheduledLaunchIsASystemLine: the cloud schedule tick
// launches as the scheduler, a system actor with no address, and the line
// names the schedule that fired.
func TestLaunchAudit_ScheduledLaunchIsASystemLine(t *testing.T) {
	pub := &countingPublisher{}
	s, _, _ := newLaunchAuditServer(t, pub)

	if err := s.launchScheduledBot(boundedCtx(t), cloudsched.ScheduledBot{ID: "sb-1", TenantID: "t1", BotID: "probe"}); err != nil {
		t.Fatalf("launchScheduledBot: %v", err)
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != "scheduler:probe" || e.ActorKind != "system" {
		t.Errorf("actor = %s/%s, want system/scheduler:probe", e.ActorKind, e.ActorID)
	}
	if e.IP != "" || e.UserAgent != "" {
		t.Errorf("ip=%q user_agent=%q: a schedule tick has no request to record", e.IP, e.UserAgent)
	}
	if e.Meta["schedule_id"] != "sb-1" || e.Meta["trigger"] != store.RunSourceKindSchedule || e.Meta["bot"] != "probe" {
		t.Errorf("meta = %v, want schedule_id=sb-1 trigger=schedule bot=probe", e.Meta)
	}
}

// TestLaunchAudit_BoardDispatchIsASystemLine: a card the board dispatcher
// launches is audited as the dispatcher, on the card's team.
func TestLaunchAudit_BoardDispatchIsASystemLine(t *testing.T) {
	pub := &countingPublisher{}
	s, rs, _ := newLaunchAuditServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }

	if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{ID: "native:1", Bot: "probe", State: native.StateReady}); err != nil {
		t.Fatalf("processBoardCard = %v, want nil for a finished run", err)
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != boardDispatcherActor || e.ActorKind != "system" {
		t.Errorf("actor = %s/%s, want system/%s", e.ActorKind, e.ActorID, boardDispatcherActor)
	}
	if e.TenantID != "t1" || e.Target != "run" || e.TargetID == "" {
		t.Errorf("tenant=%q target=%s/%q, want the card's team and the launched run", e.TenantID, e.Target, e.TargetID)
	}
	if e.Meta["bot"] != "probe" {
		t.Errorf("meta.bot = %v, want probe", e.Meta["bot"])
	}
}

// TestLaunchAudit_TriggerSpineIsASystemLine: the spine's direct launch is
// audited as the spine, on the subscription's team.
func TestLaunchAudit_TriggerSpineIsASystemLine(t *testing.T) {
	pub := &spinePublisher{}
	s, _, _ := newLaunchAuditServer(t, pub)

	runID, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1"))
	if err != nil {
		t.Fatalf("spine launch: %v", err)
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.ActorID != triggerSpineActor || e.ActorKind != "system" {
		t.Errorf("actor = %s/%s, want system/%s", e.ActorKind, e.ActorID, triggerSpineActor)
	}
	if e.TargetID != runID {
		t.Errorf("target_id = %q, want the launched run %q", e.TargetID, runID)
	}
}

// TestLaunchAudit_ALaunchThatMayHaveStartedIsAudited: a publish that
// reports failure after its message may have landed is a run the runner may
// execute — the 503 the client reads is not the end of it. The launch writes
// its line all the same, keyed by the run the publisher was handed, marked
// meta.outcome=may_have_started.
func TestLaunchAudit_ALaunchThatMayHaveStartedIsAudited(t *testing.T) {
	var runID string
	pub := &countingPublisher{
		err:      queueLandedError(),
		onLaunch: func(id string) { runID = id },
	}
	s, _, ctx := newLaunchAuditServer(t, pub)

	if rec := postLaunch(t, s, ctx, `{"bot_id":"probe"}`); rec.Code == http.StatusAccepted {
		t.Fatalf("the failed publish answered 202: %s", rec.Body.String())
	}

	e := waitForLaunchAudit(t, s.auditStore, "t1")
	if e.TargetID != runID || runID == "" {
		t.Errorf("target_id = %q, want the run the publisher was handed (%q) — the error names the run it may have left behind", e.TargetID, runID)
	}
	if e.Meta["outcome"] != launchOutcomeMayHaveStarted || e.ActorID != "u1" {
		t.Errorf("meta=%v actor=%q, want outcome=%s and the launching user u1", e.Meta, e.ActorID, launchOutcomeMayHaveStarted)
	}
}

// TestLaunchAudit_ARefusedLaunchWritesNoLine: a launch refused before
// anything could execute has nothing to audit — the publisher's refusal
// leaves the trail empty, and the next accepted launch is the first row.
func TestLaunchAudit_ARefusedLaunchWritesNoLine(t *testing.T) {
	pub := &countingPublisher{err: errors.New("PROBE: the queue refused the publish")}
	s, _, ctx := newLaunchAuditServer(t, pub)

	if rec := postLaunch(t, s, ctx, `{"bot_id":"probe"}`); rec.Code == http.StatusAccepted {
		t.Fatalf("the refused launch answered 202: %s", rec.Body.String())
	}
	pub.err = nil
	if rec := postLaunch(t, s, ctx, `{"bot_id":"probe"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	waitForLaunchAudit(t, s.auditStore, "t1")
	if n := countLaunchAudit(t, s.auditStore, "t1"); n != 1 {
		t.Errorf("%d run.launched rows, want 1 — the refused launch has no run to audit", n)
	}
}
