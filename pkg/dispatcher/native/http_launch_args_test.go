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

// The comments door is the same client surface, and its bot stamp replaces
// bot args wholesale: a comment carrying the reserved keys must never
// persist them — not the forged pair (integration ids are readable on any
// webhook-created card of the team, and a forged
// __iterion_webhook_id + __iterion_project_path re-keys the run's meter,
// bound and BYOK audience), not the BYOK overrides.
func TestBoardCommentDoorNeverPersistsTheLaunchContext(t *testing.T) {
	srv, s := newServerWithStore(t)
	defer srv.Close()

	// A card the server stamped: the comment's own var lands, the stored
	// launch context survives the wholesale stamp.
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
	body := bytes.NewBufferString(`{
	  "author": "member",
	  "body": "run this",
	  "bot": "probe",
	  "bot_args": {"member_var2": "v2", "__iterion_project_path": "harmless/other", "__iterion_webhook_id": "wh-x", "__iterion_key_overrides": "{}"}
	}`)
	r, err := http.Post(srv.URL+"/issues/"+stored.ID+"/comments", "application/json", body)
	if err != nil {
		t.Fatalf("POST comment: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST comment = %d", r.StatusCode)
	}
	after, err := s.Get(stored.ID)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if after.BotArgs["member_var2"] != "v2" {
		t.Fatalf("the comment's own var did not land (%v) — this test only means something while the door is open", after.BotArgs)
	}
	if _, carried := after.BotArgs["member_var"]; carried {
		t.Errorf("a wholesale bot-args stamp must replace the member's own vars: %v", after.BotArgs)
	}
	for _, k := range []string{"__iterion_project_path", "__iterion_webhook_id", "__iterion_repo_url", "__iterion_key_overrides"} {
		if got, ok := after.BotArgs[k]; ok && got != stored.BotArgs[k] {
			t.Errorf("the comment persisted the reserved %s = %q (forged value), want the stored %q — launch context is the server's stamp, not a comment's", k, got, stored.BotArgs[k])
		}
	}

	// A card with no stored context: a comment stamps no reserved key at
	// all — there is nothing honest to keep, so the forged keys must be
	// gone entirely.
	bare, err := s.Create(native.Issue{Title: "bare card", State: "ready"})
	if err != nil {
		t.Fatalf("store Create bare: %v", err)
	}
	body2 := bytes.NewBufferString(`{
	  "body": "run this too",
	  "bot": "probe",
	  "bot_args": {"__iterion_project_path": "harmless/other", "__iterion_webhook_id": "wh-x", "__iterion_secret_overrides": "{}", "__iterion_repo_url": "https://github.com/acme/widgets.git"}
	}`)
	r2, err := http.Post(srv.URL+"/issues/"+bare.ID+"/comments", "application/json", body2)
	if err != nil {
		t.Fatalf("POST comment bare: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("POST comment bare = %d", r2.StatusCode)
	}
	bareAfter, err := s.Get(bare.ID)
	if err != nil {
		t.Fatalf("store Get bare: %v", err)
	}
	for _, res := range native.ReservedLaunchArgs {
		if got, ok := bareAfter.BotArgs[res]; ok {
			t.Errorf("the comment door persisted the reserved %s = %q on an unstamped card — the forge rides the one unfiltered door", res, got)
		}
	}
}
