// ADR-131 §5, the probe's half of "the slot can seal" (A4 and B3 of the
// PR-C' round-1 review): a gateway route is attributed to a key only where
// that key can actually seal for the run — the run's frozen lists admit the
// gateway slot and the key opens. A held key that cannot seal leaves the
// gateway routes to the runner's environment, said by name; the sovereign
// posture keeps attributing them (it owes them a key of its own), and every
// refusal sentence on that path names the held key and why it did not seal.
//
// Mutants these witnesses red on, each run and restored: the probe not asking
// the frozen lists → W15; asking them without noting the slot on the §5
// record → W15 and W6; the probe not opening the held key → W16's two
// undecryptable rows; the refusal without the held-key note → W16's sovereign
// and degraded rows; the env-funded line without it → W15 and W16's first
// row. The launch's unfunded posture blind to the team_gateway boundary →
// W17, W17b; the preview's → W17's preview assert; the boundary's arm after
// the knob → W17b. The preview's probe told it knows the repository → W18;
// the repo-restricted key left unlisted → W18's candidate assert. The
// boundary's strict arm read over the whole attributed set → W20's three rows;
// the launch's tolerated warning, or the preview's, silenced → W20. The
// broker's Preview blind to the attachment's draw cap, or the shared request
// without the caps → W21.

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

// seedUndecryptableGatewayKey stores a team gateway key whose sealed secret
// was bound to one base URL while the row names another — the store-level
// repoint the seal's AAD makes fail closed: OpenApiKey refuses it.
func seedUndecryptableGatewayKey(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: teamID, ScopeTeamID: teamID,
		Provider: secrets.ProviderOpenAICompatible, Name: "team gateway (repointed)",
		BaseURL: gwURL, Fingerprint: gwFingerprint, CreatedAt: time.Now().UTC(),
	}
	sealed, err := secrets.SealAPIKeyFor(sealer, k, []byte(gwTeamKey))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	k.SealedSecret = sealed
	k.BaseURL = "https://repointed.example"
	if _, err := secrets.OpenApiKey(sealer, k); err == nil {
		t.Fatal("fixture: the repointed row still opens")
	}
	if err := st.Create(context.Background(), k); err != nil {
		t.Fatalf("create: %v", err)
	}
	return k
}

// failingScopeKeys fails the key list of one scope, every other read passing
// through: a degraded tier.
type failingScopeKeys struct {
	secrets.ApiKeyStore
	scope string
}

func (s failingScopeKeys) ListByTeam(ctx context.Context, teamID, userID string) ([]secrets.ApiKey, error) {
	if teamID == s.scope {
		return nil, errors.New("key list: connection reset")
	}
	return s.ApiKeyStore.ListByTeam(ctx, teamID, userID)
}

// W15 (A4): a team gateway key the run's FROZEN pair order holds out is not
// attributed — the run is env-funded exactly as ADR-131 §5's replay clause
// says, on the launch and on the resume of a pre-upgrade run alike, so the
// deployment knob (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL) does not refuse it
// claiming no tier holds a key. The env-funded line names the held key and
// the list that held it out, and the §5 line still appears once.
func TestGatewayKey_AKeyTheFrozenListsHoldOutIsNotAttributed(t *testing.T) {
	for _, row := range []struct {
		name     string
		boundary boundarySource
	}{
		{"launch under an order that names no gateway pair", boundaryDerived},
		{"resume of a pre-upgrade run", boundaryFrozen(nil)},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			p.requireLLMCredential = true
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-15", "", "team1", "owner1", gwBot, "", gatewayOnlyWorkflow(), nil, nil, model.RouteView{Workflow: gatewayOnlyWorkflow()}, nil, store.RunTrustDefault, nil, preUpgradePolicy(), nil, row.boundary)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials = %v, want nil — a key the frozen list holds out leaves the routes to the pod's gateway (ADR-131 §5)", err)
			}
			if got := openGatewayBundle(t, p, "run-gw-15", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != "" {
				t.Fatalf("gateway slot = %q under a frozen order that names no gateway pair", got)
			}
			log := buf.String()
			if !strings.Contains(log, envFundedLine) || !strings.Contains(log, "team gateway") || !strings.Contains(log, "frozen routing pair order names no pair for the openai_compatible_key slot") {
				t.Fatalf("the env-funded line does not name the held key and the list that held it out:\n%s", log)
			}
			if got := strings.Count(log, heldOutMarker); got != 1 {
				t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
			}
			if row.boundary.replay {
				return
			}
			// The preview reads the same probe: the run is env-funded, the
			// key named, and no refusal is mirrored.
			spec := previewSpec("team1", "owner1")
			spec.LLMRoutePolicy = preUpgradePolicy()
			preview, err := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), spec, gatewayOnlyWorkflow())
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			named := false
			for _, w := range preview.Warnings {
				if strings.Contains(w, "live launch refuses") {
					t.Fatalf("the preview mirrors a refusal the launch does not make: %q", w)
				}
				if strings.Contains(w, "rides the runner's openai_compatible gateway") && strings.Contains(w, "frozen routing pair order names no pair") {
					named = true
				}
			}
			if !named {
				t.Fatalf("the preview does not name the held-out key: %v", preview.Warnings)
			}
		})
	}
}

