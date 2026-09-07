package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The card's CI panel reads two things: the aggregate state (the verdict a
// merge decision is taken on) and the recent history (context). A failed
// HISTORY read used to be dropped on the floor — `history, _ :=` — so the
// panel showed an empty timeline that reads exactly like "nothing ever ran".
// The status half still answers, so the request is not failed; the failure
// is named in the payload instead.
func TestCardPullPanelSurfacesAFailedHistoryRead(t *testing.T) {
	f := newPullPanelForge(t, map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"repository_hooks": "write", "statuses": "read", "checks": "read",
	})
	// The status read takes the first check-runs call; the history read takes
	// the second, and that one breaks.
	f.failCheckRunsFrom = 2
	s, cardID := pullPanelFixture(t, f)

	w := httptest.NewRecorder()
	s.handleIssuePullCI(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls/7/ci", cardID, "7"))
	if w.Code != http.StatusOK {
		t.Fatalf("the status half answered, so the panel answers: code=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Status       forge.CIStatus `json:"status"`
		History      []forge.CIRun  `json:"history"`
		HistoryError string         `json:"history_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status.State != forge.CISuccess {
		t.Errorf("status = %+v, want the state the forge served", body.Status)
	}
	if body.HistoryError == "" {
		t.Fatal("a failed history read must be named in the response, not swallowed into an empty timeline")
	}
	if !strings.Contains(body.HistoryError, "502") && !strings.Contains(body.HistoryError, "HTTP") {
		t.Errorf("history_error = %q, want the upstream failure it came from", body.HistoryError)
	}
	if len(body.History) != 0 {
		t.Errorf("history = %+v, want none: the read failed", body.History)
	}
}

// The ordinary path carries no error field, so a client can read its presence
// as "this half is missing".
func TestCardPullPanelNoHistoryErrorWhenBothReadsSucceed(t *testing.T) {
	f := newPullPanelForge(t, map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"repository_hooks": "write", "statuses": "read", "checks": "read",
	})
	s, cardID := pullPanelFixture(t, f)

	w := httptest.NewRecorder()
	s.handleIssuePullCI(w, pullPanelReq("GET", "/api/v1/native/issues/"+cardID+"/pulls/7/ci", cardID, "7"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "history_error") {
		t.Errorf("body = %s, want no history_error when the history read worked", w.Body.String())
	}
}
