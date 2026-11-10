package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

// coolingFor is a provider cooldown ending d from now.
func coolingFor(d time.Duration) *time.Time {
	until := time.Now().Add(d)
	return &until
}

// closedWindow is a one-hour sharing window twelve hours away from now (UTC).
func closedWindow() *credpool.Window {
	start := (time.Now().UTC().Hour() + 12) % 24
	return &credpool.Window{StartHour: start, EndHour: (start + 1) % 24}
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
		{name: "the gateway entry cools (a wait)", wf: gatewayOnlyWorkflow, launch: true, wait: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) { p.CooldownUntil = coolingFor(time.Hour) })
			}},
		{name: "the gateway entry is out of its sharing hours (a wait)", wf: gatewayOnlyWorkflow, launch: true, wait: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) { p.Window = closedWindow() })
			}},
		{name: "the gateway entry's pledge serves another bot (absent, control)", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) { p.Bots = []string{"another-bot"} })
			}},
		{name: "the gateway entry's pledge serves another bot and cools", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) {
					p.Bots = []string{"another-bot"}
					p.CooldownUntil = coolingFor(time.Hour)
				})
			}},
		{name: "the gateway entry's pledge serves another bot and is out of its sharing hours", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, _ func(string, boundarySource) (credResolution, error)) {
				shapePledge(t, f.pledges, gatewayEntryPledge(), func(p *credpool.Pledge) {
					p.Bots = []string{"another-bot"}
					p.Window = closedWindow()
				})
			}},
		{name: "another run holds the gateway entry's one slot and its lent key has expired", wf: gatewayOnlyWorkflow, launch: true, providers: []string{"openai_compatible"},
			arrange: func(t *testing.T, f *attachedFixture, _ *store.RunPoolAttachment, resolve func(string, boundarySource) (credResolution, error)) {
				limitGatewayEntry(t, f, credpool.Limits{MaxConcurrentRuns: 1})
				if _, err := resolve("run-busy-other", boundaryDerived); err != nil || len(openLeases(t, f, "run-busy-other")) != 1 {
					t.Fatalf("fixture: the other run's launch: %v", err)
				}
				k, err := f.apiKeys.Get(context.Background(), "key-gw")
				if err != nil {
					t.Fatalf("get key-gw: %v", err)
				}
				expired := time.Now().Add(-time.Minute)
				k.ExpiresAt = &expired
				if err := f.apiKeys.Replace(context.Background(), k); err != nil {
					t.Fatalf("replace key-gw: %v", err)
				}
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

// An entry is a wait only when it would serve THIS launch once its slot, its
// day, its cooldown or its sharing hours clear. A vendor entry whose pledge
// serves another bot stays an absence while it cools (the broker reports the
// cooldown before the allow-list), and a held entry whose subscription was
// disconnected is an absence the freed slot would only discover: the launch
// is refused as unfunded and its preview predicts that refusal, in the
// launch's own sentence. The held entry whose credential holds is the wait.
func TestBusyPoolEntry_ALaunchWaitsOnlyForAnEntryThatWouldServeIt(t *testing.T) {
	mixed := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}},
		}}
	}
	for _, row := range []struct {
		name string
		// held: another run holds the forfait entry's one slot.
		held  bool
		shape func(t *testing.T, f *attachedFixture)
		wait  bool
	}{
		{name: "another run holds the forfait entry (a wait, control)", held: true, wait: true},
		{name: "the forfait entry's pledge serves another bot and cools",
			shape: func(t *testing.T, f *attachedFixture) {
				shapePledge(t, f.pledges, forfaitEntryPledge(), func(p *credpool.Pledge) {
					p.Bots = []string{"another-bot"}
					p.CooldownUntil = coolingFor(5 * time.Hour)
				})
			}},
		{name: "another run holds the forfait entry and its subscription was disconnected", held: true,
			shape: func(t *testing.T, f *attachedFixture) {
				rec, err := f.oauth.Get(context.Background(), "donor", secrets.OAuthKind("claude_code"))
				if err != nil {
					t.Fatalf("get the donor's subscription: %v", err)
				}
				if err := f.oauth.Delete(context.Background(), rec.ID); err != nil {
					t.Fatalf("disconnect the donor's subscription: %v", err)
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
			resolve := func(runID string) (credResolution, error) {
				wf := mixed()
				return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, boundaryDerived)
			}
			if row.held {
				shapePledge(t, f.pledges, forfaitEntryPledge(), func(p *credpool.Pledge) { p.Limits = credpool.Limits{MaxConcurrentRuns: 1} })
				if other, err := resolve("run-busy-other"); err != nil || len(other.grants) != 2 {
					t.Fatalf("fixture: the other run's launch = %v, grants %d, want the forfait and the gateway entry", err, len(other.grants))
				}
			}
			if row.shape != nil {
				row.shape(t, f)
			}
			buf.Reset()
			_, err := resolve("run-busy-launch")
			assertBusyClass(t, err, row.wait, []string{"anthropic"}, buf.String())
			preview, perr := previewReadOnly(f.pub).PreviewCredentials(ctx, runviewPreviewSpecAttached(), mixed())
			if perr != nil {
				t.Fatalf("preview: %v", perr)
			}
			w := previewWarning(preview, "The live launch refuses")
			if row.wait && (!strings.Contains(w, "busy now") || strings.Contains(w, "provision")) {
				t.Fatalf("the preview's refusal = %q, want the launch's wait sentence; warnings %v", w, preview.Warnings)
			}
			if !row.wait && (!strings.Contains(w, "provision") || strings.Contains(w, ErrCredentialAtCapacity.Error())) {
				t.Fatalf("the preview's refusal = %q, want the launch's unfunded sentence; warnings %v", w, preview.Warnings)
			}
		})
	}
}

