package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// statusRefusingForge mints every token (statuses included) and refuses the
// commit-status write with GitHub's 403 — the shape of a permission revoked
// on the installation after the token was minted with it, or a repository
// the installation lost.
func statusRefusingForge(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			atomic.AddInt32(&mints, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_with_statuses", "expires_at": "2099-01-01T00:00:00Z"})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/statuses/"):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &mints
}

// PreflightFor's denied set was written only at mint time: a token minted
// WITH statuses that later gets a 403 on SetCommitStatus left it untouched
// for the token's lifetime, so every subsequent preflight answered "fine" and
// the caller never switched to the binding it could have used. The 403 now
// records the denial, keyed on the permission the write needed.
func TestAppClientRecordsAStatusWriteRefusalForTheNextPreflight(t *testing.T) {
	srv, mints := statusRefusingForge(t)
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99}
	ctx := context.Background()
	if err := a.PreflightFor(ctx, PermissionStatuses); err != nil {
		t.Fatalf("a token minted with statuses preflights clean: %v", err)
	}
	err := a.SetCommitStatus(ctx, "o/r", "deadbeef", forge.CommitStatus{State: forge.CommitStateSuccess, Context: "revi/review"})
	if !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("SetCommitStatus = %v, want the forge's 403 surfaced", err)
	}
	var pe *forge.PermissionError
	if !errors.As(err, &pe) || len(pe.Missing) != 1 || pe.Missing[0] != "statuses:write" {
		t.Errorf("the refusal must name the grant the write needed, got %v", err)
	}
	if err := a.PreflightFor(ctx, PermissionStatuses); !errors.Is(err, forge.ErrPermissionsNotGranted) {
		t.Fatalf("PreflightFor(statuses) after the 403 = %v, want ErrPermissionsNotGranted so the caller takes its fallback", err)
	}
	// Recording a denial is bookkeeping on the cached token, never a re-mint.
	if n := atomic.LoadInt32(mints); n != 1 {
		t.Errorf("mints = %d, want 1: a refusal is noted on the token it happened to, not minted away", n)
	}
	// Unrelated permissions are untouched by a statuses refusal.
	if err := a.PreflightFor(ctx, "contents"); err != nil {
		t.Errorf("PreflightFor(contents) = %v, want nil: the 403 was about statuses", err)
	}
}
