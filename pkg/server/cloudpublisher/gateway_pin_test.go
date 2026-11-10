// ADR-131 §6, the launch half of the gateway pin (#2005 PR-C'): a pin on an
// openai_compatible key is exclusive. The write admits it like any key of the
// team (pkg/server's TestValidateKeyOverrides_AdmitsAGatewayKeyPin); at the
// walk, the gateway slot holds the very key it names, or the launch is
// refused by name — never served by the team's other gateway key, a shared
// tier, or the runner pod's environment. A vendor pin keeps its warn-only
// contract, and a run that rides no gateway route leaves the pin idle.

package cloudpublisher

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pinActor is the synthetic identity a webhook launch resolves credentials
// under: it sees the team's org-scoped keys and no user's.
const pinActor = "webhook:wh-gw"

// seedPinKey seals one team key under its own secret (two keys of one
// provider must be told apart in the bundle) the way the key routes store it:
// a gateway key binds its canonical URL into the seal and carries the
// endpoint-aware fingerprint. shape adjusts the row before the seal; repoint,
// when set, moves the stored URL after it — a row repointed in the store
// opens nowhere (the AAD binds the sealed URL).
func seedPinKey(t *testing.T, p *Publisher, prov secrets.Provider, secret string, shape func(*secrets.ApiKey), repoint string) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: "team1", ScopeTeamID: "team1",
		Provider: prov, Name: string(prov) + " " + secret,
		Fingerprint: secrets.APIKeyFingerprint(prov, secret, ""), CreatedAt: time.Now().UTC(),
	}
	if prov == secrets.ProviderOpenAICompatible {
		k.BaseURL = gwURL
		k.Fingerprint = secrets.APIKeyFingerprint(prov, secret, gwURL)
	}
	if shape != nil {
		shape(&k)
	}
	sealed, err := secrets.SealAPIKeyFor(p.sealer, k, []byte(secret))
	if err != nil {
		t.Fatalf("seal %s key: %v", prov, err)
	}
	k.SealedSecret = sealed
	if repoint != "" {
		k.BaseURL = repoint
	}
	if err := p.apiKeys.Create(t.Context(), k); err != nil {
		t.Fatalf("create %s key: %v", prov, err)
	}
	return k
}

// resolvePinnedRun is a webhook launch's walk: the synthetic actor, the
// launch's bot and repository, and the webhook's key_overrides.
func resolvePinnedRun(t *testing.T, p *Publisher, runID string, wf *ir.Workflow, pins map[string]string, policy *store.RunLLMRoutePolicy, boundary boundarySource) (credResolution, error) {
	t.Helper()
	return resolvePinnedRunOn(t, p, runID, "", wf, pins, policy, boundary)
}

// resolvePinnedRunOn is resolvePinnedRun for a launch mapped to a runner pool
// (D13) — "" is the default runner.
func resolvePinnedRunOn(t *testing.T, p *Publisher, runID, runnerPool string, wf *ir.Workflow, pins map[string]string, policy *store.RunLLMRoutePolicy, boundary boundarySource) (credResolution, error) {
	t.Helper()
	return p.resolveAndSealCredentials(gatewayLaunchCtx(), runID, "", "team1", pinActor, gwBot, runnerPool, wf, pins, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy, nil, boundary)
}

func gatewayPin(keyID string) map[string]string {
	return map[string]string{string(secrets.ProviderOpenAICompatible): keyID}
}

// seedOtherGatewayKey is the team's healthy default gateway key, created
// first: the key the walk would seal in a broken pin's place.
func seedOtherGatewayKey(t *testing.T, p *Publisher) secrets.ApiKey {
	t.Helper()
	return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-default", func(k *secrets.ApiKey) {
		k.IsDefault = true
		k.CreatedAt = time.Now().UTC().Add(-time.Hour)
	}, "")
}

// P1: a valid pin seals the key it names — over the team's default gateway
// key — and the run's content boundary names that key (ADR-131 §4).
func TestGatewayPin_TheNamedKeySealsAndStampsTheBoundary(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	seedOtherGatewayKey(t, p)
	pinned := seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "")
	creds, err := resolvePinnedRun(t, p, "run-pin-1", gatewayOnlyWorkflow(), gatewayPin(pinned.ID), nil, boundaryDerived)
	if err != nil {
		t.Fatalf("a valid gateway pin refused: %v", err)
	}
	b := openGatewayBundle(t, p, "run-pin-1", creds)
	if got := b.APIKeys[secrets.ProviderOpenAICompatible]; got != "sk-gw-pinned" {
		t.Fatalf("gateway slot = %q, want the pinned key's secret over the team's default (log:\n%s)", got, buf.String())
	}
	want := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: pinned.ID, GatewayHost: gwHost}
	if !reflect.DeepEqual(creds.contentBoundary, want) {
		t.Fatalf("content boundary = %+v, want %+v — the stamp names the pinned key", creds.contentBoundary, want)
	}
}

