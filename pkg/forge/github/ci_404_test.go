package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// "No CI" is what GitHub answers 200 + an empty list: both
// /commits/{ref}/check-runs and /commits/{ref}/status describe a commit with
// nothing on it that way. A 404 means the ref is absent OR the credential is
// short of the grant — a fine-grained PAT without `checks:read` gets exactly
// that — and reading it as "no CI" renders a green-enough panel on data the
// caller was never allowed to see.
func TestGitHubCI_404IsNotNoCI(t *testing.T) {
	cases := []struct {
		name    string
		miss    string // path suffix answered 404
		wantOp  string
		wantNee string
	}{
		{"check-runs", "/check-runs", "GET check-runs", "checks:read"},
		{"commit status", "/status", "GET commit status", "statuses:read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, tc.miss) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/check-runs") {
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "sha": "sha", "statuses": []any{}})
			}))
			defer srv.Close()

			c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
			st, err := c.GetCIStatus(context.Background(), "o/r", "sha")
			if err == nil {
				t.Fatalf("a 404 on %s must surface, got state=%q runs=%d", tc.miss, st.State, len(st.Runs))
			}
			if !errors.Is(err, forge.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
			var nf *forge.NotFoundError
			if !errors.As(err, &nf) {
				t.Fatalf("err = %v, want *forge.NotFoundError naming the grant", err)
			}
			if nf.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", nf.Op, tc.wantOp)
			}
			if len(nf.MayNeed) != 1 || nf.MayNeed[0] != tc.wantNee {
				t.Errorf("MayNeed = %v, want [%s]", nf.MayNeed, tc.wantNee)
			}
		})
	}
}

// A commit with nothing on it stays the empty, non-error answer it always
// was — the 404 fix must not turn "green repo, no CI configured" into a
// failing panel.
func TestGitHubCI_EmptyIsStillEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "sha": "sha", "statuses": []any{}})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	st, err := c.GetCIStatus(context.Background(), "o/r", "sha")
	if err != nil {
		t.Fatalf("200 + empty is not an error: %v", err)
	}
	if st.State != forge.CIUnknown || len(st.Runs) != 0 {
		t.Errorf("state=%q runs=%d, want unknown/0", st.State, len(st.Runs))
	}
	if _, err := c.ListCIHistory(context.Background(), "o/r", "sha", 5); err != nil {
		t.Fatalf("ListCIHistory on an empty ref: %v", err)
	}
}

// The history read shares fetchCheckRuns, so it inherits the same rule.
func TestGitHubListCIHistory_404Surfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	runs, err := c.ListCIHistory(context.Background(), "o/r", "sha", 5)
	if err == nil {
		t.Fatalf("a 404 must surface, got %d runs", len(runs))
	}
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
