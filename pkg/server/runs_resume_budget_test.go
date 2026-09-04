package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// The resume endpoint validates the budget ask at admission — the third
// site of the class (Service.Resume and the remote CLI are the other two).
// Without it a malformed max_duration rides RunMessage.Budget onto the
// queue and fails the runner's applyBudgetOverrides on every redelivery
// until the DLQ park. The validation runs before any store access: the
// harness has no run service at all, so reaching the store would panic.
func TestHandleResumeRun_RejectsMalformedBudgetBeforeStoreAccess(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "t1", "acme")
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1"})
	r := orgReq(ctx, "POST", "/api/runs/run-123/resume", `{"budget":{"max_duration":"4 hours"}}`, "run-123")
	w := httptest.NewRecorder()
	s.handleResumeRun(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed budget on resume: code=%d body=%s, want 400 — the ask would ride the queue and fail on every redelivery", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "max_duration") {
		t.Fatalf("400 body must name the offending field, got %s", w.Body.String())
	}
}
