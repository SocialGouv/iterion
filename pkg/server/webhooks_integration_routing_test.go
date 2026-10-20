package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The webhook lane is the integration's launch surface: the config's
// routing copy (which Provision stamps from the integration record) must
// reach the run doc and the queue message, and the gate may narrow ONLY
// through operator-owned vars — never through one the lane seeded from the
// delivery payload (ADR-129 §4 rule 2, the O6 witness).

type routingSpecCapture struct {
	mu   sync.Mutex
	spec runview.LaunchSpec
}

func (c *routingSpecCapture) SubmitLaunch(_ context.Context, _ string, spec runview.LaunchSpec, _ *ir.Workflow, _ *runview.CompiledSource) (int, error) {
	c.mu.Lock()
	c.spec = spec
	c.mu.Unlock()
	return 0, nil
}
func (*routingSpecCapture) CancelRun(context.Context, string) error { return nil }
func (*routingSpecCapture) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (*routingSpecCapture) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

const integrationProbeBot = `schema probe_out:
  ok: string

tool noop:
  command: ` + "`printf '{\"ok\":\"yes\"}'`" + `
  output: probe_out

workflow integration_probe:
  worktree: none
  entry: noop
  noop -> done
`

// launchIntegrationDelivery drives the GENERIC webhook lane (the
// provider-agnostic public entry) with the real launcher — no test seam
// between the lane and the run service — and returns the spec the
// publisher received.
func launchIntegrationDelivery(t *testing.T, cfg webhooks.Config, body string) runview.LaunchSpec {
	t.Helper()
	s := newWebhookTestServer(t)

	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "review-pr.bot"), []byte(integrationProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capture := &routingSpecCapture{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(capture))
	s.cfg.Store = fakeActiveStore{RunStore: rs}

	w := httptest.NewRecorder()
	s.handleGenericWebhook(w, genReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the delivery did not launch: code=%d body=%s", w.Code, w.Body.String())
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.spec.Vars == nil {
		t.Fatalf("the publisher received no launch spec — the lane did not reach it")
	}
	return capture.spec
}

// TestWebhookLane_CarriesIntegrationOverridesToTheRunDoc: the FULL chain —
// the integration's block provisioned onto the webhook copy, then an
// inbound delivery launching with those cells folded onto the queue
// message AND stamped on the run document — through the public entry, the
// way a real delivery arrives.
func TestWebhookLane_CarriesIntegrationOverridesToTheRunDoc(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	// The launch gate reads the team behind the webhook identity; the forge
	// fixtures never needed it (provisioning skips the approval gate when
	// the store is unwired), so seed what the delivery half requires.
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "t1", Name: "t1", Slug: "webhook-t1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	s.webhookCounter = webhooks.NewMemoryCounter()
	s.authLimiter = newAuthRateLimiter()
	s.orgUsage = orgusage.NewMemoryCounter()

	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "review-pr.bot"), []byte(integrationProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capture := &routingSpecCapture{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(capture))
	s.cfg.Store = fakeActiveStore{RunStore: rs}

	// The write half: Provision stamps the block onto the integration AND
	// its webhook copy.
	block := &llmroute.Policy{Overrides: []llmroute.Override{
		{Selector: "agent", Backend: "claw", Model: "openai_compatible/team-gateway"},
	}}
	res := seedRoutedIntegration(t, s, srv.URL, block)

	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing == nil {
		t.Fatalf("the provisioned webhook copy carries no routing block — the integration's pin would never reach a launch")
	}
	cfg.ProjectAllowlist = nil // the probe delivery clones a stand-in URL

	w := httptest.NewRecorder()
	s.handleGenericWebhook(w, genReq(gitlabCtx(cfg), `{"bot":"review-pr","vars":{},"idempotency_key":"evt-routing-1","repo_url":"https://example.com/x.git","repo_ref":"main"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the delivery did not launch: code=%d body=%s", w.Code, w.Body.String())
	}
	capture.mu.Lock()
	spec := capture.spec
	capture.mu.Unlock()
	if spec.Vars == nil {
		t.Fatalf("the publisher received no launch spec — the lane did not reach it")
	}

	var found bool
	for _, o := range spec.ModelOverrides {
		if o.Selector == "agent" && o.Backend == "claw" && o.Model == "openai_compatible/team-gateway" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the integration's cell is not on the queue message (the folded list): %+v", spec.ModelOverrides)
	}
	// The resolved snapshot rides the same spec: it is what the run doc
	// stamps as llm_route_policy and what a resume replays frozen.
	if spec.LLMRoutePolicy == nil || len(spec.LLMRoutePolicy.Overrides) == 0 {
		t.Fatalf("the launch's resolved policy carries no overrides — the run doc would stamp a pinless chain: %+v", spec.LLMRoutePolicy)
	}
	for _, o := range spec.LLMRoutePolicy.Overrides {
		if o.Selector == "agent" && o.Backend == "claw" {
			return
		}
	}
	t.Fatalf("the integration's cell is not in the resolved policy snapshot: %+v", spec.LLMRoutePolicy.Overrides)
}

// TestTrustedVarNames_OnlyTheOperatorLayerNarrowsTheGate: the lane seeds
// vars from the delivery payload; only the operator's own layer may
// declare a name trusted, so a routing field templated from a payload var
// stays undecided at the gate instead of certifying the payload's choice.
func TestTrustedVarNames_OnlyTheOperatorLayerNarrowsTheGate(t *testing.T) {
	cfg := webhooks.Config{
		ID: "gnw", TenantID: "t1", Provider: webhooks.ProviderGeneric, Enabled: true,
		BotIDs: []string{"review-pr"},
		// The operator's own layer: two pinned names.
		OperatorLaunchVars: map[string]string{"gate_context": "revi/review", "arm_automerge": "false"},
	}
	body := `{"bot":"review-pr","vars":{"scope_notes":"payload title","severity":"low"},"idempotency_key":"evt-trust-1","repo_url":"https://example.com/x.git","repo_ref":"main"}`
	spec := launchIntegrationDelivery(t, cfg, body)

	got := map[string]bool{}
	for _, name := range spec.TrustedVarNames {
		got[name] = true
	}
	for _, name := range []string{"gate_context", "arm_automerge"} {
		if !got[name] {
			t.Errorf("operator-pinned %q is not trusted — the operator's own layer must narrow the gate", name)
		}
	}
	for _, name := range []string{"scope_notes", "severity"} {
		if got[name] {
			t.Errorf("payload-derived %q is trusted — a routing field templated from it would hand the harness, model and credential choice to whoever wrote the delivery", name)
		}
	}
}
