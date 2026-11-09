package server

import (
	"context"
	"encoding/json"
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

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-162 decision 5's resume half, on the two operator surfaces the round-1
// review found unwitnessed: a fork run's manual resume (POST /runs/{id}/resume
// and its WS answer twin) re-enters the SAME bound the launch entered — the
// run document's TRUST rides the admission subject (so every fail-closed arm
// of the gate applies) and the author's stored metering subject rides it too
// (so the per-contributor twin meters beside the org's). Both halves come
// from the resumeLaunchSubject seam every resume surface crosses; the retry
// sweeper's own witness (webhooks_fork_gesture_test.go) drives the third
// surface through it.

// forkResumePublisher plays the cloud queue for a resumed run: SubmitResume
// succeeds, so the handler's metered unit stays spent — the shape a runner
// claiming the message produces.
type forkResumePublisher struct {
	mu      sync.Mutex
	resumes int
}

func (p *forkResumePublisher) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, *runview.CompiledSource) (runview.LaunchSubmission, error) {
	return runview.LaunchSubmission{}, nil
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
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(&forkResumePublisher{}))
	seedForkResumeRun(t, s, rs, "run-fork", status)
	return s, counter, degrade, rs, ctx
}

// seedForkResumeRun persists a FORK run a resume surface can load and push
// all the way to the publisher: its source compiles, its recorded hash
// matches, and its document carries the trust and the author subject the
// launch's admission stamped (ADR-162 decision 5's durable carriers).
func seedForkResumeRun(t *testing.T, s *Server, rs *store.FilesystemRunStore, runID string, status store.RunStatus) {
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
	r.ForkAuthorSubject = string(forkResumeSubject)
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
// the only surface that resumes a paused_waiting_human run.
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
