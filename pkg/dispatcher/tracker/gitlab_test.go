package tracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// fakeGitLab serves canned responses for the adapter tests. It asserts
// the project path is %2F-encoded ("owner%2Frepo") and the PRIVATE-TOKEN
// header is sent on every request.
func newFakeGitLab(t *testing.T) (*httptest.Server, *map[string]int) {
	t.Helper()
	calls := map[string]int{}
	const proj = "/api/v4/projects/owner%2Frepo"
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/issues", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "secret" {
			t.Errorf("PRIVATE-TOKEN: want secret, got %q", got)
		}
		calls[r.Method+" issues"]++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"iid":         1,
				"title":       "ready one",
				"description": "body",
				"state":       "opened",
				"labels":      []string{"ready"},
				"author":      map[string]string{"username": "alice"},
				"created_at":  "2026-05-01T00:00:00Z",
				"updated_at":  "2026-05-01T00:00:00Z",
				"web_url":     "http://gitlab.example/owner/repo/-/issues/1",
			},
			{
				"iid":        2,
				"title":      "claimed elsewhere",
				"state":      "opened",
				"labels":     []string{"ready", "iterion-claimed"},
				"created_at": "2026-05-01T00:00:00Z",
				"updated_at": "2026-05-01T00:00:00Z",
			},
			{
				"iid":        3,
				"title":      "unmatched",
				"state":      "opened",
				"labels":     []string{"junk"},
				"created_at": "2026-05-01T00:00:00Z",
				"updated_at": "2026-05-01T00:00:00Z",
			},
		})
	})
	mux.HandleFunc(proj+"/issues/1", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "secret" {
			t.Errorf("PRIVATE-TOKEN: want secret, got %q", got)
		}
		calls[r.Method+" issue1"]++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid":        1,
			"title":      "ready one",
			"state":      "opened",
			"labels":     []string{"ready"},
			"created_at": "2026-05-01T00:00:00Z",
			"updated_at": "2026-05-01T00:00:00Z",
		})
	})
	mux.HandleFunc(proj+"/issues/1/notes", func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" notes1"]++
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc(proj+"/issues/999", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newGitLab(t *testing.T, host string) *tracker.GitLabAdapter {
	t.Helper()
	a, err := tracker.NewGitLab(tracker.GitLabOptions{
		Host:         host,
		Repo:         "owner/repo",
		Token:        "secret",
		ClaimedLabel: "iterion-claimed",
		StateMapping: map[string]tracker.LabelSelector{
			"ready": {LabelsInclude: []string{"ready"}, LabelsExclude: []string{"iterion-claimed"}},
		},
	})
	if err != nil {
		t.Fatalf("NewGitLab: %v", err)
	}
	return a
}

func TestGitLabListCandidates(t *testing.T) {
	srv, _ := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].ID, "#1") {
		t.Fatalf("want 1 candidate (#1), got %+v", got)
	}
	if got[0].WorkflowState != "ready" {
		t.Fatalf("state: %s", got[0].WorkflowState)
	}
	if got[0].Metadata["author"] != "alice" {
		t.Fatalf("author: %q", got[0].Metadata["author"])
	}
}

func TestGitLabRefreshStates(t *testing.T) {
	srv, _ := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	got, err := a.RefreshStates(context.Background(), []string{id, "gitlab:bogus/x#1"})
	if err != nil {
		t.Fatalf("RefreshStates: %v", err)
	}
	if got[id] != "ready" {
		t.Fatalf("want ready, got %q", got[id])
	}
}

func TestGitLabUpdateState(t *testing.T) {
	srv, calls := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	if err := a.UpdateState(context.Background(), id, "ready"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	// Read-then-PUT: one GET + one PUT on issue1.
	if (*calls)["GET issue1"] != 1 {
		t.Fatalf("expected 1 GET issue1, got %d", (*calls)["GET issue1"])
	}
	if (*calls)["PUT issue1"] != 1 {
		t.Fatalf("expected 1 PUT issue1, got %d", (*calls)["PUT issue1"])
	}
}

func TestGitLabUpdateStateRejected(t *testing.T) {
	srv, _ := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	err := a.UpdateState(context.Background(), id, "noplace")
	if !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("want ErrTransitionRejected, got %v", err)
	}
}

func TestGitLabComment(t *testing.T) {
	srv, calls := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	if err := a.Comment(context.Background(), id, "hi"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if (*calls)["POST notes1"] != 1 {
		t.Fatalf("expected 1 note call, got %d", (*calls)["POST notes1"])
	}
}

func TestGitLabClaimAndRelease(t *testing.T) {
	srv, calls := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	if err := a.Claim(context.Background(), id, "h-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := a.Release(context.Background(), id, "h-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Claim + Release each issue a single PUT /issues/1 (add_labels /
	// remove_labels) — no label-ID resolution round trips.
	if (*calls)["PUT issue1"] != 2 {
		t.Fatalf("expected 2 PUT issue1 (claim+release), got %d", (*calls)["PUT issue1"])
	}
}

func TestGitLabNotFound(t *testing.T) {
	srv, _ := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	id := fmt.Sprintf("gitlab:%s/owner/repo#999", stripScheme(srv.URL))
	if err := a.Claim(context.Background(), id, "h"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Release folds 404 into success (idempotent).
	if err := a.Release(context.Background(), id, "h"); err != nil {
		t.Fatalf("Release on missing issue: want nil, got %v", err)
	}
}

func TestGitLabParseIDRoundTrip(t *testing.T) {
	srv, _ := newFakeGitLab(t)
	a := newGitLab(t, srv.URL)
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	id := got[0].ID
	want := fmt.Sprintf("gitlab:%s/owner/repo#1", stripScheme(srv.URL))
	if id != want {
		t.Fatalf("ID: want %q, got %q", want, id)
	}
}

// Compile-time assertion.
var _ tracker.Tracker = (*tracker.GitLabAdapter)(nil)
