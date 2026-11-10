package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A slot that exists but cannot serve NOW is a wait, not an absent key
// (P-1 of the round-3 review). A frozen team_gateway resume whose pool entry
// is busy — another run holds its one slot, another run spent its day, the
// attachment's own draw cap is spent, a vendor entry of a mixed run is held —
// is refused before anything publishes, in the capacity class the team key at
// its cap answers (ErrCredentialAtCapacity: the retry sweeper defers it for
// free on the slot cadence until max_wait, an operator gets 429), its sentence
// saying the entry is busy. A refusal naming a provider whose entry is truly
// absent (paused) stays the unfunded one (runview.ErrNoLLMCredential), and so
// does a refusal naming one busy provider beside one absent: a wait cannot
// cure the absent one.
//
// Mutants, each run and restored: the busy refusal unwrapping to the unfunded
// class -> every wait row; the busy reading taking a paused entry as busy ->
// the paused rows; a refusal waiting when one named provider is absent -> the
// mixed absent row; the attached tier noting no busy entry -> the attached
// rows (the launch twin included); the legacy tier noting none -> the legacy
// row; a busy lent row whose audience refuses the launch read as busy -> the
// audience row; the preview's attached or legacy reading off -> the launch
// twin's or the legacy row's preview assert. pkg/server's
// TestBusyPoolEntry_ARefusalOnABusyEntryIsACapacityWait drives the sweeper,
// the operator resume and the launch on the same refusal.

func shapePledge(t *testing.T, pledges *credpool.MemoryPledgeStore, id string, shape func(*credpool.Pledge)) {
	t.Helper()
	ctx := context.Background()
	p, err := pledges.Get(ctx, id)
	if err != nil {
		t.Fatalf("get pledge %s: %v", id, err)
	}
	shape(&p)
	if err := pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert pledge %s: %v", id, err)
	}
}

func forfaitEntryPledge() string {
	return credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
}

// assertBusyClass asserts the refusal's class: a wait names the busy entry
// and never asks for a credential to be provisioned; an absence is the
// unfunded refusal.
func assertBusyClass(t *testing.T, err error, wait bool, providers []string, log string) {
	t.Helper()
	var refusal *UnfundedProvidersError
	if !errors.As(err, &refusal) || !slices.Equal(refusal.Providers, providers) {
		t.Fatalf("resume = %v, want the refusal naming %v (log:\n%s)", err, providers, log)
	}
	if got := errors.Is(err, ErrCredentialAtCapacity); got != wait {
		t.Fatalf("errors.Is(ErrCredentialAtCapacity) = %v, want %v: %v (log:\n%s)", got, wait, err, log)
	}
	if got := errors.Is(err, runview.ErrNoLLMCredential); got == wait {
		t.Fatalf("errors.Is(ErrNoLLMCredential) = %v, want %v: %v", got, !wait, err)
	}
	if wait && (!strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "provision")) {
		t.Fatalf("the wait's sentence = %q, want it to say the entry is busy and ask for nothing to be provisioned", err)
	}
	if !wait && !strings.Contains(err.Error(), "provision") {
		t.Fatalf("the absence's sentence = %q, want the provisioning remedy", err)
	}
}

func TestBusyPoolEntry_AFrozenTeamGatewayResumeWaitsForABusyAttachedEntry(t *testing.T) {
	mixed := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}},
		}}
	}
	frozen := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "key-gw", GatewayHost: "gw.example"}
	for _, row := range []struct {
		name string
		wf   func() *ir.Workflow
		// arrange shapes the fixture and lets other runs draw before the
		// resume of run-busy (whose launch, when launch is set, came first
		// and was reported).
		launch    bool
		arrange   func(t *testing.T, f *attachedFixture, att *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error))
		wait      bool
		providers []string
	}{
		{name: "another run holds the gateway entry's one slot", wf: gatewayOnlyWorkflow, launch: true, wait: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				limitGatewayEntry(t, f, credpool.Limits{MaxConcurrentRuns: 1})
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 1 {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
			}},
		{name: "another run spent the gateway entry's one run a day", wf: gatewayOnlyWorkflow, wait: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				limitGatewayEntry(t, f, credpool.Limits{MaxRunsPerDay: 1})
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 1 {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
			}},
		{name: "another run spent the attachment's own draw cap", wf: gatewayOnlyWorkflow, wait: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, att *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				att.Limits.MaxRunsPerDay = 1
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 1 {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
			}},
		{name: "a mixed run whose vendor entry another run holds", wf: mixed, launch: true, wait: true, providers: []string{"anthropic"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, forfaitEntryPledge(), func(p *credpool.Pledge) { p.Limits = credpool.Limits{MaxConcurrentRuns: 1} })
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 2 {
					t.Fatalf("fixture: the other run's launch: %v, leases %d", err, len(openLeases(t, f, "run-busy-other")))
				}
			}},
		{name: "the gateway entry paused (absent, control)", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) { p.Enabled = false })
			}},
		{name: "a mixed run, its vendor entry held and its gateway entry paused (control)", wf: mixed, launch: true, providers: []string{"anthropic", "openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, forfaitEntryPledge(), func(p *credpool.Pledge) { p.Limits = credpool.Limits{MaxConcurrentRuns: 1} })
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) { p.Enabled = false })
			}},
		{name: "a busy gateway entry whose lent row's audience refuses the launch (control)", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				limitGatewayEntry(t, f, credpool.Limits{MaxConcurrentRuns: 1})
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 1 {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
				k, err := f.apiKeys.Get(context.Background(), "key-gw")
				if err != nil {
					t.Fatalf("get key-gw: %v", err)
				}
				k.Bots = []string{"another-bot"}
				if err := f.apiKeys.Replace(context.Background(), k); err != nil {
					t.Fatalf("replace key-gw: %v", err)
				}
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAttachedFixture(t)
			f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
			var buf bytes.Buffer
			f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
			ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
			att := frozenAttachment("pool-att")
			resolve := func(runID string, b boundarySource) (credResolution, error) {
				wf := row.wf()
				return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, b)
			}
			stamp := frozen
			if row.launch {
				launch, err := resolve("run-busy", boundaryDerived)
				if err != nil || !launch.contentBoundary.TeamGateway() {
					t.Fatalf("fixture: launch = %v, boundary %+v, want team_gateway (log:\n%s)", err, launch.contentBoundary, buf.String())
				}
				stamp = launch.contentBoundary
				// One report settles one open lease: the attempt ended, every
				// lease sealed side by side closes.
				for range launch.grants {
					if err := f.pub.credPool.Report(context.Background(), "run-busy", credpool.Outcome{CostUSD: 0.01}); err != nil {
						t.Fatalf("report: %v", err)
					}
				}
				if open := openLeases(t, f, "run-busy"); len(open) != 0 {
					t.Fatalf("fixture: run-busy keeps %d open leases after its attempt ended", len(open))
				}
			}
			row.arrange(t, f, att, resolve)
			buf.Reset()
			_, err := resolve("run-busy", boundaryFrozen(stamp))
			assertBusyClass(t, err, row.wait, row.providers, buf.String())
		})
	}
}

