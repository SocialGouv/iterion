package native_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The board REST surface is a client surface: it never persists the reserved
// launch context a client sends (the server stamps it, ensureBoardCard), and
// a wholesale patch keeps the STORED context while the client's own vars
// land. The remote MCP tools and the CLI drive exactly this door.
func TestBoardIssueRoutesNeverPersistTheLaunchContext(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()

	body := bytes.NewBufferString(`{
	  "title": "injected",
	  "state": "ready",
	  "bot": "probe",
	  "bot_args": {"member_var": "v", "__iterion_project_path": "harmless/other", "__iterion_webhook_id": "wh-x"}
	}`)
	r, err := http.Post(srv.URL+"/issues", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var created native.Issue
	_ = json.NewDecoder(r.Body).Decode(&created)
	r.Body.Close()
	if created.BotArgs["member_var"] != "v" {
		t.Fatalf("the route dropped the client's own var (%v) — this test only means something while the door is open", created.BotArgs)
	}
	for _, res := range native.ReservedLaunchArgs {
		if got, ok := created.BotArgs[res]; ok {
			t.Errorf("create persisted the reserved %s = %q — launch context is the server's stamp, not the client's", res, got)
		}
	}

	// A card the server stamped (ensureBoardCard writes the store directly):
	// a PATCH of the member's own vars must not rewrite the launch context.
	stored, err := s.Create(native.Issue{
		Title: "webhook card", State: "ready",
		BotArgs: map[string]string{
			"member_var":             "v1",
			"__iterion_repo_url":     "https://github.com/acme/widgets.git",
			"__iterion_project_path": "acme/widgets",
			"__iterion_webhook_id":   "wh-t1",
		},
	})
	if err != nil {
		t.Fatalf("store Create: %v", err)
	}
	patch := bytes.NewBufferString(`{"bot_args": {"member_var2": "v2", "__iterion_project_path": "harmless/other"}}`)
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/issues/"+stored.ID, patch)
	if err != nil {
		t.Fatalf("PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	var upd native.Issue
	_ = json.NewDecoder(res.Body).Decode(&upd)
	res.Body.Close()
	if upd.BotArgs["member_var2"] != "v2" {
		t.Errorf("the patch's own var did not land: %v", upd.BotArgs)
	}
	if _, carried := upd.BotArgs["member_var"]; carried {
		t.Errorf("a wholesale patch must replace the member's own vars: %v", upd.BotArgs)
	}
	for _, k := range []string{"__iterion_project_path", "__iterion_webhook_id", "__iterion_repo_url"} {
		if got := upd.BotArgs[k]; got != stored.BotArgs[k] {
			t.Errorf("the patch rewrote the reserved %s to %q, want the stored %q", k, got, stored.BotArgs[k])
		}
	}
}
