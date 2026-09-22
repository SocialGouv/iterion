package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type credentialPreviewPublisher struct {
	tierPublisher
	preview       runview.CredentialPreviewSpec
	previewWF     *ir.Workflow
	launched      runview.LaunchSpec
	launchedWF    *ir.Workflow
	launchedOwner string
}

func (p *credentialPreviewPublisher) PreviewCredentials(_ context.Context, s runview.CredentialPreviewSpec, wf *ir.Workflow) (runview.CredentialPreview, error) {
	p.preview = s
	p.previewWF = wf
	return runview.CredentialPreview{Context: s.Context, Candidates: []runview.CredentialPreviewCandidate{}, Warnings: []string{}}, nil
}
func (p *credentialPreviewPublisher) SubmitLaunch(ctx context.Context, _ string, s runview.LaunchSpec, wf *ir.Workflow, _ *runview.CompiledSource) (int, error) {
	p.launched = s
	p.launchedWF = wf
	p.launchedOwner, _ = store.OwnerFromContext(ctx)
	return 0, nil
}

func callCredentialPreview(s *Server, body string, id auth.Identity) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/teams/t1/credentials/preview", strings.NewReader(body))
	r.SetPathValue("id", "t1")
	r = r.WithContext(auth.WithIdentity(r.Context(), id))
	w := httptest.NewRecorder()
	s.handleCredentialPreview(w, r)
	return w
}

func TestCredentialPreviewWebhookMatchesActualLaunchSourceAndIdentity(t *testing.T) {
	s, rs := newTeamForkServer(t, &tierPublisher{})
	// A real model-bearing team override makes route drift falsifiable; the
	// baked tool-only bot would never exercise credential-provider narrowing.
	row, err := s.botSources.GetBySlug(store.WithTenant(t.Context(), "t1"), "t1", "probe")
	if err != nil {
		t.Fatal(err)
	}
	row.Files[botsource.MainBotFile] = "agent TEAMFORK:\n  backend: claw\n  model: \"${ITERION_PREVIEW_MODEL_TEST:-openai/gpt-5.6-sol}\"\n\nworkflow team_model:\n  entry: TEAMFORK\n  TEAMFORK -> done\n"
	if _, err := s.botSources.Update(store.WithTenant(t.Context(), "t1"), row); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_PREVIEW_MODEL_TEST", "anthropic/claude-sonnet-4-6")
	if _, _, err := runview.CompileWorkflowFromSource("probe.bot", row.Files[botsource.MainBotFile]); err != nil {
		t.Fatalf("LLM fixture compile: %v", err)
	}
	pub := &credentialPreviewPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	configs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = configs
	cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe", "other"}, DefaultBotID: "probe", KeyOverrides: map[string]string{"openai": "team-pinned-key"}, LaunchVars: map[string]string{"test_var": "configured"}, OperatorLaunchVars: map[string]string{"test_var": "operator"}}
	if err := configs.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	w := callCredentialPreview(s, `{"source":{"kind":"webhook","id":"hook"}}`, auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember})
	if w.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", w.Code, w.Body.String())
	}
	if pub.previewWF == nil || !strings.Contains(pub.preview.Launch.Source, "TEAMFORK") {
		t.Fatal("preview bypassed the team bot source")
	}
	if pub.preview.Launch.BundleDir == "" {
		t.Fatal("stored bundle did not reach preview compilation")
	}
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook}), "t1", "webhook:hook")
	vars := applyWebhookVarLayers(map[string]string{}, cfg)
	if _, err := s.launchWebhookBot(ctx, cfg, "probe", vars, "", "", "", cfg.KeyOverrides, nil, store.RunTrustDefault, ""); err != nil {
		t.Fatal(err)
	}
	if pub.preview.OwnerID != pub.launchedOwner || pub.preview.OwnerID != "webhook:hook" {
		t.Fatalf("wrong derived owner preview=%q real=%q", pub.preview.OwnerID, pub.launchedOwner)
	}
	if pub.preview.Context.BotID != pub.launched.BotID || pub.previewWF.Name != pub.launchedWF.Name {
		t.Fatal("preview and webhook launched different bots")
	}
	if !reflect.DeepEqual(pub.preview.Launch.KeyOverrides, pub.launched.KeyOverrides) || !reflect.DeepEqual(pub.preview.Launch.Vars, pub.launched.Vars) {
		t.Fatalf("configured override drift preview=%+v actual=%+v", pub.preview.Launch.KeyOverrides, pub.launched.KeyOverrides)
	}
	known := map[string]bool{"openai": true, "anthropic": true}
	a := model.EffectiveProviders(pub.previewWF, runview.ModelOverridesFromRun(runview.RunModelOverrides(pub.preview.Launch.ModelOverrides)), nil, known)
	b := model.EffectiveProviders(pub.launchedWF, runview.ModelOverridesFromRun(runview.RunModelOverrides(pub.launched.ModelOverrides)), nil, known)
	if !a.NarrowSafe || !reflect.DeepEqual(a.Providers, []string{"anthropic"}) {
		t.Fatalf("model-bearing oracle did not narrow: %+v", a)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("route derivation drift preview=%+v launch=%+v", a, b)
	}
	var out runview.CredentialPreview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Context.Source.ID != "hook" || out.Context.Source.Kind != "webhook" {
		t.Fatal("response lost real source context")
	}
}

func TestCredentialPreviewRejectsCrossTenantAndInventedOwners(t *testing.T) {
	s, _ := newTeamForkServer(t, &tierPublisher{})
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	for _, cfg := range []webhooks.Config{{ID: "foreign", TenantID: "t2", BotIDs: []string{"probe"}}, {ID: "ambiguous", TenantID: "t1", BotIDs: []string{"probe", "other"}}} {
		if err := s.webhookConfigs.Create(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	member := auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember}
	for _, tc := range []struct {
		name, body string
		id         auth.Identity
		status     int
	}{
		{"foreign source", `{"source":{"kind":"webhook","id":"foreign"}}`, member, 404},
		{"ambiguous", `{"source":{"kind":"webhook","id":"ambiguous"}}`, member, 400},
		{"not allowed", `{"source":{"kind":"webhook","id":"ambiguous"},"bot_id":"unlisted"}`, member, 400},
		{"owner", `{"source":{"kind":"personal"},"bot_id":"probe","owner_id":"another-user"}`, member, 400},
		{"personal id", `{"source":{"kind":"personal","id":"another-user"},"bot_id":"probe"}`, member, 400},
		{"foreign member", `{"source":{"kind":"personal"},"bot_id":"probe"}`, auth.Identity{UserID: "outsider", TeamID: "t2", Role: identity.RoleMember}, 403},
		{"synthetic", `{"source":{"kind":"personal"},"bot_id":"probe"}`, auth.Identity{UserID: "webhook:x", TeamID: "t1", Role: identity.RoleOwner, Kind: auth.KindWebhook}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := callCredentialPreview(s, tc.body, tc.id)
			if w.Code != tc.status {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
