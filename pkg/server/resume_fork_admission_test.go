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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// ADR-162 decision 5's resume half, on the two operator surfaces the round-1
// review found unwitnessed: a fork run's manual resume (POST /runs/{id}/resume
// and its WS answer twin) re-enters the SAME bound the launch entered — the
// run document's TRUST rides the admission subject (so every fail-closed arm
// of the gate applies) and the author's stored metering subject rides it too
// (so the per-contributor twin meters beside the org's). Both halves come
// from the resumeLaunchSubject seam every resume surface crosses; the retry
// sweeper's own witness (webhooks_fork_gesture_test.go) drives the third
// surface through it. The surfaces that cross NO gate (answer-human, an async
// answer's auto-resume, an assistant mission) meet the run service's choke
// point instead (runview.CheckResumeAdmitted): the witnesses below drive two
// of them, and the run service's own (pkg/runview) pins the comparison.

// forkResumePublisher plays the cloud queue for a resumed run: SubmitResume
// succeeds, so the handler's metered unit stays spent — the shape a runner
// claiming the message produces.
type forkResumePublisher struct {
	mu       sync.Mutex
	resumes  int
	launches []runview.LaunchSpec
}

func (p *forkResumePublisher) SubmitLaunch(_ context.Context, _ string, spec runview.LaunchSpec, _ *ir.Workflow, _ *runview.CompiledSource) (runview.LaunchSubmission, error) {
	p.mu.Lock()
	p.launches = append(p.launches, spec)
	p.mu.Unlock()
	return runview.LaunchSubmission{}, nil
}

// launched is every launch spec the run service handed the queue, in order.
func (p *forkResumePublisher) launched() []runview.LaunchSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]runview.LaunchSpec(nil), p.launches...)
}

