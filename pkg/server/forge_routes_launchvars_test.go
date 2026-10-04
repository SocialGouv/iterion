package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The integration PATCH is how an operator switches a repo's gate (e.g. its
// gate_context). Two properties make that gesture honest: a launch_vars patch
// that would drop pins the caller did not echo is REFUSED with the echo named
// (never silently applied), and bot_ids is optional — a caller changing only
// the launch vars must not have to re-state the bot set.
func TestForgeRepoBots_PartialLaunchVarsPatchNamesWhatToEcho(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srv.URL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connResp.Connection.ID+`","repo":"group/api","bot_ids":["review-pr"],`+
			`"launch_vars":{"gate_context":"iterion/review","arm_automerge":"true","mono_family":"revi"}}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	patch := func(body string) *httptest.ResponseRecorder {
		r := forgeReq(ctx, "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, body, "t1")
		r.SetPathValue("integration_id", res.IntegrationID)
		rec := httptest.NewRecorder()
		s.handleUpdateForgeRepoBots(rec, r)
		return rec
	}
	assertPins := func(phase string, want map[string]string) {
		t.Helper()
		integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
		if err != nil {
			t.Fatal(err)
		}
		for key, v := range want {
			if got := integ.LaunchVars[key]; got != v {
				t.Errorf("%s: integration %s = %q, want %q", phase, key, got, v)
			}
			if got := cfg.OperatorLaunchVars[key]; got != v {
				t.Errorf("%s: config %s = %q, want %q — the enforcement half diverged from the report", phase, key, got, v)
			}
		}
	}

	// The defect: send one key, mean "change this one". Must be refused, naming
	// the pins to echo — never applied with the other two silently dropped.
	w = patch(`{"bot_ids":["review-pr"],"launch_vars":{"gate_context":"revi/review"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial launch_vars patch: code=%d body=%s — want 400 naming the pins to echo", w.Code, w.Body.String())
	}
	for _, key := range []string{"arm_automerge", "mono_family"} {
		if !strings.Contains(w.Body.String(), key) {
			t.Errorf("the refusal must name %q: %s", key, w.Body.String())
		}
	}
	assertPins("after refused patch", map[string]string{
		"gate_context": "iterion/review", "arm_automerge": "true", "mono_family": "revi",
	})

	// The documented switch: echo the pins, change the one — and bot_ids is
	// optional on a PATCH that touches no bot.
	w = patch(`{"launch_vars":{"gate_context":"revi/review","arm_automerge":"true","mono_family":"revi"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("echo switch without bot_ids: code=%d body=%s", w.Code, w.Body.String())
	}
	assertPins("after echo switch", map[string]string{
		"gate_context": "revi/review", "arm_automerge": "true", "mono_family": "revi",
	})
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(integ.BotIDs) != 1 || integ.BotIDs[0] != "review-pr" {
		t.Errorf("a launch-vars-only patch changed the bot set: %v", integ.BotIDs)
	}

	// An explicit empty bot list is still the 400 it always was: removing the
	// last bot is the DELETE.
	w = patch(`{"bot_ids":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("explicit empty bot_ids: code=%d, want 400", w.Code)
	}

	// Whole-map replacement stays possible as the explicit gesture.
	w = patch(`{"launch_vars":{"gate_context":"revi/review"},"launch_vars_replace":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("explicit replace: code=%d body=%s", w.Code, w.Body.String())
	}
	integ, err = s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(integ.LaunchVars) != 1 || integ.LaunchVars["gate_context"] != "revi/review" {
		t.Errorf("after explicit replace: %v, want exactly gate_context=revi/review", integ.LaunchVars)
	}
}
