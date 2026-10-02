package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// E3 (#652 review round 1): the remote CLI must validate the budget
// override client-side so a typo (`--max-duration "4 hours"`) fails
// immediately with an actionable message, before the round trip.
// The server re-validates as the authoritative gate; the client-side
// check is the fast-fail so an operator doesn't wait on a wire error.
func TestRemoteRunsResume_RejectsMalformedBudgetDuration(t *testing.T) {
	// Empty RemoteClient — the validate step short-circuits before any
	// network I/O, so the client's URL/token/etc are irrelevant here.
	err := RemoteRunsResume(context.Background(), &RemoteClient{}, &Printer{}, "run-1", RemoteRunsResumeOptions{
		MaxDuration: "4 hours", // typo — must be "4h"
	})
	if err == nil {
		t.Fatal("RemoteRunsResume accepted a malformed max_duration; the operator would wait on the round-trip error")
	}
	if !strings.Contains(err.Error(), "budget") || !strings.Contains(err.Error(), "max_duration") {
		t.Fatalf("error = %v, want it to name the budget field and max_duration", err)
	}
}

// TestRemoteRunsResume_carriesTheScratchsConsent: `iterion remote runs resume`
// sends accept_scratch_loss only when given it; --force does not imply it.
func TestRemoteRunsResume_carriesTheScratchsConsent(t *testing.T) {
	for _, accept := range []bool{false, true} {
		var got map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"run_id":"r1","status":"queued"}`))
		}))
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "t"})
		err := RemoteRunsResume(context.Background(), c, &Printer{W: io.Discard, Format: OutputJSON}, "r1", RemoteRunsResumeOptions{Force: true, AcceptScratchLoss: accept})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if (got["accept_scratch_loss"] == true) != accept || got["force"] != true {
			t.Fatalf("accept=%v: request body %v", accept, got)
		}
	}
}