func (*forkResumePublisher) CancelRun(context.Context, string) error { return nil }
func (*forkResumePublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (p *forkResumePublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	p.mu.Lock()
	p.resumes++
	p.mu.Unlock()
	return nil
}

// forkResumeSubject is the metering subject the seeded fork run carries —
// the same string the launch's admission composed and stamped.
var forkResumeSubject = orgusage.ForkAuthorSubject("t1", "github", "4242")

// newForkResumeServer wires the degraded-identity harness around one paused
// fork run: the gate's team read can be broken mid-test (the fail-closed
// arm's input), the counter the twin meters on is readable, and the run
// service's publisher accepts every resume.
func newForkResumeServer(t *testing.T, status store.RunStatus) (*Server, *orgusage.MemoryCounter, *atomic.Bool, *store.FilesystemRunStore, context.Context) {
	t.Helper()
	fx := newForkResumeFixture(t, status)
	return fx.s, fx.counter, fx.degrade, fx.rs, fx.ctx
}

// forkResumeFixture is newForkResumeServer's whole harness, for the witnesses
// that read the publisher's count or break the run service's own reads.
type forkResumeFixture struct {
	s       *Server
	counter *orgusage.MemoryCounter
	degrade *atomic.Bool
	rs      *store.FilesystemRunStore
	ctx     context.Context
	pub     *forkResumePublisher
	// failNextLoad breaks the run service's NEXT run read once — a store blip
	// between the reads a surface takes.
	failNextLoad *atomic.Bool
}

func newForkResumeFixture(t *testing.T, status store.RunStatus) *forkResumeFixture {
	t.Helper()
	s := newOrgTestServer(t)
	// The WS twin resolves the persisted FilePath against the working
	// directory, so the seeded run's file must live in one.
	s.cfg.WorkDir = t.TempDir()
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	// The gate's identity read is degradable: seeded through the wrapped
	// store first (flag off), broken for the degraded rows only.
	degrade := &atomic.Bool{}
	svc, err := auth.NewService(auth.Config{
		Store:      degradedTeamRead{Store: s.authStore(), err: context.DeadlineExceeded, on: degrade},
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     mustSigner(t),
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s.authSvc = svc
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("run store: %v", err)
	}
	pub := &forkResumePublisher{}
	failNextLoad := &atomic.Bool{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(&blippingRunStore{FilesystemRunStore: rs, failNext: failNextLoad}), runview.WithLaunchPublisher(pub))
	seedForkResumeRun(t, s, rs, "run-fork", status)
	return &forkResumeFixture{s: s, counter: counter, degrade: degrade, rs: rs, ctx: ctx, pub: pub, failNextLoad: failNextLoad}
}

// blippingRunStore fails exactly one run read when armed, then answers again
// — the shape of a transient Mongo blip between two reads of one request.
type blippingRunStore struct {
	*store.FilesystemRunStore
	failNext *atomic.Bool
}

func (b *blippingRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if b.failNext.CompareAndSwap(true, false) {
		return nil, errors.New("mongo: i/o timeout")
	}
	return b.FilesystemRunStore.LoadRun(ctx, id)
}

func (p *forkResumePublisher) resumed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resumes
}

// assertNothingMetered is the refusal rows' verdict: neither the org's unit
// nor the author's was left charged.
func assertNothingMetered(t *testing.T, c *orgusage.MemoryCounter) {
	t.Helper()
	now := time.Now().UTC()
	for _, subject := range []orgusage.Subject{orgusage.OrgSubject("t1"), forkResumeSubject} {
		u, err := c.Usage(context.Background(), subject, now)
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 0 {
			t.Fatalf("%s runs = %d after a refused resume, want 0", subject, u.Runs)
		}
	}
}

// seedForkResumeRun persists a FORK run a resume surface can load and push
// all the way to the publisher: its source compiles, its recorded hash
// matches, and its document carries the trust and the author subject the
// launch's admission stamped (ADR-162 decision 5's durable carriers).
func seedForkResumeRun(t *testing.T, s *Server, rs *store.FilesystemRunStore, runID string, status store.RunStatus) {
	t.Helper()
	seedForkResumeRunAs(t, s, rs, runID, status, string(forkResumeSubject))
}

// seedForkResumeRunAs is seedForkResumeRun with the author subject the
// document carries named by the caller ("" for a fork document that names
// none — a write that lost it, or one older than the field).
func seedForkResumeRunAs(t *testing.T, s *Server, rs *store.FilesystemRunStore, runID string, status store.RunStatus, subject string) {
	t.Helper()
	const source = "workflow fork_review:\n  entry: done\n"
	logicalPath := filepath.Join(s.cfg.WorkDir, "fork_review.bot")
	if err := os.WriteFile(logicalPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, hash, err := runview.CompileWorkflowFromSource(logicalPath, source, "", store.RunTrustFork)
	if err != nil {
		t.Fatalf("compile under fork trust: %v", err)
	}
	ctx := context.Background()
	if _, err := rs.CreateRun(ctx, runID, "fork_review", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := rs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.FilePath = logicalPath
	r.WorkflowSource = source
	r.WorkflowHash = hash
	r.Status = status
	r.BotID = "review-pr"
	r.ProjectPath = "acme/widgets"
	r.Trust = store.RunTrustFork
	r.ForkAuthorSubject = subject
	if err := rs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := rs.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
}

// assertForkTwinMetered is the metering half's verdict: a resumed fork run
// spent the org's unit AND the author's own — the twin the launch metered,
// never the org's alone.
func assertForkTwinMetered(t *testing.T, c *orgusage.MemoryCounter) {
	t.Helper()
	now := time.Now().UTC()
	orgU, err := c.Usage(context.Background(), orgusage.OrgSubject("t1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if orgU.Runs != 1 {
		t.Fatalf("org runs = %d after a resumed fork run, want 1", orgU.Runs)
	}
	authorU, err := c.Usage(context.Background(), forkResumeSubject, now)
	if err != nil {
		t.Fatal(err)
	}
	if authorU.Runs != 1 {
		t.Fatalf("author runs = %d after a resumed fork run, want 1 — the resume re-enters the per-contributor bound the launch metered", authorU.Runs)
	}
}

// TestHandleResumeRun_AForkRunReentersTheForkBound: the REST resume. The
// degraded row is decision 5's "whichever surface enters" — a team the gate
// cannot read admits TRUSTED work unmetered but must REFUSE a fork run; the
// healthy row is the twin's metering.
//
// Mutation: rebuild the subject without run.Trust/run.ForkAuthorSubject at
// the handleResumeRun site (the pre-seam body) → the degraded row resumes
// (no 503) and the healthy row meters the org only.
func TestHandleResumeRun_AForkRunReentersTheForkBound(t *testing.T) {
	t.Run("an unreadable team refuses the resume", func(t *testing.T) {
		s, _, degrade, _, ctx := newForkResumeServer(t, store.RunStatusPausedOperator)
		degrade.Store(true)
		rec := httptest.NewRecorder()
		s.handleResumeRun(rec, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d (%s), want 503 — a fork run's resume is not admitted past a team the gate cannot read", rec.Code, rec.Body.String())
		}
		var body struct {
			ErrorCode string `json:"error_code"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if body.ErrorCode != denyForkTeamUnreadable {
			t.Fatalf("error_code = %q, want %q", body.ErrorCode, denyForkTeamUnreadable)
		}
	})

	t.Run("an admitted resume meters the author beside the org", func(t *testing.T) {
		s, counter, _, _, ctx := newForkResumeServer(t, store.RunStatusPausedOperator)
		rec := httptest.NewRecorder()
		s.handleResumeRun(rec, orgReq(ctx, http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d (%s), want 202 — the healthy row resumes", rec.Code, rec.Body.String())
		}
		assertForkTwinMetered(t, counter)
	})
}

// TestRunsWS_HandleAnswer_AForkRunReentersTheForkBound: the WS answer twin,
// the gated surface that resumes a paused_waiting_human run. Its ungated REST
// twin (answer-human) is refused on a fork run by the run service instead —
// TestHandleAnswerHuman_AForkRunIsRefusedNotResumedUnmetered.
//
// Mutation: the same mute at the runs_ws.go site → both rows red on their
// own assertions.
func TestRunsWS_HandleAnswer_AForkRunReentersTheForkBound(t *testing.T) {
	newConn := func(t *testing.T, s *Server, ctx context.Context) *runConn {
		t.Helper()
		id, _ := auth.FromContext(ctx)
		return &runConn{server: s, runID: "run-fork", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
			tenantID: id.TeamID, userID: id.UserID, identity: id}
	}
	answerPayload := func(t *testing.T) json.RawMessage {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}

	t.Run("an unreadable team refuses the answer", func(t *testing.T) {
		s, _, degrade, _, ctx := newForkResumeServer(t, store.RunStatusPausedWaitingHuman)
		degrade.Store(true)
		c := newConn(t, s, ctx)
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a", Payload: answerPayload(t)})
		if got := firstWSError(t, c).Code; got != denyForkTeamUnreadable {
			t.Fatalf("error code = %q, want %q — a fork run's answer is not admitted past an unreadable team", got, denyForkTeamUnreadable)
		}
	})

	t.Run("an admitted answer meters the author beside the org", func(t *testing.T) {
		s, counter, _, _, ctx := newForkResumeServer(t, store.RunStatusPausedWaitingHuman)
		c := newConn(t, s, ctx)
		c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a", Payload: answerPayload(t)})
		// The resume succeeded: the handler answers an ACK, not an error
		// envelope. (A failure past the gate would surface as an error
		// envelope AND roll both units back, reddening the metering verdict.)
		select {
		case data := <-c.sendCh:
			if !strings.Contains(string(data), `"type":"ack"`) {
				t.Fatalf("the answer did not resume cleanly: %s", data)
			}
		default:
			t.Fatal("no envelope sent — the handler must answer the answer")
		}
		assertForkTwinMetered(t, counter)
	})
}

// TestResumeLaunchSubject_IsTheOneSeamAllResumeSurfacesCross keeps the class
// closed as the package grows: a resume surface that builds its subject by
// hand can drop the run's trust and the author's subject exactly the way
// handleResumeRun and handleAnswer once did. Every constructor of a resume
// subject goes through resumeLaunchSubject, or names itself here with a
// reason.
func TestResumeLaunchSubject_IsTheOneSeamAllResumeSurfacesCross(t *testing.T) {
	allowed := map[string]string{
		"launch_gate.go":                "the seam itself",
		"resume_fork_admission_test.go": "these witnesses",
	}
	handBuilt := regexp.MustCompile(`launchSubject\{ExistingRun:`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go")) {
			continue
		}
		b, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !handBuilt.Match(b) {
			continue
		}
		if _, ok := allowed[name]; !ok {
			t.Errorf("%s builds a resume subject by hand — use resumeLaunchSubject so the run's trust and the author's subject ride every resume, or allowlist the reason", name)
		}
	}
}

// TestHandleAnswerHuman_AForkRunIsRefusedNotResumedUnmetered: POST
// /api/runs/{id}/answer-human is the REST twin of the WS answer, and it
// crosses no admission gate — the run-by-id ladder authorizes it, unmetered,
// on a trusted run. On a fork run that would re-enter the engine past every
// fail-closed arm of decision 5 with neither counter charged, so the run
// service refuses it (runview.CheckResumeAdmitted, the choke point every
// resume crosses): 409 resume_not_admitted, nothing published, nothing
// metered, the run still paused. The degraded row is the review's probe — a
// team the WS twin refuses 503 on.
//
// Mutation: mute CheckResumeAdmitted in runview.Service.Resume → both rows
// answer 202 and the publisher counts a resume (red on its own assertion).
// The publisher is a double, so SubmitResume's own comparison never runs.
func TestHandleAnswerHuman_AForkRunIsRefusedNotResumedUnmetered(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		name := "a readable team"
		if degraded {
			name = "an unreadable team"
		}
		t.Run(name, func(t *testing.T) {
			fx := newForkResumeFixture(t, store.RunStatusPausedWaitingHuman)
			fx.degrade.Store(degraded)
			rec := httptest.NewRecorder()
			fx.s.handleAnswerHuman(rec, orgReq(fx.ctx, http.MethodPost, "/api/runs/run-fork/answer-human", `{"answers":{"q":"a"}}`, "run-fork"))
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d (%s), want 409 — a fork run resumed with no admission runs the author's code unmetered", rec.Code, rec.Body.String())
			}
			var body struct {
				ErrorCode string `json:"error_code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if body.ErrorCode != resumeNotAdmittedErrorCode {
				t.Fatalf("error_code = %q, want %q", body.ErrorCode, resumeNotAdmittedErrorCode)
			}
			if n := fx.pub.resumed(); n != 0 {
				t.Fatalf("the publisher received %d resumes — the refusal must come before anything is published", n)
			}
			assertNothingMetered(t, fx.counter)
			r, err := fx.rs.LoadRun(context.Background(), "run-fork")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != store.RunStatusPausedWaitingHuman {
				t.Fatalf("status = %s, want the run still paused for the gated resume", r.Status)
			}
		})
	}
}

// TestRunsWS_HandleAnswer_ABlipOnTheGatesReadNeverResumes: the WS answer
// reads the run ONCE, for the gate's subject and for the resume. A blip on
// that read is answered before anything resumes — never by a later read the
// gate did not judge, which would resume a fork run admitted as trusted work
// (the gate's unreadable-team arm fails open for it).
//
// Mutation: re-read the run after the gate (the two-read shape) → the second
// read succeeds and the answer reaches the run service, whose choke point
// refuses it as resume_not_admitted instead of the WS answering its own
// failed read (red on the error-code assertion). With the choke point muted
// too, that shape acks a resumed fork run with zero author runs — the
// review's probe.
func TestRunsWS_HandleAnswer_ABlipOnTheGatesReadNeverResumes(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		name := "a readable team"
		if degraded {
			name = "an unreadable team"
		}
		t.Run(name, func(t *testing.T) {
			fx := newForkResumeFixture(t, store.RunStatusPausedWaitingHuman)
			fx.degrade.Store(degraded)
			id, _ := auth.FromContext(fx.ctx)
			c := &runConn{server: fx.s, runID: "run-fork", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
				tenantID: id.TeamID, userID: id.UserID, identity: id}
			payload, err := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}})
			if err != nil {
				t.Fatal(err)
			}
			fx.failNextLoad.Store(true)
			c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a", Payload: payload})
			if got := firstWSError(t, c).Code; got != "run_not_found" {
				t.Fatalf("error code = %q, want run_not_found — the answer must fail on the read its gate judged", got)
			}
			if fx.failNextLoad.Load() {
				t.Fatal("the blip was never consumed — this row proves nothing about the gate's read")
			}
			if n := fx.pub.resumed(); n != 0 {
				t.Fatalf("the publisher received %d resumes after a blip on the gate's read", n)
			}
			assertNothingMetered(t, fx.counter)
		})
	}
}

