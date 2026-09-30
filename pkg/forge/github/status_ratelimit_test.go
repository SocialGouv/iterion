package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// statusRateLimitedForge mints every token (statuses included) and answers
// both commit-status calls — the write, and the read of the combined status —
// with GitHub's primary rate limit: a 403 whose budget is spent.
func statusRateLimitedForge(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var mints int32
	reset := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			atomic.AddInt32(&mints, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_with_statuses", "expires_at": "2099-01-01T00:00:00Z"})
		case strings.Contains(r.URL.Path, "/statuses/"), strings.HasSuffix(r.URL.Path, "/status"):
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", reset)
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded for installation ID 99."})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &mints
}

// A rate-limited status call is a wait, not a refusal. Read as a 403 it used
// to note the statuses grant as withheld, and every PreflightFor(statuses)
// answered "the installation withholds statuses" for the rest of the cached
// token's life — the gate took its fallback for an hour on a limit.
func TestAppClientStatusCallsReadARateLimitAsAWait(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(*AppClient) error{
		"SetCommitStatus": func(a *AppClient) error {
			return a.SetCommitStatus(ctx, "o/r", "deadbeef", forge.CommitStatus{State: forge.CommitStateSuccess, Context: "revi/review"})
		},
		"ListCommitStatuses": func(a *AppClient) error {
			_, err := a.ListCommitStatuses(ctx, "o/r", "deadbeef")
			return err
		},
	}
	for name, call := range calls {
		srv, mints := statusRateLimitedForge(t)
		a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99}
		err := call(a)
		var se *forge.StatusError
		if errors.Is(err, forge.ErrForbidden) || !errors.As(err, &se) || !se.RateLimited() {
			t.Errorf("%s: err = %v, want a rate-limited *forge.StatusError", name, err)
			continue
		}
		if err := a.PreflightFor(ctx, PermissionStatuses); err != nil {
			t.Errorf("%s: PreflightFor(statuses) after a rate limit = %v, want nil — a wait is not a withheld grant", name, err)
		}
		if n := atomic.LoadInt32(mints); n != 1 {
			t.Errorf("%s: mints = %d, want 1", name, n)
		}
	}
}

// The management-token path shares the transport: its status write is typed
// the same way.
func TestAdminClientStatusWriteReadsARateLimitAsAWait(t *testing.T) {
	srv, _ := statusRateLimitedForge(t)
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	err := c.SetCommitStatus(context.Background(), "o/r", "deadbeef", forge.CommitStatus{State: forge.CommitStateSuccess, Context: "revi/review"})
	var se *forge.StatusError
	if errors.Is(err, forge.ErrForbidden) || !errors.As(err, &se) || !se.RateLimited() {
		t.Fatalf("err = %v, want a rate-limited *forge.StatusError", err)
	}
	if wait := time.Until(se.ResetAt); wait < 29*time.Minute || wait > 30*time.Minute {
		t.Errorf("ResetAt in %v, want ~30m — GitHub said when the budget resets", wait)
	}
}