// W16 (B3): a held gateway key that cannot seal — its sealed secret does not
// open, or its tier's list could not be read — is named wherever the run's
// fate is said. An undecryptable key is not attributed (the routes ride the
// pod's gateway, named; before the walk spent any key, knob on or off), the
// sovereign posture still attributes and refuses naming the key and why, and
// a tier read as holding because its list failed refuses (knob on) naming the
// tier, never "no tier holds a credential".
func TestGatewayKey_AHeldKeyThatCannotSealIsNamed(t *testing.T) {
	t.Run("undecryptable, knob on and off: env-funded, named", func(t *testing.T) {
		for _, knob := range []bool{false, true} {
			p, buf := gatewayKeyPublisher(t)
			p.requireLLMCredential = knob
			k := seedUndecryptableGatewayKey(t, p.apiKeys, p.sealer, "team1")
			if _, err := resolveGatewayRun(t, p, "run-gw-16a", gatewayOnlyWorkflow(), nil); err != nil {
				t.Fatalf("knob=%v: resolveAndSealCredentials = %v, want nil — an undecryptable key leaves the routes to the pod's gateway", knob, err)
			}
			log := buf.String()
			if !strings.Contains(log, envFundedLine) || !strings.Contains(log, k.ID) || !strings.Contains(log, "does not open") {
				t.Fatalf("knob=%v: the env-funded line does not name the undecryptable key %s:\n%s", knob, k.ID, log)
			}
		}
	})
	t.Run("undecryptable, sovereign: refused, naming the key", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		k := seedUndecryptableGatewayKey(t, p.apiKeys, p.sealer, "team1")
		_, err := resolveGatewayRun(t, p, "run-gw-16b", gatewayOnlyWorkflow(), nil)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) || !refusal.Sovereign {
			t.Fatalf("resolveAndSealCredentials = %v, want the sovereign unfunded refusal", err)
		}
		if msg := err.Error(); !strings.Contains(msg, k.ID) || !strings.Contains(msg, "does not open") {
			t.Fatalf("refusal %q does not name the held key %s and why it did not seal", msg, k.ID)
		}
	})
	t.Run("degraded platform tier, knob on: refused, naming the tier", func(t *testing.T) {
		p, _ := gatewayKeyPublisher(t)
		p.requireLLMCredential = true
		p.apiKeys = failingScopeKeys{ApiKeyStore: p.apiKeys, scope: secrets.PlatformTenantID}
		_, err := resolveGatewayRun(t, p, "run-gw-16c", gatewayOnlyWorkflow(), nil)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) {
			t.Fatalf("resolveAndSealCredentials = %v, want the knob's unfunded refusal", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "platform tier") || !strings.Contains(msg, "could not be read") {
			t.Fatalf("refusal %q does not say the platform tier was read as holding a gateway key because its list failed", msg)
		}
	})
}