// TestResumeLaunchSubject_AnUnreadRunIsAnUnknownTrustTheGateRefuses: a nil
// run is NOT the trusted default (ADR-162 decision 8: an unknown trust
// loses). The gate refuses it first — ahead of the local-mode and
// super-admin bypasses, which admit trusted work unmetered — as a transient
// 503 the retry sweeper re-arms.
//
// Mutations: return the bare existing-run subject for nil → every row admits;
// drop the gate's TrustUnknown arm → every row admits; drop the reason from
// retryDenialIsTransient → the last assertion reds.
func TestResumeLaunchSubject_AnUnreadRunIsAnUnknownTrustTheGateRefuses(t *testing.T) {
	cloud := newOrgTestServer(t)
	cloud.orgUsage = orgusage.NewMemoryCounter()
	member := seedGate(t, cloud, gateSpec{id: "t1"})
	for _, row := range []struct {
		name string
		s    *Server
		ctx  context.Context
	}{
		{"a cloud member", cloud, member},
		{"a super-admin", cloud, superAdminCtx()},
		{"local mode", &Server{}, context.Background()},
	} {
		t.Run(row.name, func(t *testing.T) {
			adm, d := row.s.gateLaunch(row.ctx, row.s.resumeLaunchSubject(nil))
			if d == nil {
				adm.rollback(nil)
				t.Fatal("an unread run was admitted — its trust is unknown, and a fork run behind it would re-enter unmetered")
			}
			if d.reason != denyResumeRunUnreadable || d.status != http.StatusServiceUnavailable {
				t.Fatalf("denial = %d %q, want 503 %q", d.status, d.reason, denyResumeRunUnreadable)
			}
		})
	}
	if !retryDenialIsTransient(denyResumeRunUnreadable) {
		t.Fatal("an unread run doc is a store blip: the retry sweeper must re-arm it, never abandon a paid run")
	}
}

