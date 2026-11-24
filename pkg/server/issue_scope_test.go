package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The board issues' by-id class (ADR-141): an id-addressed issue is served
// from ITS team — the caller's standing in that team authorizes, the stamp
// lands on the issue's team, and every handler's per-request store
// resolution serves the issue's own board unchanged.

// newIssueScopeServer arms the run-scope identities (org-1, tenant-A,
// tenant-B) with two board stores — one per team — and a resolver that
// asks each store. "amb" is the magic id the resolver reports ambiguous.
func newIssueScopeServer(t *testing.T) (*Server, *native.Store, *native.Store) {
	t.Helper()
	srv, _ := newRunScopeServer(t)
	storeA, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.CloudBoardFor = func(teamID string) native.BoardStore {
		if teamID == "tenant-B" {
			return storeB
		}
		return storeA
	}
	srv.cfg.IssueTenantResolver = func(ctx context.Context, issueID string) (string, error) {
		if issueID == "amb" {
			return "", ErrAmbiguousIssueID
		}
		if _, err := storeA.Resolve(issueID); err == nil {
			return "tenant-A", nil
		}
		if _, err := storeB.Resolve(issueID); err == nil {
			return "tenant-B", nil
		}
		return "", tracker.ErrNotFound
	}
	// The class is the set of REGISTERED routes: the board mount goes up
	// here so every witness's pattern exists for the classification to
	// match, exactly as the cloud mount does.
	api := &native.BoardAPI{Resolve: srv.cloudBoardResolve}
	api.RegisterRoutesWithMiddleware(srv.mux.ServeMux, "/api/v1/native", srv.boardWriteAuth)
	return srv, storeA, storeB
}

func issueIn(t *testing.T, store *native.Store, title string) string {
	t.Helper()
	iss, err := store.Create(native.Issue{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	return iss.ID
}

// A member of the issue's team whose credential is pinned to ANOTHER team
// reads the issue by id: the stamp lands on the issue's team, identity and
// store tenant agree on it.
func TestIssueByIDIsServedFromTheIssuesTeam(t *testing.T) {
	srv, storeA, _ := newIssueScopeServer(t)
	id := issueIn(t, storeA, "team A's card")
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/native/issues/"+id, caller("u-both", "tenant-B"), "")
	if code != http.StatusOK {
		t.Fatalf("member of the issue's team reading by id: %d", code)
	}
	if got.TeamID != "tenant-A" || got.Via != "membership" || tenant != "tenant-A" {
		t.Fatalf("identity = %+v tenant = %q: want the standing in tenant-A (the issue's team)", got, tenant)
	}
}

// No standing in the issue's team: 404 — the same answer a missing issue
// gives (no existence oracle).
func TestIssueByIDInvisibleToAForeignCaller(t *testing.T) {
	srv, storeA, _ := newIssueScopeServer(t)
	id := issueIn(t, storeA, "team A's card")
	code, _, _, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/native/issues/"+id, caller("u-member-b", "tenant-B"), "")
	if code != http.StatusNotFound {
		t.Fatalf("caller without standing: %d, want 404", code)
	}
}

// The ladder: a viewer of the team reads; a mutation takes a member.
func TestIssueByIDLadder(t *testing.T) {
	srv, storeA, _ := newIssueScopeServer(t)
	id := issueIn(t, storeA, "team A's card")
	viewer := caller("u-viewer-a", "tenant-A")
	code, _, _, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/native/issues/"+id, viewer, "")
	if code != http.StatusOK {
		t.Fatalf("viewer read: %d, want 200", code)
	}
	code, _, _, _ = scopedHeader(t, srv, http.MethodPatch, "/api/v1/native/issues/"+id, viewer, "")
	if code != http.StatusForbidden {
		t.Fatalf("viewer mutation: %d, want 403 — a viewer reads, a member changes", code)
	}
}

// A missing issue and an ambiguous prefix take their contracts: 404 for
// the miss; the AMBIENT stamp for the ambiguous prefix (not an address —
// the handler's own resolution renders the verdict for the caller's team).
func TestIssueByIDMissingAndAmbiguous(t *testing.T) {
	srv, _, _ := newIssueScopeServer(t)
	code, _, _, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/native/issues/native:nobody", caller("u-both", "tenant-A"), "")
	if code != http.StatusNotFound {
		t.Fatalf("missing issue: %d, want 404", code)
	}
	code, got, tenant, _ := scopedHeader(t, srv, http.MethodGet, "/api/v1/native/issues/amb", caller("u-both", "tenant-A"), "")
	if code != http.StatusOK || got.TeamID != "tenant-A" || tenant != "tenant-A" {
		t.Fatalf("ambiguous prefix: %d %+v %q — the ambient stamp decides", code, got, tenant)
	}
}

// The full chain a production request walks: the run's-board twin — a
// bearer pinned to B whose holder is a member of A reads team A's issue
// through the REAL BoardAPI mount, served from A's store.
func TestIssueByIDThroughRequireAuth(t *testing.T) {
	srv, storeA, _ := newIssueScopeServer(t)
	id := issueIn(t, storeA, "team A's card")
	ts := fullStack(t, srv)

	pinned, _, err := srv.signer.IssueAccess(caller("u-both", "tenant-B"))
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/native/issues/"+id, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pinned)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("through the full chain: %d — the choke point must re-scope the board routes to the issue's team", resp.StatusCode)
	}
}