// legacyTwoDonorFixture is an org legacy pool with two donors of one slot
// each: a Claude forfait (anthropic) and a lent OpenAI key (openai). The team
// holds only its gateway key.
func legacyTwoDonorFixture(t *testing.T) *Publisher {
	t.Helper()
	return legacyDonorsFixture(t, secrets.ProviderOpenAI, nil)
}

// legacyDonorsFixture is legacyTwoDonorFixture whose second donor lends a key
// of provider lent, its row's workload audience keyBots.
func legacyDonorsFixture(t *testing.T, lent secrets.Provider, keyBots []string) *Publisher {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	lentKeys := secrets.NewMemoryApiKeyStore()
	oa := secrets.ApiKey{ID: "key-oa", TenantID: "team-donor2", ScopeTeamID: "team-donor2", ScopeUserID: "donor2", Provider: lent, Name: "donor2 " + string(lent), Bots: keyBots,
		Fingerprint: secrets.APIKeyFingerprint(lent, "sk-oa-0000", "")}
	sealed, err := secrets.SealAPIKeyFor(sealer, oa, []byte("sk-oa-0000"))
	if err != nil {
		t.Fatalf("seal the lent key: %v", err)
	}
	oa.SealedSecret = sealed
	if err := lentKeys.Create(ctx, oa); err != nil {
		t.Fatalf("seed the lent key: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	for _, p := range []credpool.Pledge{
		{ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1", UserID: "donor",
			Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}, Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxConcurrentRuns: 1}},
		{ID: credpool.PledgeID("donor2", credpool.SourceAPIKey, string(lent)), PoolID: "pool-1", UserID: "donor2",
			Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(lent), KeyID: "key-oa"}, Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxConcurrentRuns: 1}},
	} {
		if err := pledges.Upsert(ctx, p); err != nil {
			t.Fatalf("seed pledge %s: %v", p.ID, err)
		}
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(), OAuth: oauth, APIKeys: lentKeys, Sealer: sealer, Logger: testLogger()})
	teamKeys := secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, teamKeys, sealer, poolTeam, nil)
	return &Publisher{runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, apiKeys: teamKeys,
		logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude, identity: &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}}}
}

// legacyRoutes is a team_gateway run with a gateway route and, per name, a
// decided vendor primary: review (anthropic), verdict (openai).
func legacyRoutes(nodes ...string) *ir.Workflow {
	models := map[string]string{"gw": "openai_compatible/team/m", "review": "anthropic/claude-opus-5", "verdict": "openai/gpt-6"}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
	for _, id := range append([]string{"gw"}, nodes...) {
		wf.Nodes[id] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: "claw", Model: models[id]}, Tools: []string{}}
	}
	return wf
}