// W17 (B2 of the round-1 review): a MIXED run held to its team's gateway
// (team_gateway: AmbientLLMRefused armed, every environment fallback refused)
// whose vendor route nothing in the bundle funds can only die at the runner —
// the permissive knob's reason to tolerate an unfunded route ("the pod's env
// may fund it") is false for it. It is refused by name at launch, knob on or
// off, with the boundary's own sentence, and the preview mirrors the refusal.
func TestGatewayKey_ATeamGatewayRunsUnfundedVendorRouteIsRefusedByName(t *testing.T) {
	wf := mixedGatewayWorkflow()
	for _, knob := range []bool{false, true} {
		p, buf := gatewayKeyPublisher(t)
		p.requireLLMCredential = knob
		seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
		_, err := resolveGatewayRun(t, p, "run-gw-17", wf, nil)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) {
			t.Fatalf("knob=%v: resolveAndSealCredentials = %v, want the team_gateway run refused naming its unfunded vendor route (log:\n%s)", knob, err, buf.String())
		}
		if !refusal.TeamGateway || refusal.Sovereign || len(refusal.Providers) != 1 || refusal.Providers[0] != "anthropic" {
			t.Fatalf("knob=%v: refusal = %+v, want the team_gateway arm naming anthropic alone", knob, refusal)
		}
		if msg := err.Error(); !strings.Contains(msg, "team_gateway") || strings.Contains(msg, "unset ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL") {
			t.Fatalf("knob=%v: refusal %q must say the run is held to its gateway, never offer the environment the boundary refuses", knob, msg)
		}
		if knob {
			continue
		}
		preview, perr := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), previewSpec("team1", "owner1"), wf)
		if perr != nil {
			t.Fatalf("preview: %v", perr)
		}
		if want := "The live launch refuses: " + err.Error(); previewWarning(preview, want) == "" {
			t.Fatalf("the preview does not mirror the launch's refusal.\n want: %s\n got: %v", want, preview.Warnings)
		}
	}
}

// W17b: the boundary's arm outranks the knob's — a resume replaying
// team_gateway whose gateway key is gone since leaves EVERY attributed route
// unfunded, and the refusal still names the boundary rather than offering the
// environment fallback the replayed ambient refusal forbids.
func TestGatewayKey_AReplayedTeamGatewayRefusalNamesTheBoundaryOverTheKnob(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	p.requireLLMCredential = true
	wf := mixedGatewayWorkflow()
	frozen := boundaryFrozen(&store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: "key-gone", GatewayHost: "gw.example"})
	_, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-17b", "", "team1", "owner1", gwBot, "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, frozen)
	var refusal *UnfundedProvidersError
	if !errors.As(err, &refusal) || !refusal.TeamGateway {
		t.Fatalf("resolveAndSealCredentials = %v, want the team_gateway arm (log:\n%s)", err, buf.String())
	}
	if msg := err.Error(); strings.Contains(msg, "unset ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL") {
		t.Fatalf("refusal %q offers the environment fallback a team_gateway run is refused", msg)
	}
}

// W18 (B4 of the round-1 review): a preview that knows no repository (a
// personal source, a webhook whose allowlist is a pattern or several
// repositories) of a decided gateway program whose team gateway key is
// restricted to repositories does not state that no tier holds a key serving
// it — every launch on the key's repository seals it and is held to the
// team's gateway. The env-funded answer is conditional, and the key is listed
// with its repo_filtered reason. The control: a preview naming the key's
// repository selects it.
func TestGatewayKey_ARepoLessPreviewSaysTheEnvFundedAnswerIsConditional(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { k.Repos = []string{gwRepo} })
	preview, err := previewReadOnly(p).PreviewCredentials(store.WithTenant(context.Background(), "team1"), previewSpec("team1", "owner1"), gatewayOnlyWorkflow())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	conditional := false
	for _, w := range preview.Warnings {
		if strings.Contains(w, "no credential tier this launch consults holds a gateway key serving it") {
			t.Fatalf("the repo-less preview states unconditionally that no tier holds a gateway key: %q", w)
		}
		if strings.Contains(w, "team gateway") && strings.Contains(w, "repositories this preview does not know") {
			conditional = true
		}
	}
	if !conditional {
		t.Fatalf("the preview does not say its env-funded answer depends on the launch's repository: %v", preview.Warnings)
	}
	listed := false
	for _, c := range preview.Candidates {
		if c.Provider == string(secrets.ProviderOpenAICompatible) && c.State == "repo_filtered" && strings.Contains(c.Reason, "none is known to this preview") && !c.Selected {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("the repo-restricted gateway key is not listed with its repo_filtered reason: %+v", preview.Candidates)
	}
	control, err := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), previewSpec("team1", "owner1"), gatewayOnlyWorkflow())
	if err != nil {
		t.Fatalf("preview (control): %v", err)
	}
	if got := selectedDescriptions(control); !slices.Contains(got, "team:api_key:openai_compatible") {
		t.Fatalf("control: a preview naming the key's repository selected %v, want the gateway key", got)
	}
}

