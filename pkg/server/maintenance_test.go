package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

func armTestFence(t *testing.T, s *Server) *platformcfg.MemoryStore[platformcfg.Maintenance] {
	t.Helper()
	st := platformcfg.NewMemoryStore[platformcfg.Maintenance]()
	if err := st.Put(context.Background(), platformcfg.Maintenance{FenceLaunches: true, Reason: "schema rollout"}); err != nil {
		t.Fatal(err)
	}
	s.maintenanceStore = st
	return st
}

func TestFenceHTTPLaunch(t *testing.T) {
	s, counter, ctx, rs := newQuotaReleaseServer(t)
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(&landingPublishStub{}))
	body, err := json.Marshal(map[string]any{"source": aWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	st := armTestFence(t, s)
	req := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleLaunchRun(w, httptest.NewRequest("POST", "/api/runs", strings.NewReader(string(body))).WithContext(ctx))
		return w
	}
	rec, err := st.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := req()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), denyMaintenanceFence) || !strings.Contains(w.Body.String(), "schema rollout") || !strings.Contains(w.Body.String(), rec.UpdatedAt.Format(time.RFC3339Nano)) {
		t.Fatalf("armed: %d %s", w.Code, w.Body.String())
	}
	if meteredRuns(t, counter) != 0 {
		t.Fatal("fenced launch was metered")
	}
	if err := st.Put(ctx, platformcfg.Maintenance{Reason: "rollout done"}); err != nil {
		t.Fatal(err)
	}
	w = req()
	// The same valid request is accepted immediately after disarming.
	if w.Code != http.StatusAccepted || strings.Contains(w.Body.String(), denyMaintenanceFence) {
		t.Fatalf("disarmed: %d %s", w.Code, w.Body.String())
	}
	armTestFence(t, s)
	if _, d := s.gateLaunch(superAdminCtx(), launchSubject{}); d != nil {
		t.Fatalf("break glass: %+v", d)
	}
}

func TestFenceBoardAndTrigger(t *testing.T) {
	t.Run("board", func(t *testing.T) {
		pub := &countingPublisher{}
		s, _ := newGatedBoardServer(t, gateSpec{id: "t1"}, pub)
		armTestFence(t, s)
		err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{ID: "native:1", Bot: "probe", State: native.StateReady})
		var denial *launchDeniedError
		if !errors.As(err, &denial) || denial.Reason != denyMaintenanceFence || pub.count() != 0 {
			t.Fatalf("board: %v, publishes %d", err, pub.count())
		}
	})
	t.Run("trigger", func(t *testing.T) {
		pub := &spinePublisher{}
		s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
		armTestFence(t, s)
		_, err := s.triggerLauncher().Launch(spineCtx(t), spinePlan("t1"))
		var denial *launchDeniedError
		if !errors.As(err, &denial) || denial.Reason != denyMaintenanceFence || pub.count() != 0 {
			t.Fatalf("trigger: %v, publishes %d", err, pub.count())
		}
	})
}