// P2: a gateway pin that did not fill the gateway slot refuses the launch by
// name — the pinned key's id and why — whether the walk would have sealed the
// team's other gateway key in its place, left the run to the pod's gateway, or
// sealed nothing for an attributed route. Each row is red without the
// chokepoint: the launch proceeds on something the operator did not name.
func TestGatewayPin_ABrokenPinRefusesTheLaunchByName(t *testing.T) {
	dial := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${C5_UNSET_ROUTE_DIAL:-openai_compatible/scaleway/deepseek-v4.1-flash}"}},
	}}
	missing := func(*testing.T, *Publisher) string { return secrets.NewApiKeyID() }
	for _, row := range []struct {
		name     string
		wf       *ir.Workflow
		pool     string
		policy   *store.RunLLMRoutePolicy
		resume   bool
		other    bool
		pinned   func(*testing.T, *Publisher) string
		cause    GatewayPinCause
		sentence string
	}{
		{name: "deleted, beside the team's other gateway key", other: true, pinned: missing,
			cause: GatewayPinMissing, sentence: "deleted since the pin was written"},
		{name: "deleted, the team's only gateway key — the pod's gateway would serve the run", pinned: missing,
			cause: GatewayPinMissing, sentence: "deleted since the pin was written"},
		{name: "withheld by its repository audience, beside the team's other gateway key", other: true,
			pinned: func(t *testing.T, p *Publisher) string {
				return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", func(k *secrets.ApiKey) { k.Repos = []string{"acme/other"} }, "").ID
			},
			cause: GatewayPinWithheld, sentence: "its repository audience [acme/other] does not name repository " + gwRepo},
		{name: "withheld by its bot audience, the team's only gateway key — the pod's gateway would serve the run",
			pinned: func(t *testing.T, p *Publisher) string {
				return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", func(k *secrets.ApiKey) { k.Bots = []string{"other-bot"} }, "").ID
			},
			cause: GatewayPinWithheld, sentence: "its workload audience [other-bot] does not name bot " + gwBot},
		{name: "undecryptable, beside the team's other gateway key", other: true,
			pinned: func(t *testing.T, p *Publisher) string {
				return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "https://gw2.example").ID
			},
			cause: GatewayPinUndecryptable, sentence: "its sealed secret does not open"},
		{name: "scoped to a user the launch's identity cannot see, beside the team's other gateway key", other: true,
			pinned: func(t *testing.T, p *Publisher) string {
				return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", func(k *secrets.ApiKey) { k.ScopeUserID = "someone" }, "").ID
			},
			cause: GatewayPinInvisible, sentence: `the launch's identity "` + pinActor + `" cannot spend it`},
		{name: "held out by the run's frozen routing pair order", policy: preUpgradePolicy(),
			pinned: func(t *testing.T, p *Publisher) string {
				return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "").ID
			},
			cause: GatewayPinHeldOut, sentence: "frozen routing pair order names no pair for the openai_compatible_key slot"},
		{name: "deleted, on an env-templated gateway dial the walk cannot decide, under a runner pool", wf: dial, pool: "pool-a", other: true, pinned: missing,
			cause: GatewayPinMissing, sentence: "deleted since the pin was written"},
		{name: "deleted, on a resume replaying team_gateway", resume: true, other: true, pinned: missing,
			cause: GatewayPinMissing, sentence: "deleted since the pin was written"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			if row.other {
				seedOtherGatewayKey(t, p)
			}
			pin := row.pinned(t, p)
			wf := row.wf
			if wf == nil {
				wf = gatewayOnlyWorkflow()
			}
			boundary := boundaryDerived
			if row.resume {
				boundary = boundaryFrozen(&store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: pin, GatewayHost: gwHost})
			}
			creds, err := resolvePinnedRunOn(t, p, "run-pin-2", row.pool, wf, gatewayPin(pin), row.policy, boundary)
			var refusal *GatewayPinRefusedError
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a *GatewayPinRefusedError — the launch proceeded on gateway slot %q, boundary %+v (log:\n%s)",
					err, openGatewayBundle(t, p, "run-pin-2", creds).APIKeys[secrets.ProviderOpenAICompatible], creds.contentBoundary, buf.String())
			}
			if refusal.KeyID != pin || refusal.Tenant != "team1" {
				t.Errorf("refusal names key %q of tenant %q, want key %q of team1", refusal.KeyID, refusal.Tenant, pin)
			}
			if refusal.Cause != row.cause {
				t.Errorf("refusal cause = %q (%s), want %q", refusal.Cause, refusal.Detail, row.cause)
			}
			msg := err.Error()
			for _, want := range []string{pin, row.sentence, "exclusive (ADR-131 §6)"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal %q does not say %q", msg, want)
				}
			}
			if creds.contentBoundary != nil && !row.resume {
				t.Errorf("a refused launch derived a content boundary %+v", creds.contentBoundary)
			}
		})
	}
}

