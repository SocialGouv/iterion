// ADR-131 §4 and §5 on a replay, and the probe's attached-pool tier (round 2
// of the PR-C' review): the frozen stamp, not the probe, decides whether a
// resume's gateway routes are attributed — a team_gateway run keeps them (and
// a slot it cannot fill is refused by name), a run whose launch sealed no
// gateway key never seals one (a var read late at launch, a key created since,
// a fork re-routing a node). The attached pool's gateway entry is read for
// existence and audience, never availability, and a gateway route attributed
// to a key no tier then seals rides the runner's environment gateway, named.
//
// Mutants these witnesses red on, each run and restored: the replay rule's
// team_gateway arm dropped (attribution from the probe) → W22's resume rows
// together with the probe reading availability, and
// TestSubmitResume_ReplaysTheFrozenTeamGatewayBoundary alone; the hold-out
// arm dropped → W23's untrusted-var and fork rows; launchSealedGateway always
// false → W23's pool row; the own_keys stamp naming no sealed key → W23's pool
// row; the probe reading availability → W24's draw-capped row (and W21); the
// unsealed demotion off → W24's knob row and the platform row; the preview's
// demotion off → W24's preview assert; the attached arm reading paused,
// unhealthy and bot-filtered entries as there → W24's three status rows; a
// deleted entry row read as an unread one → W24's deleted-row row; the
// attached arm without repoOnly → W25.

package cloudpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayEntryPledge is the attached fixture's gateway entry.
func gatewayEntryPledge() string {
	return credpool.PledgeID("gw-lender", credpool.SourceAPIKey, string(secrets.ProviderOpenAICompatible))
}

func limitGatewayEntry(t *testing.T, f *attachedFixture, l credpool.Limits) {
	t.Helper()
	ctx := context.Background()
	p, err := f.pledges.Get(ctx, gatewayEntryPledge())
	if err != nil {
		t.Fatalf("get gateway pledge: %v", err)
	}
	p.Limits = l
	if err := f.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert gateway pledge: %v", err)
	}
}