// W19 (B6 and the C4 gap of the round-1 review): an ATTACHED pool's gateway
// entry serving the launch is a gateway key a tier the walk consults holds —
// the probe reads it like the team's own, through the entry's own audiences —
// so a decided gateway route it funds is attributed: the walk seals the entry
// and the run is held to the team's gateway (team_gateway, ambient refused,
// no vendor crossing on the gateway node), the mixed run as the gateway-only
// one, which is no longer left to the pod's gateway. An entry whose key row
// excludes the launch attributes nothing, and the preview reads the same.
func TestGatewayKey_AnAttachedPoolsGatewayEntryAttributesTheRoute(t *testing.T) {
	mixed := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"gw":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"draft": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "draft"}, LLMFields: ir.LLMFields{Backend: "claude_code"}},
		}}
	}
	for _, row := range []struct {
		name      string
		wf        func() *ir.Workflow
		policy    *store.RunLLMRoutePolicy
		excluded  bool
		gwCrosses bool
	}{
		{"mixed run, the entry serves the launch", mixed, ladderPolicy(), false, false},
		{"gateway-only run, the entry serves the launch", gatewayOnlyWorkflow, nil, false, false},
		{"gateway-only run, the entry's key row names another repository", gatewayOnlyWorkflow, nil, true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAttachedFixture(t)
			if row.excluded {
				other := []string{"acme/other"}
				if _, err := f.apiKeys.Patch(store.WithTenant(context.Background(), poolTeam), "key-gw", secrets.ApiKeyPatch{Repos: &other}); err != nil {
					t.Fatalf("patch: %v", err)
				}
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, f.sealer, poolTeam, secrets.ProviderOpenAI, "sk-openai-team")
			f.pub.apiKeys = keys
			var buf bytes.Buffer
			f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
			wf := row.wf()
			ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
			creds, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-19", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, row.policy, frozenAttachment("pool-att"), boundaryDerived)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			var b secrets.RunBundle
			if creds.secretsRef != "" {
				b = bundleFrom(t, f.pub, ctx, creds, "run-gw-19")
			}
			sealed := b.APIKeys[secrets.ProviderOpenAICompatible] != ""
			if sealed == row.excluded || creds.contentBoundary.TeamGateway() == row.excluded || b.AmbientLLMRefused == row.excluded {
				t.Fatalf("entry sealed=%v boundary=%+v ambient=%v, want the entry %s (log:\n%s)", sealed, creds.contentBoundary, b.AmbientLLMRefused,
					map[bool]string{false: "sealed under team_gateway", true: "left out and the run own_keys"}[row.excluded], buf.String())
			}
			if row.excluded && !strings.Contains(buf.String(), envFundedLine) {
				t.Fatalf("an entry its key row excludes attributed the gateway route: the run is not env-funded (log:\n%s)", buf.String())
			}
			if row.policy != nil && policyCrossing(wf, "gw") != row.gwCrosses {
				t.Fatalf("gateway node crossing = %v under a pool-funded team_gateway run", policyCrossing(wf, "gw"))
			}
			if row.policy != nil {
				return
			}
			preview, perr := previewReadOnly(f.pub).PreviewCredentials(ctx, runviewPreviewSpecAttached(), gatewayOnlyWorkflow())
			if perr != nil {
				t.Fatalf("preview: %v", perr)
			}
			if got := slices.Contains(selectedDescriptions(preview), "pool:api_key:openai_compatible"); got == row.excluded {
				t.Fatalf("preview selected %v, want the pool's gateway entry selected = %v", selectedDescriptions(preview), !row.excluded)
			}
		})
	}
}

// runviewPreviewSpecAttached is the preview spec of the attached fixture's
// launch: its team, its requester, the frozen attachment.
func runviewPreviewSpecAttached() runview.CredentialPreviewSpec {
	spec := previewSpec(poolTeam, "requester")
	spec.PoolAttachment = frozenAttachment("pool-att")
	return spec
}

