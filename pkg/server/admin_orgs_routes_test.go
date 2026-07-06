package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func newOrgTestServer(t *testing.T) *Server {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	s.authSvc = svc
	return s
}

func superAdminCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "admin", IsSuperAdmin: true})
}

func orgReq(ctx context.Context, method, path, body, id string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	if id != "" {
		r.SetPathValue("id", id)
	}
	return r
}

func seedTeam(t *testing.T, s *Server, id, slug string) {
	t.Helper()
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: id, Name: id, Slug: slug, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
}

func seedOrg(t *testing.T, s *Server, id, slug string) {
	t.Helper()
	if _, err := s.authStore().CreateOrg(context.Background(), identity.Org{
		ID: id, Name: id, Slug: slug, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
}

// TestHandleResumeRun_SuspendGate asserts the resume HTTP path enforces
// the same suspend gate as launch: a member of a suspended/read-only org
// is denied (403) before any engine re-entry. Regression guard for the
// resume-bypass blocker (handleLaunchRun had the gate; handleResumeRun
// did not). The gate returns before s.runs is touched, so a nil run
// service in the test harness is fine.
func TestHandleResumeRun_SuspendGate(t *testing.T) {
	s := newOrgTestServer(t)
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "t1", Name: "t1", Slug: "acme", Status: identity.TeamStatusSuspended, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed suspended team: %v", err)
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1"})
	r := orgReq(ctx, "POST", "/api/runs/run-123/resume", `{}`, "run-123")
	w := httptest.NewRecorder()
	s.handleResumeRun(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("suspended-org resume: code=%d body=%s want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot launch") {
		t.Fatalf("expected suspend-gate message, got %s", w.Body.String())
	}
}

// firstWSError drains one envelope from a runConn's sendCh (handleAnswer
// sends synchronously into the buffered channel, so it is already there
// when the handler returns) and decodes it as an error payload.
func firstWSError(t *testing.T, c *runConn) wsErrorPayload {
	t.Helper()
	select {
	case data := <-c.sendCh:
		var env runWSEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.Type != wsTypeError {
			t.Fatalf("envelope type = %q, want error", env.Type)
		}
		var p wsErrorPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("decode error payload: %v", err)
		}
		return p
	default:
		t.Fatal("no envelope sent")
		return wsErrorPayload{}
	}
}

// TestRunsWS_HandleAnswer_SuspendGate is the WebSocket counterpart of
// TestHandleResumeRun_SuspendGate. Answering a paused_waiting_human run
// over the WS re-enters the engine via runs.Resume, and it is the ONLY
// way to resume that status (there is no HTTP answer endpoint), so it
// must enforce the same org-suspend gate as the HTTP resume path. The
// gate returns before c.server.runs is touched, so a nil run service in
// the harness is fine; an allowed caller falls through to "no_answers".
func TestRunsWS_HandleAnswer_SuspendGate(t *testing.T) {
	s := newOrgTestServer(t)
	for id, status := range map[string]identity.TeamStatus{
		"susp": identity.TeamStatusSuspended,
		"act":  identity.TeamStatusActive,
	} {
		if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
			ID: id, Name: id, Slug: id, Status: status, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed team %s: %v", id, err)
		}
	}
	newConn := func(idn auth.Identity) *runConn {
		return &runConn{
			server:   s,
			runID:    "run-123",
			sendCh:   make(chan []byte, 8),
			closed:   make(chan struct{}),
			tenantID: idn.TeamID,
			userID:   idn.UserID,
			identity: idn,
		}
	}
	// Empty answers: an allowed caller passes the gate and the handler
	// then reports "no_answers" — proof the gate did NOT fire and that
	// runs.Resume (nil here) was never reached.
	emptyAnswers := runWSEnvelope{Type: wsTypeAnswer, AckID: "a1", Payload: json.RawMessage(`{"answers":{}}`)}

	t.Run("suspended org denied", func(t *testing.T) {
		c := newConn(auth.Identity{UserID: "u1", TeamID: "susp"})
		c.handleAnswer(emptyAnswers)
		if got := firstWSError(t, c).Code; got != "org_suspended" {
			t.Fatalf("error code = %q, want org_suspended", got)
		}
	})
	t.Run("active org passes gate", func(t *testing.T) {
		c := newConn(auth.Identity{UserID: "u1", TeamID: "act"})
		c.handleAnswer(emptyAnswers)
		if got := firstWSError(t, c).Code; got == "org_suspended" {
			t.Fatalf("active org must not be suspend-gated; got %q", got)
		}
	})
	t.Run("super-admin bypasses suspended org", func(t *testing.T) {
		c := newConn(auth.Identity{UserID: "admin", TeamID: "susp", IsSuperAdmin: true})
		c.handleAnswer(emptyAnswers)
		if got := firstWSError(t, c).Code; got == "org_suspended" {
			t.Fatalf("super-admin must bypass suspend gate; got %q", got)
		}
	})
}

func TestHandleAdminSetOrgStatus(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := superAdminCtx()
	seedOrg(t, s, "t1", "acme")

	// invalid status → 400
	w := httptest.NewRecorder()
	s.handleAdminSetOrgStatus(w, orgReq(ctx, "POST", "/api/admin/orgs/t1/status", `{"status":"weird"}`, "t1"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: code=%d body=%s", w.Code, w.Body.String())
	}

	// suspend → 200, persisted on the org
	w = httptest.NewRecorder()
	s.handleAdminSetOrgStatus(w, orgReq(ctx, "POST", "/api/admin/orgs/t1/status", `{"status":"suspended","reason":"abuse"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("suspend: code=%d body=%s", w.Code, w.Body.String())
	}
	o, _ := s.authStore().GetOrg(ctx, "t1")
	if !o.Suspended() || o.SuspendReason != "abuse" || o.SuspendedBy != "admin" {
		t.Fatalf("suspend not persisted: %+v", o)
	}

	// restore → active, suspend metadata cleared
	w = httptest.NewRecorder()
	s.handleAdminSetOrgStatus(w, orgReq(ctx, "POST", "/api/admin/orgs/t1/status", `{"status":"active"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("restore: code=%d", w.Code)
	}
	o, _ = s.authStore().GetOrg(ctx, "t1")
	if o.Suspended() || o.SuspendedAt != nil || o.SuspendReason != "" {
		t.Fatalf("restore did not clear suspend metadata: %+v", o)
	}
}

func TestHandleAdminUpdateOrg_QuotaValidation(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := superAdminCtx()
	seedOrg(t, s, "t1", "acme")

	// negative quota → 400
	w := httptest.NewRecorder()
	s.handleAdminUpdateOrg(w, orgReq(ctx, "PATCH", "/api/admin/orgs/t1", `{"memory_quota_bytes":-5}`, "t1"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative quota: code=%d", w.Code)
	}

	// valid quotas → 200, persisted on the org
	w = httptest.NewRecorder()
	s.handleAdminUpdateOrg(w, orgReq(ctx, "PATCH", "/api/admin/orgs/t1", `{"memory_quota_bytes":1048576,"monthly_run_quota":100}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", w.Code, w.Body.String())
	}
	o, _ := s.authStore().GetOrg(ctx, "t1")
	if o.MemoryQuotaBytes != 1048576 || o.MonthlyRunQuota != 100 {
		t.Fatalf("quotas not persisted: %+v", o)
	}
}

func TestHandleAdminListOrgs(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := superAdminCtx()
	seedOrg(t, s, "t1", "a")
	seedOrg(t, s, "t2", "b")

	w := httptest.NewRecorder()
	s.handleAdminListOrgs(w, orgReq(ctx, "GET", "/api/admin/orgs", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d", w.Code)
	}
	var resp struct {
		Orgs []orgView `json:"orgs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Orgs) != 2 {
		t.Fatalf("got %d orgs want 2", len(resp.Orgs))
	}
	if resp.Orgs[0].Status != "active" {
		t.Fatalf("empty status should render as active, got %q", resp.Orgs[0].Status)
	}
}
