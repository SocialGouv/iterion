package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

func prStateReq(token, prURL string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/forge/pull-request?pr_url="+prURL, nil)
	if token != "" {
		r.Header.Set("X-Iterion-Run", token)
	}
	return r
}

// A run's delivery tail has to know whether the pull request it is about to
// push onto is still open, and it must learn it WITHOUT holding a forge
// credential — the whole point of the publish grant. This is the read half of
// that grant: same token, same repo scope, no write.
func TestForgePullRequestState(t *testing.T) {
	cases := []struct {
		name      string
		state     string
		wantOpen  bool
		wantState string
	}{
		{"open", "open", true, "open"},
		{"merged", "merged", false, "merged"},
		{"closed", "closed", false, "closed"},
		// A provider that reports no state says nothing: the tail must not
		// read silence as a closure and strand its work.
		{"unreported", "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			gc := &fakeGateClient{headSHA: "deadbeefcafe", state: tc.state}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

			w := httptest.NewRecorder()
			s.handleForgePullRequest(w, prStateReq("tok1", "https://github.com/o/r/pull/42"))
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var resp forgePullRequestResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Open != tc.wantOpen || resp.State != tc.wantState {
				t.Fatalf("open=%v state=%q, want open=%v state=%q", resp.Open, resp.State, tc.wantOpen, tc.wantState)
			}
			if resp.HeadSHA != "deadbeefcafe" {
				t.Fatalf("the tail compares its own work against this head: %+v", resp)
			}
		})
	}
}

// The grant's repo is the whole authority here, exactly as on the write half:
// a token must not be able to read a pull request outside it.
func TestForgePullRequestState_ScopedToTheGrantsRepo(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	w := httptest.NewRecorder()
	s.handleForgePullRequest(w, prStateReq("tok1", "https://github.com/other/repo/pull/1"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a grant scoped to o/r must not read other/repo: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestForgePullRequestState_RejectsAnUnknownToken(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	w := httptest.NewRecorder()
	s.handleForgePullRequest(w, prStateReq("nope", "https://github.com/o/r/pull/42"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown run token must 401, got %d", w.Code)
	}
}
