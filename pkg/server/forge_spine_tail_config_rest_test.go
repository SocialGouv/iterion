package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The routing switch and its first-flip instant are provisioner-owned: the
// forge orchestrator projects them onto the config, and the webhook REST has
// no field for either. A client can therefore neither SET them — a body that
// names them is not a way around the routing API's refusals and approval
// gate — nor DROP them: an edit of an unrelated field, or a token rotation,
// writes the config back with the routing it read.
func TestWebhookREST_RoutingFieldsAreNotClientWritable(t *testing.T) {
	since := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	spine := webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}
	// What a client would send to move the switch, were it writable.
	const routingBody = `"forge_routing":{"issues":"legacy","pull_requests":"spine"},"spine_since":"2001-01-01T00:00:00Z"`

	create := func(t *testing.T, s *Server, body string) webhooks.Config {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks", body, "t1", ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
		}
		var created struct {
			Config webhooks.Config `json:"config"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create: %v", err)
		}
		return created.Config
	}
	stored := func(t *testing.T, s *Server, id string) webhooks.Config {
		t.Helper()
		cfg, err := s.webhookConfigs.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get config: %v", err)
		}
		return cfg
	}
	// flip writes the switch the way the provisioner does: on the store.
	flip := func(t *testing.T, s *Server, id string) {
		t.Helper()
		cfg := stored(t, s, id)
		cfg.ForgeRouting, cfg.SpineSince = spine, &since
		if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
			t.Fatalf("flip: %v", err)
		}
	}
	wantRouting := func(t *testing.T, when string, cfg webhooks.Config, routing webhooks.ForgeRouting, at *time.Time) {
		t.Helper()
		if cfg.ForgeRouting != routing {
			t.Errorf("%s: forge_routing = %+v, want %+v", when, cfg.ForgeRouting, routing)
		}
		switch {
		case at == nil && cfg.SpineSince != nil:
			t.Errorf("%s: spine_since = %v, want none", when, cfg.SpineSince)
		case at != nil && (cfg.SpineSince == nil || !cfg.SpineSince.Equal(*at)):
			t.Errorf("%s: spine_since = %v, want %v", when, cfg.SpineSince, *at)
		}
	}

	t.Run("a create cannot set them", func(t *testing.T) {
		s := newWebhookTestServer(t)
		cfg := create(t, s, `{"name":"gh","provider":"github","bot_ids":["review-pr"],`+routingBody+`}`)
		wantRouting(t, "the create's answer", cfg, webhooks.ForgeRouting{}, nil)
		wantRouting(t, "the stored config", stored(t, s, cfg.ID), webhooks.ForgeRouting{}, nil)
	})

	t.Run("a patch cannot set them", func(t *testing.T) {
		s := newWebhookTestServer(t)
		cfg := create(t, s, `{"name":"gh","provider":"github","bot_ids":["review-pr"]}`)
		w := httptest.NewRecorder()
		s.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+cfg.ID,
			`{"name":"renamed",`+routingBody+`}`, "t1", cfg.ID))
		if w.Code != http.StatusOK {
			t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
		}
		got := stored(t, s, cfg.ID)
		if got.Name != "renamed" {
			t.Fatalf("the patch did not land (name %q)", got.Name)
		}
		wantRouting(t, "after a patch naming the fields", got, webhooks.ForgeRouting{}, nil)
	})

	t.Run("a patch can neither move nor drop a flipped switch", func(t *testing.T) {
		s := newWebhookTestServer(t)
		cfg := create(t, s, `{"name":"gh","provider":"github","bot_ids":["review-pr"]}`)
		flip(t, s, cfg.ID)

		for _, c := range []struct{ name, body string }{
			{"naming the fields", `{"name":"renamed-1",` + routingBody + `}`},
			{"blanking the fields", `{"name":"renamed-2","forge_routing":{},"spine_since":null}`},
			{"an unrelated edit", `{"name":"renamed-3","hold_labels":["wip"]}`},
		} {
			w := httptest.NewRecorder()
			s.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+cfg.ID, c.body, "t1", cfg.ID))
			if w.Code != http.StatusOK {
				t.Fatalf("patch %s: code=%d body=%s", c.name, w.Code, w.Body.String())
			}
			// The answer is the row the client reads next: it shows the
			// switch as stored, not as sent.
			var answered webhooks.Config
			if err := json.Unmarshal(w.Body.Bytes(), &answered); err != nil {
				t.Fatalf("decode patch %s: %v", c.name, err)
			}
			wantRouting(t, "the answer of a patch "+c.name, answered, spine, &since)
			wantRouting(t, "the stored config after a patch "+c.name, stored(t, s, cfg.ID), spine, &since)
		}
		if got := stored(t, s, cfg.ID); got.Name != "renamed-3" {
			t.Fatalf("the patches did not land (name %q)", got.Name)
		}
	})

	t.Run("a rotation keeps a flipped switch", func(t *testing.T) {
		s := newWebhookTestServer(t)
		cfg := create(t, s, `{"name":"gh","provider":"github","bot_ids":["review-pr"]}`)
		flip(t, s, cfg.ID)
		before := stored(t, s, cfg.ID)

		w := httptest.NewRecorder()
		s.handleRotateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks/"+cfg.ID+"/rotate",
			`{`+routingBody+`}`, "t1", cfg.ID))
		if w.Code != http.StatusOK {
			t.Fatalf("rotate: code=%d body=%s", w.Code, w.Body.String())
		}
		got := stored(t, s, cfg.ID)
		if got.TokenHash == before.TokenHash {
			t.Fatal("the rotation did not land (same token hash)")
		}
		wantRouting(t, "after a rotation", got, spine, &since)
	})
}