// W22 (Y-1): a team_gateway run its attached pool's gateway entry funds keeps
// the entry through a resume the RunID-blind preview reads the entry
// unavailable for — its own dead attempt's lease still open on a pledge that
// serves one run at a time, its own run unit spent on a pledge that serves
// one run a day: the frozen stamp attributes the routes, the walk's RunID-aware
// draw renews the run's own admission, and the entry seals with the ambient
// refusal. An entry another run holds is a real failure: the resume is
// refused by name (the boundary's strict arm), never published dead.
func TestGatewayKey_AResumeOfAPoolFundedTeamGatewayRunDrawsItsOwnEntry(t *testing.T) {
	mixed := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}},
		}}
	}
	for _, row := range []struct {
		name    string
		limits  credpool.Limits
		wf      func() *ir.Workflow
		report  bool
		other   bool
		refused bool
	}{
		{"one run at a time, the dead attempt's lease still open", credpool.Limits{MaxConcurrentRuns: 1}, gatewayOnlyWorkflow, false, false, false},
		{"one run a day, the attempt reported", credpool.Limits{MaxRunsPerDay: 1}, gatewayOnlyWorkflow, true, false, false},
		{"a mixed run, one run at a time, the dead attempt's lease still open", credpool.Limits{MaxConcurrentRuns: 1}, mixed, false, false, false},
		{"another run holds the entry's one slot", credpool.Limits{MaxConcurrentRuns: 1}, gatewayOnlyWorkflow, true, true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAttachedFixture(t)
			limitGatewayEntry(t, f, row.limits)
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, f.sealer, poolTeam, secrets.ProviderAnthropic, "sk-ant-team")
			f.pub.apiKeys = keys
			var buf bytes.Buffer
			f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
			ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
			att := frozenAttachment("pool-att")
			resolve := func(runID string, b boundarySource) (credResolution, error) {
				wf := row.wf()
				return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, b)
			}
			launch, err := resolve("run-gw-22", boundaryDerived)
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			if !launch.contentBoundary.TeamGateway() || bundleFrom(t, f.pub, ctx, launch, "run-gw-22").APIKeys[secrets.ProviderOpenAICompatible] != "sk-gw-0000" {
				t.Fatalf("fixture: the launch is not funded by the entry under team_gateway: boundary=%+v (log:\n%s)", launch.contentBoundary, buf.String())
			}
			if row.report {
				if err := f.pub.credPool.Report(context.Background(), "run-gw-22", credpool.Outcome{CostUSD: 0.01}); err != nil {
					t.Fatalf("report: %v", err)
				}
			}
			if row.other {
				if _, err := resolve("run-gw-22-other", boundaryDerived); err != nil {
					t.Fatalf("the other run's launch: %v", err)
				}
				if open := openLeases(t, f, "run-gw-22-other"); len(open) != 1 {
					t.Fatalf("fixture: the other run holds %d leases, want the entry's one slot", len(open))
				}
			}
			buf.Reset()
			resumed, err := resolve("run-gw-22", boundaryFrozen(launch.contentBoundary))
			if row.refused {
				var refusal *UnfundedProvidersError
				if !errors.As(err, &refusal) || !refusal.TeamGateway || !slices.Equal(refusal.Providers, []string{string(secrets.ProviderOpenAICompatible)}) {
					t.Fatalf("resume = %v, want the boundary's refusal naming the gateway route the entry cannot serve (log:\n%s)", err, buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("resume: %v (log:\n%s)", err, buf.String())
			}
			b := bundleFrom(t, f.pub, ctx, resumed, "run-gw-22")
			if b.APIKeys[secrets.ProviderOpenAICompatible] != "sk-gw-0000" || !b.AmbientLLMRefused || resumed.contentBoundary != launch.contentBoundary {
				t.Fatalf("resume: gateway sealed=%v ambient=%v boundary=%+v, want the entry sealed under the frozen team_gateway stamp (log:\n%s)",
					b.APIKeys[secrets.ProviderOpenAICompatible] != "", b.AmbientLLMRefused, resumed.contentBoundary, buf.String())
			}
			if strings.Contains(buf.String(), envFundedLine) {
				t.Fatalf("the resume read the run env-funded (log:\n%s)", buf.String())
			}
		})
	}
}

// varsVendorDefaultGatewayWorkflow routes its one node on {{vars.model}}, whose declared
// default is a vendor route.
func varsVendorDefaultGatewayWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name: "wf",
		Vars: map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "anthropic/claude-opus-5"}},
		Nodes: map[string]ir.Node{
			"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "{{vars.model}}"}, Tools: []string{}},
		},
	}
}

// replayPublisher is the gateway publisher with a run store and a capturing
// publication, the team holding a gateway key and an anthropic key.
func replayPublisher(t *testing.T) (*Publisher, *bytes.Buffer, *store.FilesystemRunStore, *[]*queue.RunMessage, string) {
	t.Helper()
	p, buf := gatewayKeyPublisher(t)
	dir := t.TempDir()
	rs, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	p.store = rs
	published := &[]*queue.RunMessage{}
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error { *published = append(*published, m); return nil }
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	return p, buf, rs, published, dir
}

