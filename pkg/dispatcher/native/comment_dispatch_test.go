package native

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAddComment_AutoDispatch verifies the CommentDispatcher hook: a comment
// whose body leads with a "/command" is resolved (here by a test stub mimicking
// the server's resolveBoardComment) into a bot + bot_args + a transition to the
// dispatch-eligible state, so the polling dispatcher then runs it. A comment
// with no command (or that the resolver declines) is recorded with no dispatch.
func TestAddComment_AutoDispatch(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := st.Create(Issue{Title: "Improve retries", Body: "make them exponential"})
	if err != nil {
		t.Fatal(err)
	}
	// Stub resolver — the native twin of pkg/server.resolveBoardComment: a
	// /featurly comment opens an MR back-linking THIS card (source_issue_ref =
	// the card's own prefixed id).
	st.SetCommentDispatcher(func(in Issue, body string) (string, map[string]string, string, bool) {
		if !strings.HasPrefix(strings.TrimSpace(body), "/featurly") {
			return "", nil, "", false
		}
		return "feature-dev", map[string]string{
			"feature_prompt":   "add export endpoint",
			"open_mr":          "true",
			"source_issue_ref": in.ID,
		}, StateReady, true
	})

	mux := http.NewServeMux()
	st.RegisterRoutes(mux, "/api/v1/native")
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues/"+iss.ID+"/comments", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// A /command comment dispatches.
	if w := post(`{"body":"/featurly add export endpoint"}`); w.Code != http.StatusOK {
		t.Fatalf("comment POST code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := st.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bot != "feature-dev" {
		t.Errorf("bot should be stamped from the resolver: %+v", got)
	}
	if got.BotArgs["open_mr"] != "true" || got.BotArgs["source_issue_ref"] != iss.ID {
		t.Errorf("open_mr/source_issue_ref stamp missing or wrong: %+v", got.BotArgs)
	}
	if got.State != StateReady {
		t.Errorf("issue should move to the dispatch-eligible state, got %q", got.State)
	}

	// A plain comment (no command) records but must NOT dispatch / re-stamp.
	iss2, err := st.Create(Issue{Title: "Just a note"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues/"+iss2.ID+"/comments", strings.NewReader(`{"body":"thanks, lgtm"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("plain comment code=%d", w.Code)
	}
	got2, _ := st.Get(iss2.ID)
	if got2.Bot != "" {
		t.Errorf("plain comment must not dispatch a bot: %+v", got2)
	}
	if len(got2.Comments) != 1 {
		t.Errorf("plain comment should still be recorded, got %d comments", len(got2.Comments))
	}
}

// TestAddComment_RetriesAGivenUpCard: /command on a card the dispatcher
// gave up on is the operator's documented way back. Every /command
// resolves to StateReady (resolveBoardComment) and the default give-up
// column is a SINK, so the bare SetState this handler kept — the one
// operator surface the terminal-sink sweep missed — answered 409, AFTER
// the comment and the bot stamp had already persisted: the card was left
// mutated but un-dispatched, and "retry it by commenting" was dead.
func TestAddComment_RetriesAGivenUpCard(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := st.Create(Issue{Title: "gave up", State: StateBlocked})
	if err != nil {
		t.Fatal(err)
	}
	if st.Board().StateByName(StateBlocked) == nil || !st.Board().StateByName(StateBlocked).Terminal {
		t.Skip("this board's give-up column is not a sink — nothing to prove")
	}
	st.SetCommentDispatcher(func(Issue, string) (string, map[string]string, string, bool) {
		return "feature-dev", map[string]string{"feature_prompt": "retry"}, StateReady, true
	})
	mux := http.NewServeMux()
	st.RegisterRoutes(mux, "/api/v1/native")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues/"+iss.ID+"/comments",
		strings.NewReader(`{"body":"/featurly try again"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("comment on a given-up card: code=%d body=%s — the terminal sink refuses the one exit the "+
			"operator has, and the comment + bot stamp already landed", w.Code, w.Body.String())
	}
	got, err := st.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReady {
		t.Fatalf("card state = %q, want %q — the card is mutated but never became dispatchable", got.State, StateReady)
	}
	if got.Bot != "feature-dev" {
		t.Fatalf("bot = %q, want the resolved one", got.Bot)
	}
}
