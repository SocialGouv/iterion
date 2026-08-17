package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The admin client must satisfy the merge-gate commit-status capabilities:
// write (post a verdict) and read (repair without overwriting a real one).
var _ forge.CommitStatusClient = (*AdminClient)(nil)
var _ forge.CommitStatusLister = (*AdminClient)(nil)

func TestSetCommitStatusInterface(t *testing.T) {
	// Compile-time assertions above are the real check; this keeps the file a
	// valid test unit.
}

// GitLab returns every status row on the commit (retries included), so the
// lister must keep only the newest row per name — regardless of the order the
// API returns them in — and map GitLab's wire states onto the normalized
// vocabulary.
func TestListCommitStatuses_DedupAndStateMapping(t *testing.T) {
	mux := http.NewServeMux()
	// Subgroup path: the project id is the URL-escaped full path.
	mux.HandleFunc("GET /api/v4/projects/grp%2Fsub%2Fproj/repository/commits/abc123/statuses",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				// Old failed attempt of the gate, then its newer green retry —
				// newest-first here, oldest-first below, to pin order-independence.
				{"id": 12, "status": "success", "name": "iterion/review", "description": "0 findings", "target_url": "https://x/2"},
				{"id": 7, "status": "failed", "name": "iterion/review", "description": "2 findings", "target_url": "https://x/1"},
				{"id": 3, "status": "running", "name": "ci/build", "description": "", "target_url": ""},
				{"id": 9, "status": "canceled", "name": "ci/lint", "description": "", "target_url": ""},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	sts, err := c.ListCommitStatuses(context.Background(), "grp/sub/proj", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 3 {
		t.Fatalf("want 3 deduplicated statuses, got %d: %+v", len(sts), sts)
	}
	byName := map[string]forge.CommitStatus{}
	for _, s := range sts {
		byName[s.Context] = s
	}
	gate := byName["iterion/review"]
	if gate.State != forge.CommitStateSuccess || gate.Description != "0 findings" || gate.TargetURL != "https://x/2" {
		t.Fatalf("gate must be the newest row (id 12): %+v", gate)
	}
	if byName["ci/build"].State != forge.CommitStatePending {
		t.Fatalf("running must read as pending: %+v", byName["ci/build"])
	}
	if byName["ci/lint"].State != forge.CommitStateError {
		t.Fatalf("canceled must read as error: %+v", byName["ci/lint"])
	}
}

func TestListCommitStatuses_ErrorStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/repository/commits/abc/statuses",
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "403 Forbidden"})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	if _, err := c.ListCommitStatuses(context.Background(), "grp/proj", "abc"); err == nil {
		t.Fatal("want an error on a non-2xx response, got nil")
	}
}

// GitLab refuses pending → pending ("Cannot transition status via :enqueue
// from :pending", HTTP 400) where GitHub takes the same POST as a no-op. The
// merge gate's in-flight claim is the only writer that posts pending, and it
// asks for a state that is already true — so the claim must read as claimed,
// not as a failure that leaves the check reading "absent".
func TestSetCommitStatus_PendingOverPendingIsClaimed(t *testing.T) {
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/statuses/abc", func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "Cannot transition status via :enqueue from :pending (Reason(s): Status cannot transition via \"enqueue\")",
		})
	})
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/repository/commits/abc/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "status": "pending", "name": "iterion/review"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	err := c.SetCommitStatus(context.Background(), "grp/proj", "abc", forge.CommitStatus{
		State: forge.CommitStatePending, Context: "iterion/review",
	})
	if err != nil {
		t.Fatalf("a claim over an existing pending claim must succeed, got %v", err)
	}
	if posts != 1 {
		t.Fatalf("want exactly one POST attempt, got %d", posts)
	}
}

// The narrowing that keeps the case above from swallowing real failures: the
// same 400 with the context sitting on any other state still surfaces, since
// the claim did NOT take and the gate would otherwise read as claimed.
func TestSetCommitStatus_PendingOverOtherStateStillFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/statuses/abc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Cannot transition status via :enqueue from :running"})
	})
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/repository/commits/abc/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "status": "success", "name": "iterion/review"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	if err := c.SetCommitStatus(context.Background(), "grp/proj", "abc", forge.CommitStatus{
		State: forge.CommitStatePending, Context: "iterion/review",
	}); err == nil {
		t.Fatal("want the rejection to surface when the context is not already pending")
	}
}

// A verdict write (the state the gate actually gates on) never takes the
// already-claimed path: a rejected verdict is a real failure.
func TestSetCommitStatus_VerdictRejectionSurfaces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/statuses/abc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Cannot transition status"})
	})
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/repository/commits/abc/statuses", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "status": "pending", "name": "iterion/review"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	if err := c.SetCommitStatus(context.Background(), "grp/proj", "abc", forge.CommitStatus{
		State: forge.CommitStateFailure, Context: "iterion/review",
	}); err == nil {
		t.Fatal("want a rejected verdict to surface")
	}
}