// messageBundle opens the bundle a published message carries.
func messageBundle(t *testing.T, p *Publisher, m *queue.RunMessage, runID string) secrets.RunBundle {
	t.Helper()
	if m.SecretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := p.runSecrets.Get(store.WithTenant(context.Background(), "team1"), m.SecretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(mustDEKSealer(t, m.BundleDEK), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return b
}

// pauseForResume parks a run with a checkpoint at node.
func pauseForResume(t *testing.T, rs *store.FilesystemRunStore, ctx context.Context, runID, node string) {
	t.Helper()
	run, err := rs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusFailedResumable
	run.Checkpoint = &store.Checkpoint{NodeID: node, ArtifactsKnown: true}
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

const heldOutLine = "a replay seals none its launch did not"

// W23 (X-1): a replay never seals a gateway key its launch did not. A launch
// reading an untrusted launch var late leaves `{{vars.model}}` undecided
// (own_keys, nothing sealed for the gateway on the default runner); its
// resume reads the run's inputs as decided — the gateway route stays the
// environment's, as at launch, named; the trusted control is team_gateway on
// both. A fork whose new inputs route a vendor node to the gateway replays
// the parent's own_keys stamp: its first resume seals no gateway key either.
func TestGatewayKey_AReplayNeverSealsAGatewayKeyItsLaunchDidNot(t *testing.T) {
	const src = "workflow wf:\n  start -> done\n"
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "a launch var read late", true: "the trusted control"}[trusted], func(t *testing.T) {
			p, buf, rs, published, _ := replayPublisher(t)
			ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", "owner1"), gwRepo)
			spec := runview.LaunchSpec{FilePath: "wf.bot", Source: src, BotID: gwBot, ProjectPath: gwRepo,
				Vars: map[string]string{"model": "openai_compatible/team/m"}, LLMRoutePolicy: ladderPolicy()}
			if trusted {
				spec.TrustedVarNames = []string{"model"}
			}
			if _, err := p.SubmitLaunch(ctx, "run-gw-23", spec, varsVendorDefaultGatewayWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
				t.Fatalf("SubmitLaunch: %v (log:\n%s)", err, buf.String())
			}
			launch := (*published)[len(*published)-1]
			launchSealed := messageBundle(t, p, launch, "run-gw-23").APIKeys[secrets.ProviderOpenAICompatible] != ""
			if launchSealed != trusted || launch.ContentBoundary.TeamGateway() != trusted {
				t.Fatalf("fixture: launch gateway sealed=%v boundary=%+v, want both %v", launchSealed, launch.ContentBoundary, trusted)
			}
			pauseForResume(t, rs, ctx, "run-gw-23", "gw")
			buf.Reset()
			if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: "run-gw-23", FilePath: "wf.bot", Source: src}, varsVendorDefaultGatewayWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
				t.Fatalf("SubmitResume: %v (log:\n%s)", err, buf.String())
			}
			resume := (*published)[len(*published)-1]
			if got := messageBundle(t, p, resume, "run-gw-23").APIKeys[secrets.ProviderOpenAICompatible] != ""; got != trusted {
				t.Fatalf("resume sealed the gateway key = %v, want %v — the launch's: a replay never moves a run onto a key its launch left to the environment (log:\n%s)", got, trusted, buf.String())
			}
			if named := strings.Contains(buf.String(), heldOutLine); named == trusted {
				t.Fatalf("hold-out line present = %v, want %v (log:\n%s)", named, !trusted, buf.String())
			}
		})
	}
	t.Run("a fork re-routing a vendor node to the gateway", func(t *testing.T) {
		p, buf, rs, published, dir := replayPublisher(t)
		ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", "owner1"), gwRepo)
		if _, err := p.SubmitLaunch(ctx, "run-gw-23p", runview.LaunchSpec{FilePath: "wf.bot", Source: src, BotID: gwBot, ProjectPath: gwRepo, LLMRoutePolicy: ladderPolicy()},
			varsVendorDefaultGatewayWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitLaunch: %v (log:\n%s)", err, buf.String())
		}
		if parent := (*published)[len(*published)-1]; parent.ContentBoundary.TeamGateway() || messageBundle(t, p, parent, "run-gw-23p").APIKeys[secrets.ProviderOpenAICompatible] != "" {
			t.Fatalf("fixture: the vendor parent is %+v with a gateway key sealed", parent.ContentBoundary)
		}
		parent, err := rs.LoadRun(ctx, "run-gw-23p")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		parent.Status = store.RunStatusCancelled
		parent.Checkpoint = &store.Checkpoint{NodeID: "gw", ArtifactsKnown: true}
		if err := rs.SaveRun(ctx, parent); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if err := rs.WriteTurn(ctx, &store.TurnCheckpoint{RunID: "run-gw-23p", NodeID: "gw", Backend: "claw", FinishReason: "tool_use",
			MessagesRef: "gw/0/0.messages.json", Messages: json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`), WrittenAt: time.Now().UTC()}); err != nil {
			t.Fatalf("WriteTurn: %v", err)
		}
		svc, err := runview.NewService(dir, runview.WithLogger(iterlog.Nop()))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		res, err := svc.Fork(ctx, runview.ForkSpec{RunID: "run-gw-23p", NodeID: "gw", TurnIndex: -1, NewInputs: map[string]any{"model": "openai_compatible/team/m"}})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		buf.Reset()
		if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: res.NewRunID, FilePath: "wf.bot", Source: src}, varsVendorDefaultGatewayWorkflow(), &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitResume of the fork child: %v (log:\n%s)", err, buf.String())
		}
		child := (*published)[len(*published)-1]
		if messageBundle(t, p, child, res.NewRunID).APIKeys[secrets.ProviderOpenAICompatible] != "" || child.ContentBoundary.TeamGateway() {
			t.Fatalf("the fork child sealed the gateway key under %+v: its parent's launch sealed none (log:\n%s)", child.ContentBoundary, buf.String())
		}
		if !strings.Contains(buf.String(), heldOutLine) {
			t.Fatalf("the fork child's hold-out is not named (log:\n%s)", buf.String())
		}
	})
	t.Run("an env dial under a runner pool, whose launch sealed the pair", func(t *testing.T) {
		dial := func() *ir.Workflow {
			return &ir.Workflow{Nodes: map[string]ir.Node{
				"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${W23_UNSET_ROUTE_DIAL:-openai_compatible/scaleway/deepseek-v4.1-flash}"}},
			}}
		}
		p, buf := gatewayKeyPublisher(t)
		seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
		resolve := func(b boundarySource) credResolution {
			creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-23d", "", "team1", "owner1", gwBot, "pool-a", dial(), nil, nil, model.RouteView{Workflow: dial()}, nil, store.RunTrustDefault, nil, nil, nil, b)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v (log:\n%s)", err, buf.String())
			}
			return creds
		}
		launch := resolve(boundaryDerived)
		if launch.contentBoundary.TeamGateway() || launch.contentBoundary.KeyID == "" || launch.contentBoundary.GatewayHost != gwHost {
			t.Fatalf("launch stamp = %+v, want own_keys naming the gateway pair it sealed", launch.contentBoundary)
		}
		resumed := resolve(boundaryFrozen(launch.contentBoundary))
		if openGatewayBundleOn(t, p, "run-gw-23d", "pool-a", resumed).APIKeys[secrets.ProviderOpenAICompatible] == "" {
			t.Fatalf("the resume of a launch that sealed the pair seals none (log:\n%s)", buf.String())
		}
	})
}

// W24 (Y-1's probe half, Y-4, Y-6): a gateway route attributed to a key no
// tier then seals rides the runner's environment gateway, named, and no
// refusal counts it. The attached pool's entry is attributed whatever the
// attachment's draw cap says (the probe reads existence and audience): the
// second launch on a spent cap draws nothing, the deployment knob does not
// refuse it, the launch names the entry, and the preview says the same — never
// that no tier holds a gateway key. A platform key the walk does not consult
// once a pool grant serves the run is named the same way.
func TestGatewayKey_AnAttributedRouteNoTierSealsIsNamedAndLeftToTheEnvironment(t *testing.T) {
	t.Run("an attached entry the draw cap holds, knob on", func(t *testing.T) {
		f := newAttachedFixture(t)
		f.pub.requireLLMCredential = true
		var buf bytes.Buffer
		f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
		att := frozenAttachment("pool-att")
		att.Limits.MaxRunsPerDay = 1
		ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
		resolve := func(runID string) (credResolution, error) {
			wf := gatewayOnlyWorkflow()
			return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, boundaryDerived)
		}
		if _, err := resolve("run-gw-24a"); err != nil {
			t.Fatalf("first launch: %v", err)
		}
		buf.Reset()
		second, err := resolve("run-gw-24b")
		if err != nil {
			t.Fatalf("second launch = %v, want it launched: the entry's routes ride the runner's gateway when the draw serves none (log:\n%s)", err, buf.String())
		}
		if second.contentBoundary.TeamGateway() || len(second.grants) != 0 {
			t.Fatalf("fixture: second launch boundary=%+v grants=%d, want own_keys and nothing drawn", second.contentBoundary, len(second.grants))
		}
		const named = `the attached pool pool-att's gateway entry "pliage-deepseek" (key-gw) attributed them, but its pool's draw served no entry now`
		if !strings.Contains(buf.String(), named) {
			t.Fatalf("the launch does not name the entry the draw could not serve (log:\n%s)", buf.String())
		}
		spec := runviewPreviewSpecAttached()
		spec.PoolAttachment = att
		preview, perr := previewReadOnly(f.pub).PreviewCredentials(ctx, spec, gatewayOnlyWorkflow())
		if perr != nil {
			t.Fatalf("preview: %v", perr)
		}
		if w := previewWarning(preview, "no credential tier this launch consults holds a gateway key"); w != "" {
			t.Fatalf("the preview claims no tier holds a gateway key while the attached pool's entry does: %s", w)
		}
		if previewWarning(preview, named) == "" || previewWarning(preview, "The live launch refuses") != "" {
			t.Fatalf("the preview does not mirror the launch (the entry named, no refusal): %v", preview.Warnings)
		}
	})
	for _, row := range []struct {
		name  string
		shape func(*credpool.Pledge)
	}{
		{"a paused entry lends nothing", func(p *credpool.Pledge) { p.Enabled = false }},
		{"an unhealthy entry lends nothing", func(p *credpool.Pledge) { p.Health = credpool.HealthAuthFailed }},
		{"an entry its pledge's bot filter refuses lends nothing", func(p *credpool.Pledge) { p.Bots = []string{"review-pr"} }},
		// The eligibility filter reports a cooldown or closed sharing hours
		// before the bot filter: the probe reads the verdict no clock clears.
		{"an entry its pledge's bot filter refuses lends nothing while it cools", func(p *credpool.Pledge) {
			p.Bots, p.CooldownUntil = []string{"review-pr"}, coolingFor(time.Hour)
		}},
		{"an entry its pledge's bot filter refuses lends nothing out of its sharing hours", func(p *credpool.Pledge) {
			p.Bots, p.Window = []string{"review-pr"}, closedWindow()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAttachedFixture(t)
			pl, err := f.pledges.Get(context.Background(), gatewayEntryPledge())
			if err != nil {
				t.Fatalf("get gateway pledge: %v", err)
			}
			row.shape(&pl)
			if err := f.pledges.Upsert(context.Background(), pl); err != nil {
				t.Fatalf("upsert gateway pledge: %v", err)
			}
			var buf bytes.Buffer
			f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
			ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
			wf := gatewayOnlyWorkflow()
			if _, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-24s", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, frozenAttachment("pool-att"), boundaryDerived); err != nil {
				t.Fatalf("launch: %v", err)
			}
			if !strings.Contains(buf.String(), envFundedLine) {
				t.Fatalf("an entry the pool's eligibility refuses attributed the gateway route (log:\n%s)", buf.String())
			}
		})
	}
	t.Run("a busy entry whose key row was deleted lends nothing", func(t *testing.T) {
		f := newAttachedFixture(t)
		limitGatewayEntry(t, f, credpool.Limits{MaxConcurrentRuns: 1})
		var buf bytes.Buffer
		f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
		ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
		resolve := func(runID string) (credResolution, error) {
			wf := gatewayOnlyWorkflow()
			return f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, frozenAttachment("pool-att"), boundaryDerived)
		}
		if _, err := resolve("run-gw-24h"); err != nil {
			t.Fatalf("the holding run's launch: %v", err)
		}
		if err := f.apiKeys.Delete(store.WithTenant(context.Background(), poolTeam), "key-gw"); err != nil {
			t.Fatalf("delete the entry's key row: %v", err)
		}
		buf.Reset()
		if _, err := resolve("run-gw-24d"); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if !strings.Contains(buf.String(), envFundedLine) || strings.Contains(buf.String(), "could not be read") {
			t.Fatalf("an entry whose key row is gone is read as holding a gateway key: the run must be env-funded with no unread note (log:\n%s)", buf.String())
		}
	})
	t.Run("a platform key while a pool grant serves the run", func(t *testing.T) {
		f := newAttachedFixture(t)
		if err := f.pledges.Delete(context.Background(), gatewayEntryPledge()); err != nil {
			t.Fatalf("delete the gateway entry: %v", err)
		}
		keys := secrets.NewMemoryApiKeyStore()
		var platID string
		seedGatewayKey(t, keys, f.sealer, secrets.PlatformTenantID, func(k *secrets.ApiKey) { platID = k.ID })
		f.pub.apiKeys = keys
		var buf bytes.Buffer
		f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
		wf := &ir.Workflow{Nodes: map[string]ir.Node{
			"gw":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"draft": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "draft"}, LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5", Provider: "anthropic"}, Tools: []string{}},
		}}
		ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
		creds, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-24p", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, frozenAttachment("pool-att"), boundaryDerived)
		if err != nil {
			t.Fatalf("launch: %v (log:\n%s)", err, buf.String())
		}
		if creds.grant == nil || bundleFrom(t, f.pub, ctx, creds, "run-gw-24p").APIKeys[secrets.ProviderOpenAICompatible] != "" {
			t.Fatalf("fixture: grant=%v, want the forfait granted and no gateway key sealed", creds.grant)
		}
		if named := `the platform tier's gateway key "team gateway" (` + platID + `) attributed them, but the platform tier is not consulted once a pool grant serves the run`; !strings.Contains(buf.String(), named) {
			t.Fatalf("the launch does not name the platform key its walk never consulted (log:\n%s)", buf.String())
		}
	})
}

