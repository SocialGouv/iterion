package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

var _ forge.ReviewRequestWithdrawer = (*AdminClient)(nil)
var _ forge.ReviewRequestWithdrawer = (*AppClient)(nil)

// withdrawFixture serves GET/DELETE .../requested_reviewers and records what
// the client did: how many of each call, and the reviewers each DELETE named.
type withdrawFixture struct {
	t       *testing.T
	srv     *httptest.Server
	gets    int
	deletes int
	deleted [][]string
	pending []string
	teams   []string
	getCode int
	delCode int
	errBody string
}

func newWithdrawFixture(t *testing.T, pending ...string) *withdrawFixture {
	t.Helper()
	f := &withdrawFixture{t: t, pending: pending}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/42/requested_reviewers", func(w http.ResponseWriter, _ *http.Request) {
		f.gets++
		if f.getCode != 0 && f.getCode/100 != 2 {
			w.WriteHeader(f.getCode)
			_, _ = w.Write([]byte(f.errBody))
			return
		}
		users := make([]map[string]any, 0, len(f.pending))
		for _, l := range f.pending {
			users = append(users, map[string]any{"login": l})
		}
		teams := make([]map[string]any, 0, len(f.teams))
		for _, s := range f.teams {
			teams = append(teams, map[string]any{"slug": s})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": users, "teams": teams})
	})
	mux.HandleFunc("DELETE /repos/o/r/pulls/42/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		f.deletes++
		var body struct {
			Reviewers []string `json:"reviewers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("DELETE body undecodable: %v", err)
		}
		f.deleted = append(f.deleted, body.Reviewers)
		if f.delCode != 0 && f.delCode/100 != 2 {
			w.WriteHeader(f.delCode)
			_, _ = w.Write([]byte(f.errBody))
			return
		}
		// GitHub answers the removal with the PR object; the client discards it.
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *withdrawFixture) client() *AdminClient {
	return &AdminClient{HTTP: f.srv.Client(), APIBase: f.srv.URL, Token: "t"}
}

// The load-bearing case: none of the armed logins is pending, so NOTHING is
// written. GitHub 422s a removal naming an account that is not currently
// requested — a blind DELETE would turn every second publish on the same PR
// into a logged failure.
func TestWithdrawPullReviewRequests_NothingPendingIssuesNoDelete(t *testing.T) {
	f := newWithdrawFixture(t, "a-human")
	got, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("withdrew %v, want nothing", got)
	}
	if f.gets != 1 || f.deletes != 0 {
		t.Fatalf("gets=%d deletes=%d, want 1 read and no write", f.gets, f.deletes)
	}
}

// Empty logins (the lane not armed on this webhook) must not even read.
func TestWithdrawPullReviewRequests_NoLoginsIsNoRoundTrip(t *testing.T) {
	f := newWithdrawFixture(t, "iterion-bot")
	for _, logins := range [][]string{nil, {}, {"", "   "}} {
		got, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, logins)
		if err != nil || len(got) != 0 {
			t.Fatalf("logins=%v: got=%v err=%v", logins, got, err)
		}
	}
	if f.gets != 0 || f.deletes != 0 {
		t.Fatalf("gets=%d deletes=%d, want no round-trip at all", f.gets, f.deletes)
	}
}

// Additive-safe: only the intersection is withdrawn, the humans stay, and the
// match is case-insensitive while the payload echoes the forge's own casing.
func TestWithdrawPullReviewRequests_WithdrawsOnlyTheIntersection(t *testing.T) {
	f := newWithdrawFixture(t, "A-Human", "Iterion-Bot", "revu-bot")
	f.teams = []string{"platform"}
	got, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42,
		[]string{"iterion-BOT", " revu-bot ", "never-requested"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Iterion-Bot", "revu-bot"}
	if !slices.Equal(got, want) {
		t.Fatalf("withdrew %v, want %v (the forge's own casing)", got, want)
	}
	if f.deletes != 1 {
		t.Fatalf("deletes=%d, want exactly 1", f.deletes)
	}
	if !slices.Equal(f.deleted[0], want) {
		t.Fatalf("DELETE named %v, want %v — a human reviewer must never be dropped", f.deleted[0], want)
	}
}

// A team request is somebody else's decision: the `teams` half of the pending
// set is read and left alone, never folded into the reviewers payload.
func TestWithdrawPullReviewRequests_TeamsAreLeftAlone(t *testing.T) {
	f := newWithdrawFixture(t)
	f.teams = []string{"iterion-bot", "platform"}
	got, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || f.deletes != 0 {
		t.Fatalf("a team sharing the login's name must not be withdrawn: got=%v deletes=%d", got, f.deletes)
	}
}

// Falsifiable #4 reads this in the log: a connection short of the grant gets a
// typed refusal NAMING pull_requests:write, not an opaque 403.
func TestWithdrawPullReviewRequests_MissingGrantIsTyped(t *testing.T) {
	f := newWithdrawFixture(t, "iterion-bot")
	f.delCode = http.StatusForbidden
	f.errBody = `{"message":"Resource not accessible by integration"}`
	_, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err == nil {
		t.Fatal("a 403 on the removal must surface")
	}
	var pe *forge.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want *forge.PermissionError", err, err)
	}
	if !slices.Contains(pe.Missing, "pull_requests:write") {
		t.Fatalf("missing grants = %v, want pull_requests:write named", pe.Missing)
	}
	if !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("err must keep its forbidden identity: %v", err)
	}
}

// The READ half names its OWN grant: a read-only fine-grained PAT is refused
// on the pending-set fetch, and the remedy must say pull_requests:read rather
// than send the operator after a write grant the call never needed.
func TestWithdrawPullReviewRequests_ReadRefusalNamesTheReadGrant(t *testing.T) {
	f := newWithdrawFixture(t, "iterion-bot")
	f.getCode = http.StatusForbidden
	f.errBody = `{"message":"Resource not accessible by personal access token"}`
	_, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	var pe *forge.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want *forge.PermissionError", err, err)
	}
	if !slices.Equal(pe.Missing, []string{"pull_requests:read"}) {
		t.Fatalf("missing grants = %v, want exactly [pull_requests:read] — the read half needs no write", pe.Missing)
	}
	if f.deletes != 0 {
		t.Fatalf("deletes=%d, want 0 — the pending set is unknown", f.deletes)
	}
}

// A failed READ surfaces and writes NOTHING — never a DELETE on an unknown
// pending set, which is exactly how a reviewer iterion was not named gets
// dropped. An unreadable set is reported, NOT read as "nothing was pending":
// the caller's Warn then says the request is still standing.
func TestWithdrawPullReviewRequests_ReadFailureIssuesNoDelete(t *testing.T) {
	f := newWithdrawFixture(t, "iterion-bot")
	f.getCode = http.StatusInternalServerError
	f.errBody = `{"message":"boom"}`
	_, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err == nil {
		t.Fatal("a failed pending-set read must surface")
	}
	if f.deletes != 0 {
		t.Fatalf("deletes=%d, want 0 — the pending set is unknown", f.deletes)
	}
}

// `{"users": null}` — the shape a forge (or a proxy) emits for an empty
// pending set. A nil slice must read as "nothing pending", not panic and not
// write.
func TestWithdrawPullReviewRequests_NullUsersIsAnEmptyPendingSet(t *testing.T) {
	mux := http.NewServeMux()
	deletes := 0
	mux.HandleFunc("GET /repos/o/r/pulls/42/requested_reviewers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"users":null,"teams":null}`))
	})
	mux.HandleFunc("DELETE /repos/o/r/pulls/42/requested_reviewers", func(http.ResponseWriter, *http.Request) { deletes++ })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	got, err := c.WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err != nil || len(got) != 0 || deletes != 0 {
		t.Fatalf("got=%v err=%v deletes=%d, want an empty no-op", got, err, deletes)
	}
}

// A 404 (GitHub's answer for a resource a credential may not see) stays typed
// as a not-found carrying the needed grant, not as a success.
func TestWithdrawPullReviewRequests_DeleteNotFoundSurfaces(t *testing.T) {
	f := newWithdrawFixture(t, "iterion-bot")
	f.delCode = http.StatusNotFound
	_, err := f.client().WithdrawPullReviewRequests(context.Background(), "o/r", 42, []string{"iterion-bot"})
	if err == nil || !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want a forge not-found", err)
	}
}