// W20 (Y-2 of the round-2 review): the content boundary's strict arm refuses
// a team_gateway run for a node's DECIDED primary route nothing funds (W17),
// never for a provider only a rescue route or an env dial names: a rescue
// fires only once its node's own route failed, and the runner's pod expands
// a `${DIAL:-vendor/…}` with its own environment — a runner pool's to the
// gateway the sealed pair funds. The run launches team_gateway, the launch
// warns naming the provider and its site, and the preview mirrors the warning
// instead of a refusal.
func TestGatewayKey_ATeamGatewayRunsRescueAndEnvDialRoutesAreWarnedNotRefused(t *testing.T) {
	envDial := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"gw":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "${W20_UNSET_DIAL_BACKEND:-claw}", Model: "${W20_UNSET_DIAL_MODEL:-anthropic/claude-opus-5}"}, Tools: []string{}},
		}}
	}
	rescue := func() *ir.Workflow {
		return &ir.Workflow{Nodes: map[string]ir.Node{
			"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{},
				Fallbacks: []ir.Fallback{{Name: "rescue", Backend: "claw", Model: "openai/gpt-6-sol"}}},
		}}
	}
	for _, row := range []struct {
		name       string
		wf         func() *ir.Workflow
		runnerPool string
		anthropic  bool
		tolerated  string
	}{
		{"an env dial under a runner pool", envDial, "pool-a", false, "anthropic (named by review)"},
		{"an env dial on the default runner", envDial, "", false, "anthropic (named by review)"},
		{"a rescue route", rescue, "", true, "openai (named by review#rescue)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			if row.anthropic {
				seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
			}
			wf := row.wf()
			creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-20", "", "team1", "owner1", gwBot, row.runnerPool, wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials = %v, want the team_gateway run launched (log:\n%s)", err, buf.String())
			}
			b := openGatewayBundleOn(t, p, "run-gw-20", row.runnerPool, creds)
			if !creds.contentBoundary.TeamGateway() || b.APIKeys[secrets.ProviderOpenAICompatible] == "" || !b.AmbientLLMRefused {
				t.Fatalf("fixture: boundary=%+v gateway sealed=%v ambient=%v, want a team_gateway launch", creds.contentBoundary, b.APIKeys[secrets.ProviderOpenAICompatible] != "", b.AmbientLLMRefused)
			}
			if !strings.Contains(buf.String(), "no credential it carries funds "+row.tolerated) {
				t.Fatalf("the launch does not name the tolerated %s (log:\n%s)", row.tolerated, buf.String())
			}
			if row.runnerPool != "" {
				return
			}
			preview, perr := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), previewSpec("team1", "owner1"), row.wf())
			if perr != nil {
				t.Fatalf("preview: %v", perr)
			}
			if w := previewWarning(preview, "The live launch refuses"); w != "" {
				t.Fatalf("the preview predicts a refusal the launch does not make: %s", w)
			}
			if previewWarning(preview, "no credential it carries funds "+row.tolerated) == "" {
				t.Fatalf("the preview does not mirror the launch's warning naming %s: %v", row.tolerated, preview.Warnings)
			}
		})
	}
}

// W21 (Y-6 of the round-2 review): the credential preview's attached-pool
// read is the walk's request — the attachment's draw scope and caps ride it
// (attachedPoolRequest) and the broker's Preview judges them — so an
// attachment whose runs-per-day draw cap is spent selects no entry, as the
// walk draws none. Before, the preview selected the gateway entry the walk
// then refused (errScopeDrawCapped).
func TestGatewayKey_ThePreviewReadsTheAttachmentsDrawCap(t *testing.T) {
	f := newAttachedFixture(t)
	att := frozenAttachment("pool-att")
	att.Limits.MaxRunsPerDay = 1
	ctx := secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
	wf := gatewayOnlyWorkflow()
	if _, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-21", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att, boundaryDerived); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if open := openLeases(t, f, "run-gw-21"); len(open) != 1 {
		t.Fatalf("fixture: the first launch drew %d leases, want the gateway entry's one (the draw cap's single run)", len(open))
	}
	spec := runviewPreviewSpecAttached()
	spec.PoolAttachment = att
	preview, err := previewReadOnly(f.pub).PreviewCredentials(ctx, spec, gatewayOnlyWorkflow())
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := selectedDescriptions(preview); slices.Contains(got, "pool:api_key:openai_compatible") {
		t.Fatalf("the preview selects the gateway entry of an attachment whose draw cap is spent (selected %v): the walk draws none", got)
	}
	exhausted := false
	for _, c := range preview.Candidates {
		if c.Tier == "pool" && c.Provider == string(secrets.ProviderOpenAICompatible) && c.State == string(credpool.StatusExhausted) && strings.Contains(c.Reason, "draw cap") {
			exhausted = true
		}
	}
	if !exhausted {
		t.Fatalf("the gateway entry is not shown exhausted by the attachment's draw cap: %+v", preview.Candidates)
	}
}