// The legacy pool's twin: a team_gateway run's vendor route a donor of the
// mutualised pool funded at launch, the donor's one slot held by another run
// at resume — a wait, not an absent credential.
func TestBusyPoolEntry_AFrozenTeamGatewayResumeWaitsForABusyLegacyDonor(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxConcurrentRuns: 1})
	keys := secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, keys, f.sealer, poolTeam, nil)
	f.pub.apiKeys = keys
	var buf bytes.Buffer
	f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
	ctx := store.WithTenant(context.Background(), poolTeam)
	resolve := func(runID string, b boundarySource) (credResolution, error) {
		wf := mixedGatewayWorkflow()
		return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, b)
	}
	launch, err := resolve("run-legacy", boundaryDerived)
	if err != nil || !launch.contentBoundary.TeamGateway() || launch.grant == nil {
		t.Fatalf("fixture: launch = %v, boundary %+v, grant %v, want a team_gateway run the donor funds (log:\n%s)", err, launch.contentBoundary, launch.grant, buf.String())
	}
	if err := f.pub.credPool.Report(context.Background(), "run-legacy", credpool.Outcome{CostUSD: 0.01}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if other, err := resolve("run-legacy-other", boundaryDerived); err != nil || other.grant == nil {
		t.Fatalf("fixture: the other run's launch = %v, grant %v", err, other.grant)
	}
	buf.Reset()
	_, err = resolve("run-legacy", boundaryFrozen(launch.contentBoundary))
	assertBusyClass(t, err, true, []string{"anthropic"}, buf.String())
	// A launch now is refused the same way, and the preview — which reads
	// the team's org through the identity resolver — says so.
	_, err = resolve("run-legacy-third", boundaryDerived)
	assertBusyClass(t, err, true, []string{"anthropic"}, buf.String())
	f.pub.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}}
	preview, perr := previewReadOnly(f.pub).PreviewCredentials(ctx, previewSpec(poolTeam, "requester"), mixedGatewayWorkflow())
	if perr != nil {
		t.Fatalf("preview: %v", perr)
	}
	if w := previewWarning(preview, "The live launch refuses"); !strings.Contains(w, "busy now") || strings.Contains(w, "provision") {
		t.Fatalf("the preview's refusal = %q, want the launch's wait sentence; warnings %v", w, preview.Warnings)
	}
}

// The launch twin, and its preview: a mixed team_gateway LAUNCH whose vendor
// route only the attached pool's forfait funds, held by another run, is
// refused as a wait (ErrCredentialAtCapacity, the launch answers 429), and
// the credential preview predicts that refusal in the launch's own sentence —
// the busy entry named, nothing to provision.
func TestBusyPoolEntry_ALaunchOnABusyVendorEntryWaitsAndThePreviewSaysSo(t *testing.T) {
	mixed := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}},
		}}
	}
	f := newAttachedFixture(t)
	f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
	var buf bytes.Buffer
	f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
	shapePledge(t, f.pledges, forfaitEntryPledge(), func(p *credpool.Pledge) { p.Limits = credpool.Limits{MaxConcurrentRuns: 1} })
	ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
	att := frozenAttachment("pool-att")
	resolve := func(runID string) (credResolution, error) {
		wf := mixed()
		return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, boundaryDerived)
	}
	if other, err := resolve("run-busy-other"); err != nil || len(other.grants) != 2 {
		t.Fatalf("fixture: the other run's launch = %v, grants %d, want the forfait and the gateway entry", err, len(other.grants))
	}
	buf.Reset()
	_, err := resolve("run-busy-launch")
	assertBusyClass(t, err, true, []string{"anthropic"}, buf.String())
	spec := runviewPreviewSpecAttached()
	preview, perr := previewReadOnly(f.pub).PreviewCredentials(ctx, spec, mixed())
	if perr != nil {
		t.Fatalf("preview: %v", perr)
	}
	w := previewWarning(preview, "The live launch refuses")
	if !strings.Contains(w, ErrCredentialAtCapacity.Error()) || !strings.Contains(w, "busy now") || strings.Contains(w, "provision") {
		t.Fatalf("the preview's refusal = %q, want the launch's wait sentence (busy, nothing to provision); warnings %v", w, preview.Warnings)
	}
}
