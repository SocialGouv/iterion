package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// A connection whose token was minted WITH statuses, and whose status write
// then gets GitHub's 403 (the grant revoked after the mint, a repo the
// installation lost): the first approve fails at the write — nothing can know
// beforehand — but the refusal is recorded on the connection's client, so the
// NEXT approve's preflight reports statuses withheld and the lane writes
// through the webhook's forge_token binding instead. Before the denial was
// recorded, every approve on that connection failed the same way for up to
// an hour while a working binding sat unused.
func TestReviewApproveTakesTheBindingAfterAStatusWriteRefusal(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, true)
	f.statusForbidden = true
	f.perms["maintainer-jane"] = "maintain"
	approve := func(id int) map[string]string {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFromID("maintainer-jane", id), prforge.EventHeaderIssueComment, pt))
		if w.Code != http.StatusOK {
			t.Fatalf("approve #%d: code=%d body=%s", id, w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp
	}

	first := approve(556)
	if first["status"] == "revi-approved" {
		t.Fatalf("the first write on a token the forge refuses cannot land: %v", first)
	}
	second := approve(557)
	if second["status"] != "revi-approved" {
		t.Fatalf("the second approve must take the binding the recorded denial arms: %v (status bearers=%v)", second, f.bearersFor("status"))
	}
	bearers := f.bearersFor("status")
	if len(bearers) < 2 || bearers[len(bearers)-1] != "Bearer ghp_hand_owned" {
		t.Fatalf("the second status write must ride the forge_token binding, got %v", bearers)
	}
	statuses, _ := f.snapshot()
	if len(statuses) != 1 || statuses[0]["state"] != "success" {
		t.Fatalf("want exactly one landed status (through the binding), got %v", statuses)
	}
}