// P2b: an attached pool's gateway entry is a substitute like any other — the
// walk seals it in a broken pin's place, and the launch is refused all the
// same. The refusal travels WITH the walk's resolution, so the pool grants it
// acquired reach the caller's release (SubmitLaunch and SubmitResume release
// every grant of a launch that does not happen); dropped, their leases would
// hold the donors' slots for the lease's whole life on a run that never ran.
func TestGatewayPin_ARefusalCarriesTheWalksPoolGrantsToTheRelease(t *testing.T) {
	f := newAttachedFixture(t)
	f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
	pin := secrets.NewApiKeyID()
	ctx := store.WithTenant(t.Context(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, "run-pin-pool", poolOrg, poolTeam, "requester", "docs-refresh", "", nil, gatewayPin(pin), nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil, frozenAttachment("pool-att"), boundaryDerived)
	var refusal *GatewayPinRefusedError
	if !errors.As(err, &refusal) || refusal.Cause != GatewayPinMissing {
		t.Fatalf("err = %v, want the missing pin refused while the attached pool's gateway entry filled its slot", err)
	}
	lent := false
	for _, g := range creds.grants {
		if g != nil && g.Ref == string(secrets.ProviderOpenAICompatible) {
			lent = true
		}
	}
	if !lent {
		t.Fatalf("grants carried with the refusal = %v, want the attached pool's gateway grant among them — the caller can only release what the refusal hands back", creds.grants)
	}
}

// P3: a gateway pin is idle on a run that rides no gateway route — the run
// has no use for the key, so neither a healthy pin nor a broken one acts —
// and the walk says nothing false about it: the gateway slot is out because
// no route rides it, not because a frozen list dropped the pin.
func TestGatewayPin_IsIdleOnARunThatRidesNoGatewayRoute(t *testing.T) {
	vendor := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-5"}},
	}}
	for _, row := range []struct {
		name   string
		pinned func(*testing.T, *Publisher) string
	}{
		{"a healthy gateway pin", func(t *testing.T, p *Publisher) string {
			return seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "").ID
		}},
		{"a deleted gateway pin", func(*testing.T, *Publisher) string { return secrets.NewApiKeyID() }},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			seedPinKey(t, p, secrets.ProviderAnthropic, "sk-ant-team", nil, "")
			pin := row.pinned(t, p)
			creds, err := resolvePinnedRun(t, p, "run-pin-3", vendor, gatewayPin(pin), nil, boundaryDerived)
			if err != nil {
				t.Fatalf("a vendor-only run refused over its idle gateway pin: %v", err)
			}
			b := openGatewayBundle(t, p, "run-pin-3", creds)
			if b.APIKeys[secrets.ProviderOpenAICompatible] != "" || b.APIKeys[secrets.ProviderAnthropic] != "sk-ant-team" {
				t.Fatalf("bundle keys = %v, want the vendor key alone", b.APIKeys)
			}
			if !reflect.DeepEqual(creds.contentBoundary, ownKeys()) {
				t.Errorf("content boundary = %+v, want own_keys", creds.contentBoundary)
			}
			if log := buf.String(); strings.Contains(log, "pinned api key "+pin) {
				t.Errorf("the walk names the idle gateway pin's fate:\n%s", log)
			}
		})
	}
}

// P4: the vendor-pin control — a vendor pin keeps its warn-only contract. A
// withheld or deleted pin is said (or silently left to the walk, as it always
// was), and the launch proceeds on the walk's next key.
func TestGatewayPin_AVendorPinKeepsItsWarnOnlyContract(t *testing.T) {
	vendor := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-5"}},
	}}
	for _, row := range []struct {
		name   string
		pinned func(*testing.T, *Publisher) string
		said   string
	}{
		{"withheld by its repository audience", func(t *testing.T, p *Publisher) string {
			return seedPinKey(t, p, secrets.ProviderAnthropic, "sk-ant-pinned", func(k *secrets.ApiKey) { k.Repos = []string{"acme/other"} }, "").ID
		}, "the pin does NOT lift an audience"},
		{"deleted", func(*testing.T, *Publisher) string { return secrets.NewApiKeyID() }, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			seedPinKey(t, p, secrets.ProviderAnthropic, "sk-ant-default", func(k *secrets.ApiKey) { k.IsDefault = true }, "")
			pin := row.pinned(t, p)
			creds, err := resolvePinnedRun(t, p, "run-pin-4", vendor, map[string]string{string(secrets.ProviderAnthropic): pin}, nil, boundaryDerived)
			if err != nil {
				t.Fatalf("a broken vendor pin refused the launch: %v", err)
			}
			if got := openGatewayBundle(t, p, "run-pin-4", creds).APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-default" {
				t.Fatalf("anthropic slot = %q, want the walk's next key", got)
			}
			if row.said != "" && !strings.Contains(buf.String(), row.said) {
				t.Errorf("the walk does not say %q:\n%s", row.said, buf.String())
			}
		})
	}
}