func TestFenceResumeExistingRun(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t)
	installQueueOutageTestPublisher(t, s, nil)
	const source = "workflow fence_resume:\n  entry: done\n"
	path := filepath.Join(s.cfg.WorkDir, "fence_resume.bot")
	_, hash, err := runview.CompileWorkflowFromSource(path, source)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(s.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "before-fence"
	run, err := st.CreateRun(context.Background(), runID, "fence_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.FilePath, run.WorkflowSource, run.WorkflowHash = path, source, hash
	run.Status = store.RunStatusPausedOperator
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheckpoint(context.Background(), runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	armTestFence(t, s)
	req := httptest.NewRequest("POST", "/api/runs/"+runID+"/resume", strings.NewReader(`{"source":"workflow fence_resume:\n  entry: done\n"}`))
	req.SetPathValue("id", runID)
	w := httptest.NewRecorder()
	s.handleResumeRun(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume under fence: %d %s", w.Code, w.Body.String())
	}
}

type brokenMaintenanceStore struct{}

func (brokenMaintenanceStore) Get(context.Context) (*platformcfg.Maintenance, error) {
	return nil, errors.New("offline")
}
func (brokenMaintenanceStore) Put(context.Context, platformcfg.Maintenance) error {
	return errors.New("offline")
}

func TestFenceReadFailure(t *testing.T) {
	s := &Server{maintenanceStore: brokenMaintenanceStore{}}
	if d := s.gateMaintenance(context.Background(), launchSubject{}); d == nil || d.status != 503 {
		t.Fatalf("read failure: %+v", d)
	}
	if d := s.gateMaintenance(context.Background(), launchSubject{ExistingRun: true}); d != nil {
		t.Fatalf("existing run blocked: %+v", d)
	}
}

func TestFenceSettingsAuthorizationAndDate(t *testing.T) {
	s := newOrgTestServer(t)
	s.maintenanceStore = platformcfg.NewMemoryStore[platformcfg.Maintenance]()
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(make([]byte, 32)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.signer = signer
	adminToken, _, err := signer.IssueAccess(auth.Identity{UserID: "admin", IsSuperAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	adminReq := func(body string) *http.Request {
		req := httptest.NewRequest("PUT", "/api/admin/settings/maintenance", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		return req
	}
	s.registerAdminSettingsFamilyRoutes()
	memberToken, _, err := signer.IssueAccess(auth.Identity{UserID: "member", TeamID: "team"})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT"} {
		req := httptest.NewRequest(method, "/api/admin/settings/maintenance", strings.NewReader(`{"fence_launches":false,"reason":"bypass"}`))
		req.Header.Set("Authorization", "Bearer "+memberToken)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("member %s = %d %s", method, w.Code, w.Body.String())
		}
	}

	for _, method := range []string{"GET", "PUT"} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(method, "/api/admin/settings/maintenance", strings.NewReader(`{"fence_launches":true,"reason":"rollout"}`)))
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("unprivileged %s = %d", method, w.Code)
		}
	}
	for _, body := range []string{`{}`, `{"fence_launches":true}`, `{"fence_launches":"true","reason":"rollout"}`, `{"fence_launches":null,"reason":"rollout"}`} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, adminReq(body))
		if w.Code != 400 {
			t.Fatalf("invalid %s = %d", body, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, adminReq(`{"fence_launches":true,"reason":"rollout"}`))
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	rec, err := s.maintenanceStore.Get(context.Background())
	if err != nil || rec == nil || rec.UpdatedAt.IsZero() || rec.UpdatedBy != "admin" || !rec.FenceLaunches {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

func TestFenceFork(t *testing.T) {
	s := newOrgTestServer(t)
	armTestFence(t, s)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/runs/parent/fork", strings.NewReader(`{"node_id":"node"}`))
	req.SetPathValue("id", "parent")
	s.handleForkRun(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), denyMaintenanceFence) {
		t.Fatalf("fork: %d %s", w.Code, w.Body.String())
	}
}

// A durable claimed retry is an existing run even when its document read fails.
type fenceUnreadableRetry struct{ *fakeRetryStore }

func (f fenceUnreadableRetry) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, errors.New("transient run read")
}

func TestFenceRetryExistingRun(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		st := newFakeRetryStore()
		st.claimWins["run-a"] = true
		var rs store.RunStore = st
		if unreadable {
			rs = fenceUnreadableRetry{st}
		}
		resumer := &fakeResumer{}
		s := newRetrySweeperServer(t, rs, resumer)
		armTestFence(t, s)
		at := time.Now().UTC().Add(-time.Minute)
		s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())
		if len(resumer.calls) != 1 || len(st.abandoned) != 0 {
			t.Fatalf("unreadable=%v: resumes=%d abandoned=%v", unreadable, len(resumer.calls), st.abandoned)
		}
	}
}

func TestFenceWebSocketExistingRun(t *testing.T) {
	s, _, ctx, rs := newQuotaReleaseServer(t)
	seedFinishedRun(t, rs, "existing-ws")
	armTestFence(t, s)
	id, _ := auth.FromContext(ctx)
	c := &runConn{server: s, runID: "existing-ws", sendCh: make(chan []byte, 8), closed: make(chan struct{}),
		tenantID: id.TeamID, userID: id.UserID, identity: id}
	payload, err := json.Marshal(map[string]any{"answers": map[string]any{"q": "a"}, "source": notAWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	c.handleAnswer(runWSEnvelope{Type: wsTypeAnswer, AckID: "a", Payload: payload})
	// Reaches the ordinary resume status check instead of the maintenance gate.
	if got := firstWSError(t, c).Code; got != runNotResumableErrorCode {
		t.Fatalf("resume error = %q", got)
	}
}