// W25 (Y-3): a preview that knows no repository of a decided gateway program
// whose only gateway key is the attached pool's entry, its row restricted to
// repositories, does not state that no tier holds a gateway key: its
// env-funded answer is conditional, naming the entry, which it lists as the
// pool's repo_filtered candidate. The control: a preview naming the entry's
// repository selects it.
func TestGatewayKey_ARepoLessPreviewOfARepoRestrictedAttachedEntryIsConditional(t *testing.T) {
	f := newAttachedFixture(t)
	repos := []string{gwRepo}
	if _, err := f.apiKeys.Patch(store.WithTenant(context.Background(), poolTeam), "key-gw", secrets.ApiKeyPatch{Repos: &repos}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	preview, err := previewReadOnly(f.pub).PreviewCredentials(store.WithTenant(context.Background(), poolTeam), runviewPreviewSpecAttached(), gatewayOnlyWorkflow())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if w := previewWarning(preview, "no credential tier this launch consults holds a gateway key serving it"); w != "" {
		t.Fatalf("the repo-less preview states unconditionally that no tier holds a gateway key: %q", w)
	}
	if previewWarning(preview, `the attached pool pool-att's gateway entry "pliage-deepseek" (key-gw) is restricted to repositories this preview does not know`) == "" {
		t.Fatalf("the preview does not say its env-funded answer depends on the launch's repository: %v", preview.Warnings)
	}
	if !slices.ContainsFunc(preview.Candidates, func(c runview.CredentialPreviewCandidate) bool {
		return c.Tier == "pool" && c.Provider == string(secrets.ProviderOpenAICompatible) && c.State == string(credpool.StatusRepoFiltered) && !c.Selected
	}) {
		t.Fatalf("the repo-restricted entry is not listed as the pool's repo_filtered candidate: %+v", preview.Candidates)
	}
	control, err := previewReadOnly(f.pub).PreviewCredentials(secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo), runviewPreviewSpecAttached(), gatewayOnlyWorkflow())
	if err != nil {
		t.Fatalf("preview (control): %v", err)
	}
	if got := selectedDescriptions(control); !slices.Contains(got, "pool:api_key:openai_compatible") {
		t.Fatalf("control: a preview naming the entry's repository selected %v, want the entry", got)
	}
}