// The legacy pool lends ONE credential per run (the broker grants the first
// want that serves). A strict arm's refusal (these team_gateway runs')
// naming two providers that only its busy entries hold is no wait: once both
// free, the pool grants one and the same launch is refused for the other. The
// knob's refusal, which one grant ends, waits (busy_wait_guarantee_test.go). It is refused as unfunded, its sentence
// saying why. And the preview notes a legacy candidate only when the pool
// selects nothing, as the walk notes a skip only on the pool's abstention:
// with the anthropic donor free and the openai donor held, the walk grants the
// forfait and refuses openai as unfunded — so does the preview. A pick the
// lent row's audience then withholds is a selection all the same: the walk
// reads no skip of it, and the preview no candidate.
func TestBusyPoolEntry_TheLegacyPoolsBusyEntriesWaitForOneProviderOnly(t *testing.T) {
	resolve := func(p *Publisher, runID string, wf *ir.Workflow) (credResolution, error) {
		ctx := store.WithTenant(context.Background(), poolTeam)
		return p.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	}
	hold := func(t *testing.T, p *Publisher, runID string, wf *ir.Workflow) {
		t.Helper()
		if cr, err := resolve(p, runID, wf); err != nil || cr.grant == nil {
			t.Fatalf("fixture: %s's launch = %v, grant %v, want a donor's grant", runID, err, cr.grant)
		}
	}
	t.Run("both donors held: no wait funds both", func(t *testing.T) {
		p := legacyTwoDonorFixture(t)
		hold(t, p, "run-other-oa", legacyRoutes("verdict"))
		hold(t, p, "run-other-an", legacyRoutes("review"))
		_, err := resolve(p, "run-3", legacyRoutes("review", "verdict"))
		assertBusyClass(t, err, false, []string{"anthropic", "openai"}, "")
		if !strings.Contains(err.Error(), "busy now") || !strings.Contains(err.Error(), "one credential per run") {
			t.Fatalf("the refusal = %q, want it to name the busy entries and why no wait funds both", err)
		}
	})
	t.Run("one donor held for the one provider it lacks: a wait (control)", func(t *testing.T) {
		p := legacyTwoDonorFixture(t)
		hold(t, p, "run-other-oa", legacyRoutes("verdict"))
		_, err := resolve(p, "run-3", legacyRoutes("verdict"))
		assertBusyClass(t, err, true, []string{"openai"}, "")
	})
	t.Run("both donors free: the pool grants one, the other is unfunded", func(t *testing.T) {
		p := legacyTwoDonorFixture(t)
		cr, err := resolve(p, "run-3", legacyRoutes("review", "verdict"))
		assertBusyClass(t, err, false, []string{"openai"}, "")
		if cr.grant == nil || cr.grant.Ref != "claude_code" {
			t.Fatalf("premise: the pool grants the first want that serves (the forfait), got %v", cr.grant)
		}
	})
	t.Run("the openai donor held, the anthropic donor free: the preview predicts the walk", func(t *testing.T) {
		p := legacyTwoDonorFixture(t)
		hold(t, p, "run-other-oa", legacyRoutes("verdict"))
		ctx := store.WithTenant(context.Background(), poolTeam)
		preview, perr := previewReadOnly(p).PreviewCredentials(ctx, previewSpec(poolTeam, "requester"), legacyRoutes("review", "verdict"))
		if perr != nil {
			t.Fatalf("preview: %v", perr)
		}
		_, err := resolve(p, "run-3", legacyRoutes("review", "verdict"))
		assertBusyClass(t, err, false, []string{"openai"}, "")
		w := previewWarning(preview, "The live launch refuses")
		if !strings.Contains(w, "provision") || strings.Contains(w, ErrCredentialAtCapacity.Error()) {
			t.Fatalf("the preview's refusal = %q, want the walk's unfunded refusal of openai; warnings %v", w, preview.Warnings)
		}
	})
	t.Run("the forfait held, the broker's pick withheld by its lent row's audience: neither waits", func(t *testing.T) {
		p := legacyDonorsFixture(t, secrets.ProviderAnthropic, []string{"another-bot"})
		hold(t, p, "run-other-an", legacyRoutes("review"))
		ctx := store.WithTenant(context.Background(), poolTeam)
		preview, perr := previewReadOnly(p).PreviewCredentials(ctx, previewSpec(poolTeam, "requester"), legacyRoutes("review"))
		if perr != nil {
			t.Fatalf("preview: %v", perr)
		}
		cr, err := resolve(p, "run-3", legacyRoutes("review"))
		if len(cr.releasedGrants) != 1 || cr.releasedGrants[0].Ref != string(secrets.ProviderAnthropic) {
			t.Fatalf("premise: the broker picks the free lent key and its row's audience withholds it, released %v", cr.releasedGrants)
		}
		assertBusyClass(t, err, false, []string{"anthropic"}, "")
		w := previewWarning(preview, "The live launch refuses")
		if !strings.Contains(w, "provision") || strings.Contains(w, ErrCredentialAtCapacity.Error()) {
			t.Fatalf("the preview's refusal = %q, want the walk's unfunded refusal of anthropic; warnings %v", w, preview.Warnings)
		}
	})
}

// The tolerated warning of a team_gateway run says why each kind of site is
// left unrefused: a supervisor is never started under the boundary (runview
// refuses every spec), which no "rescue route" or "env dial" explains.
func TestTeamGatewayTolerated_NamesASupervisorAsASupervisor(t *testing.T) {
	sites := map[string][]string{"xai": {"supervisor:watch"}, "openai": {"review#rescue"}}
	for _, row := range []struct {
		name               string
		attributed         []string
		supervisor, rescue bool
	}{
		{"a supervisor only", []string{"xai"}, true, false},
		{"a rescue only", []string{"openai"}, false, true},
		{"both", []string{"openai", "xai"}, true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			named, why := teamGatewayTolerated(row.attributed, sites, nil, map[string]bool{})
			if named == "" {
				t.Fatal("nothing tolerated")
			}
			if got := strings.Contains(why, "supervisor is never started"); got != row.supervisor {
				t.Fatalf("why = %q, want the supervisor reason %v", why, row.supervisor)
			}
			if got := strings.Contains(why, "rescue route"); got != row.rescue {
				t.Fatalf("why = %q, want the rescue reason %v", why, row.rescue)
			}
		})
	}
}