// TestSweepDueRetries_AnUnreadableForkRunDocReArmsNotResumes: the review's
// sweeper probe. A due retry of a FORK run whose document read blips — the
// sweeper used to proceed on the retry row alone with the trusted default
// subject: over an unreadable team the gate failed open (author unmetered),
// over a healthy one it metered the org and skipped the author's bound. The
// unknown trust is refused as transient: no resume, nothing metered, the
// retry re-armed for the next pass.
//
// Mutation: return the bare existing-run subject for a nil run in
// resumeLaunchSubject → both rows resume (red on their own assertion).
func TestSweepDueRetries_AnUnreadableForkRunDocReArmsNotResumes(t *testing.T) {
	forkDoc := func() *store.Run {
		return &store.Run{
			ID: "run-a", Status: store.RunStatusFailedResumable,
			BotID: "review-pr", ProjectPath: "acme/widgets",
			Trust:             store.RunTrustFork,
			ForkAuthorSubject: string(orgusage.ForkAuthorSubject("team-1", "github", "4242")),
		}
	}
	for _, degraded := range []bool{false, true} {
		name := "a healthy team"
		if degraded {
			name = "an unreadable team"
		}
		t.Run(name, func(t *testing.T) {
			inner := newFakeRetryStore()
			inner.claimWins["run-a"] = true
			inner.loadRun["run-a"] = forkDoc()
			st := &firstLoadBlipsRetryStore{fakeRetryStore: inner}
			s, counter := newMeteredRetrySweeperServer(t, st)
			degrade := &atomic.Bool{}
			degrade.Store(degraded)
			svc, err := auth.NewService(auth.Config{
				Store:      degradedTeamRead{Store: s.authStore(), err: context.DeadlineExceeded, on: degrade},
				Sessions:   auth.NewMemorySessionStore(),
				Signer:     mustSigner(t),
				SignupMode: auth.SignupOpen,
				RefreshTTL: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			s.authSvc = svc
			resumer := &fakeResumer{}

			at := time.Now().UTC().Add(-time.Minute)
			s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

			if !st.blipped.Load() {
				t.Fatal("the sweeper's run read never blipped — this row proves nothing")
			}
			if len(resumer.calls) != 0 {
				t.Fatalf("a fork run whose document could not be read was resumed (%d calls, admitted %+v) — its trust is unknown", len(resumer.calls), resumer.calls[0].Admitted)
			}
			if len(inner.abandoned) != 0 {
				t.Fatalf("the retry was abandoned (%v) — a blip is not the end of a paid run's retry", inner.abandoned)
			}
			if _, ok := inner.rearmed["run-a"]; !ok {
				t.Fatalf("the retry was not re-armed (rearmed=%v)", inner.rearmed)
			}
			if got := meteredRetryRuns(t, counter); got != 0 {
				t.Fatalf("org runs = %d after a refused retry, want 0", got)
			}
		})
	}
}

// TestSweepDueRetries_AForkRunsResumeCarriesWhatTheGateAdmitted: the sweeper
// hands the run service what its gate judged — the run's trust and the
// author subject the gate charged — so the choke point can compare it with
// the document the resume re-enters.
//
// Mutation: drop Admitted from the sweeper's ResumeSpec → red on its own
// assertion (and, through the real run service, every fork retry would be
// refused as resume_not_admitted).
func TestSweepDueRetries_AForkRunsResumeCarriesWhatTheGateAdmitted(t *testing.T) {
	subject := string(orgusage.ForkAuthorSubject("team-1", "github", "4242"))
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	st.loadRun["run-a"] = &store.Run{
		ID: "run-a", Status: store.RunStatusFailedResumable,
		BotID: "review-pr", ProjectPath: "acme/widgets",
		Trust: store.RunTrustFork, ForkAuthorSubject: subject,
	}
	s, counter := newMeteredRetrySweeperServer(t, st)
	resumer := &fakeResumer{}

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1 (abandoned=%v rearmed=%v)", len(resumer.calls), st.abandoned, st.rearmed)
	}
	got := resumer.calls[0].Admitted
	want := runview.GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: subject}
	if got == nil || *got != want {
		t.Fatalf("Admitted = %+v, want %+v — the run service cannot tell a metered fork retry from an unmetered one", got, want)
	}
	author, err := counter.Usage(context.Background(), orgusage.Subject(subject), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if author.Runs != 1 {
		t.Fatalf("author runs = %d, want 1 — the admission it names must be the one the gate charged", author.Runs)
	}
}

// firstLoadBlipsRetryStore fails the sweeper's FIRST run read (the guard read
// that names the admission subject) and answers every later one — the
// review's probe shape: a blip on the read the gate's subject comes from.
type firstLoadBlipsRetryStore struct {
	*fakeRetryStore
	blipped atomic.Bool
}

func (f *firstLoadBlipsRetryStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if f.blipped.CompareAndSwap(false, true) {
		return nil, errors.New("mongo: i/o timeout")
	}
	return f.fakeRetryStore.LoadRun(ctx, id)
}

// TestAssistantMission_AForkTargetsResumeIsRejectedNotDispatchedUnmetered:
// the assistant mission's run.resume crosses no admission gate, and it
// accepts a failed_resumable target — exactly where the retry sweeper parks a
// fork run it refused on fork_author_cost_cap_exceeded. The run service
// refuses that resume (runview.CheckResumeAdmitted): the receipt is rejected
// with the refusal, and nothing reaches the publisher, so an author already
// over the bound is never continued through a mission.
//
// Mutation: mute CheckResumeAdmitted in runview.Service.Resume → the target's
// resume is dispatched to the publisher (red on its own assertion).
func TestAssistantMission_AForkTargetsResumeIsRejectedNotDispatchedUnmetered(t *testing.T) {
	f := newMissionSlotFixture(t)
	ctx := context.Background()
	rs := f.srv.runs.RunStore()
	target, err := rs.LoadRun(ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	target.Trust = store.RunTrustFork
	target.ForkAuthorSubject = string(forkResumeSubject)
	if err := rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteArtifact(ctx, &store.Artifact{
		RunID: f.assistant, NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionResume,
			"args": map[string]any{"run_id": f.target},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	const id = "mission-fork-resume"
	f.mission(t, id, nil)

	f.coord.attempt(ctx, id) // the proposal becomes a prepared receipt
	f.coord.attempt(ctx, id) // dispatched: refused by the run service
	m := f.get(t, id)
	if len(m.Receipts) != 1 {
		t.Fatalf("receipts = %+v, want the one resume", m.Receipts)
	}
	r := m.Receipts[0]
	if n := f.pub.askedFor(f.target); n != 0 {
		t.Fatalf("the fork target's resume reached the publisher %d times — a mission continued an untrusted run past its bound", n)
	}
	if r.State != assistantmission.ReceiptRejected || !strings.Contains(r.Error, runview.ErrResumeNotAdmitted.Error()) {
		t.Fatalf("receipt = %s %q, want rejected naming the admission refusal", r.State, r.Error)
	}
	got, err := rs.LoadRun(ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunStatusFailedResumable {
		t.Fatalf("target status = %s, want it still parked", got.Status)
	}
}

// TestGateLaunch_TheUnmeteredBypassesRefuseAForkRunsResume: decision 5's two
// bypass arms — local mode and the super-admin — admit trusted work
// unmetered, so each refuses an untrusted subject instead. The super-admin
// row drives the REST resume of a fork run, a surface that reaches the arm;
// the local row drives the gate on the subject the resume seam builds. Each
// row asserts its arm's own denial, not the run service's choke point behind
// it, so neither guard stands in for the other.
//
// Mutations: `if false` for the super-admin arm → the REST row answers the
// choke point's 409 resume_not_admitted instead of the gate's 503; `if false`
// for the local-mode arm → the local row is admitted.
func TestGateLaunch_TheUnmeteredBypassesRefuseAForkRunsResume(t *testing.T) {
	t.Run("a super-admin's REST resume of a fork run", func(t *testing.T) {
		fx := newForkResumeFixture(t, store.RunStatusPausedOperator)
		rec := httptest.NewRecorder()
		fx.s.handleResumeRun(rec, orgReq(superAdminCtx(), http.MethodPost, "/api/runs/run-fork/resume", `{}`, "run-fork"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d (%s), want 503 — the super-admin bypass never admits an untrusted run unmetered", rec.Code, rec.Body.String())
		}
		var body struct {
			ErrorCode string `json:"error_code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if body.ErrorCode != denyForkMeteringUnavailable {
			t.Fatalf("error_code = %q, want %q", body.ErrorCode, denyForkMeteringUnavailable)
		}
		if n := fx.pub.resumed(); n != 0 {
			t.Fatalf("%d resume(s) published past the refusal", n)
		}
		assertNothingMetered(t, fx.counter)
	})

	t.Run("local mode on a fork run's resume subject", func(t *testing.T) {
		run := &store.Run{ID: "run-fork", BotID: "review-pr", ProjectPath: "acme/widgets",
			Trust: store.RunTrustFork, ForkAuthorSubject: string(forkResumeSubject)}
		adm, d := (&Server{}).gateLaunch(context.Background(), (&Server{}).resumeLaunchSubject(run))
		if d == nil {
			adm.rollback(nil)
			t.Fatal("local mode admitted a fork run's resume — it meters nothing, so an untrusted subject is refused")
		}
		if d.reason != denyForkMeteringUnavailable || d.status != http.StatusServiceUnavailable {
			t.Fatalf("denial = %d %q, want 503 %q", d.status, d.reason, denyForkMeteringUnavailable)
		}
	})
}
