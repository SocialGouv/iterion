package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The rule: at the SHARED tiers (org, platform) the fill stays one
// credential per wire family — except a slot some route of the run NAMES,
// which is fundable on its own name.
//
// Both directions of the same bench, because either half alone is passable:
// the pinned case must gain the credential, and the UNPINNED case must keep
// the exact behaviour it had before the rule existed.

// wfPinning builds a one-agent workflow whose node pins the given provider
// ("" = pins nothing). The pinned set is derived from the compiled IR the
// launch resolves, so the bench feeds the real thing rather than a list.
func wfPinning(provider string) *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{
			Backend:  "claude_code",
			Model:    "claude-opus-5",
			Provider: provider,
		},
	}}}
}

// platformBundleFor resolves a bundle for a run whose workflow pins
// `provider`, with the platform holding a key for each of `platformKeys`
// and the tenant holding `tenantKeys`.
func platformBundleFor(t *testing.T, provider string, platformKeys, tenantKeys []secrets.Provider, tenantForfait bool) secrets.RunBundle {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	for _, p := range platformKeys {
		seedKey(t, keys, sealer, secrets.PlatformTenantID, p, "platform-"+string(p))
	}
	for _, p := range tenantKeys {
		seedKey(t, keys, sealer, "team1", p, "tenant-"+string(p))
	}
	pub := &Publisher{
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
	}
	if tenantForfait {
		oauth := secrets.NewMemoryOAuthStore()
		seedOAuth(t, oauth, sealer, "webhook:cfg-1", "sk-ant-tenant-forfait")
		pub.oauthForfait = oauth
	}
	rs := pub.runSecrets.(*secrets.MemoryRunSecretsStore)
	pinned := derivePinnedProviders(wfPinning(provider), model.ModelOverrides{}, nil)
	if provider != "" && len(pinned) == 0 {
		t.Fatalf("inert bench: the walk read no pin from a node pinning %q — it would prove nothing about the exception", provider)
	}
	if provider == "" && len(pinned) != 0 {
		t.Fatalf("inert bench: a node pinning nothing yielded pins %v", pinned)
	}
	return resolveBundlePinned(t, pub, rs, sealer, "run-1", "team1", "webhook:cfg-1", pinned)
}

// A platform holding BOTH an Anthropic and a Moonshot key funds only
// anthropic for an unpinned run (one key per wire family). A node pinned
// `provider: moonshot` makes the moonshot slot fundable — and the key lands
// where no default-precedence reader looks, so the unpinned work of the same
// run is untouched.
func TestPinnedProvider_PlatformFundsThePinnedSlotOnATakenWire(t *testing.T) {
	b := platformBundleFor(t, "moonshot",
		[]secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderMoonshot}, nil, false)

	if got := b.PinnedAPIKeys[secrets.ProviderMoonshot]; got != "platform-moonshot" {
		t.Errorf("PinnedAPIKeys[moonshot] = %q, want the platform key — a node pinned to moonshot has no credential", got)
	}
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "platform-anthropic" {
		t.Errorf("APIKeys[anthropic] = %q, want the platform key — the family winner must not change", got)
	}
	// THE property: an unpinned node must not be able to reach it. The
	// anthropic wire ranks moonshot ABOVE anthropic, so a moonshot key in
	// APIKeys would capture every unpinned node of the run.
	if got, present := b.APIKeys[secrets.ProviderMoonshot]; present {
		t.Errorf("APIKeys[moonshot] = %q — a pinned-only key in the default map reroutes every unpinned node onto another vendor", got)
	}
}

// The other direction, same harness: with nothing pinned, the moonshot key
// is NOT funded. This is the behaviour that must not change.
func TestPinnedProvider_UnpinnedRunKeepsOneKeyPerWireFamily(t *testing.T) {
	b := platformBundleFor(t, "",
		[]secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderMoonshot}, nil, false)

	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "platform-anthropic" {
		t.Errorf("APIKeys[anthropic] = %q, want the platform key", got)
	}
	if got, present := b.APIKeys[secrets.ProviderMoonshot]; present {
		t.Errorf("APIKeys[moonshot] = %q — an unpinned run must still get ONE key per wire family", got)
	}
	if got, present := b.PinnedAPIKeys[secrets.ProviderMoonshot]; present {
		t.Errorf("PinnedAPIKeys[moonshot] = %q — nothing pins moonshot, so nothing may fund it", got)
	}
}

// The rule the family gate exists for: a tenant with its OWN forfait keeps
// it serving its unpinned nodes. A pinned node may add a platform key beside
// it, and that key must not become the run's default — the delegate's
// precedence puts a facade key above an OAuth forfait, so a capture here
// would silently move every unpinned node onto the platform's account.
func TestPinnedProvider_PlatformKeyNeverServesTheUnpinnedWorkOfAForfaitTenant(t *testing.T) {
	b := platformBundleFor(t, "moonshot",
		[]secrets.Provider{secrets.ProviderMoonshot}, nil, true)

	if len(b.OAuthCredentials["claude_code"]) == 0 {
		t.Fatal("inert bench: the tenant forfait did not resolve, so there is no wire for the platform key to capture")
	}
	if got, present := b.APIKeys[secrets.ProviderMoonshot]; present {
		t.Fatalf("APIKeys[moonshot] = %q — it outranks the tenant's forfait in the delegate's precedence, so every unpinned node would spend the platform's Kimi account", got)
	}
	if got := b.PinnedAPIKeys[secrets.ProviderMoonshot]; got != "platform-moonshot" {
		t.Errorf("PinnedAPIKeys[moonshot] = %q, want the platform key — the pinned node still needs funding", got)
	}
}

// A tenant's OWN keys were never gated on the wire family, and are not now:
// the exception lives in the shared tiers only.
func TestPinnedProvider_TenantKeysAreUnaffected(t *testing.T) {
	b := platformBundleFor(t, "", nil,
		[]secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderMoonshot}, false)

	for _, p := range []secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderMoonshot} {
		if got := b.APIKeys[p]; got != "tenant-"+string(p) {
			t.Errorf("APIKeys[%s] = %q, want the tenant's own key", p, got)
		}
	}
	if len(b.PinnedAPIKeys) != 0 {
		t.Errorf("PinnedAPIKeys = %v — a tenant's own keys never ride the pinned channel", b.PinnedAPIKeys)
	}
}

// derivePinnedProviders reads a `provider:` CHAIN, not just a single value:
// the first element is where the run starts, and an unfunded chain head is
// a refusal at the first attempt.
func TestDerivePinnedProviders_ReadsChainsAndPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		node  *ir.Workflow
		wants []string
	}{
		{"chain_head_and_tail", wfPinning("moonshot,anthropic"), []string{"anthropic", "moonshot"}},
		{"single", wfPinning("zai"), []string{"zai"}},
		{"nothing", wfPinning(""), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := derivePinnedProviders(tc.node, model.ModelOverrides{}, nil)
			if len(got) != len(tc.wants) {
				t.Fatalf("derivePinnedProviders = %v, want %v", got, tc.wants)
			}
			for i := range got {
				if got[i] != tc.wants[i] {
					t.Fatalf("derivePinnedProviders = %v, want %v", got, tc.wants)
				}
			}
		})
	}
}

// A claw node pins its provider in the MODEL SPEC (it has no `provider:`
// hint of its own), so the same rule has to read that form.
func TestDerivePinnedProviders_ReadsAClawModelPrefix(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"review": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "review"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "moonshot/kimi-k2"},
	}}}
	got := derivePinnedProviders(wf, model.ModelOverrides{}, nil)
	if len(got) != 1 || got[0] != "moonshot" {
		t.Fatalf("derivePinnedProviders = %v, want [moonshot] — a claw node pins through its model spec", got)
	}
}

// The frozen contract. A resume replays store.Run.PinnedProviders; it must
// NOT re-derive from the workflow it happens to hold, or a source that moved
// between launch and resume would change which credentials the run is
// granted — funding decided by a program the launch never approved.
//
// Driven through the real resolution, twice on one bench: the same platform
// store, the same tenant, once with the launch's frozen set and once with the
// set a moved source would yield.
func TestPinnedProvider_ResumeReplaysTheLaunchSetNotTheCurrentSource(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "platform-anthropic")
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderMoonshot, "platform-moonshot")

	// What the launch derived and stamped, from a program that pinned nothing.
	launchSet := derivePinnedProviders(wfPinning(""), model.ModelOverrides{}, nil)
	// What the SAME bot would yield after someone added a moonshot pin.
	movedSet := derivePinnedProviders(wfPinning("moonshot"), model.ModelOverrides{}, nil)
	if len(movedSet) == 0 {
		t.Fatal("inert bench: the moved source yields no pin, so the two inputs do not differ")
	}

	resolve := func(pinned []string) secrets.RunBundle {
		p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		return resolveBundlePinned(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1", pinned)
	}

	// The resume passes the LAUNCH's set: the moonshot key stays unfunded,
	// exactly as at launch.
	if b := resolve(launchSet); len(b.PinnedAPIKeys) != 0 {
		t.Errorf("resuming on the launch-frozen set funded %v — the run gained a credential its launch never had", b.PinnedAPIKeys)
	}
	// Proof the bench can tell the two apart: fed the moved set, the same
	// resolution DOES fund it. A test that only checked the first half would
	// pass on a publisher that ignored the set entirely.
	if b := resolve(movedSet); b.PinnedAPIKeys[secrets.ProviderMoonshot] != "platform-moonshot" {
		t.Errorf("fed the moved source's set, PinnedAPIKeys = %v — the bench cannot distinguish the two inputs, so its first half proves nothing", b.PinnedAPIKeys)
	}
}

// End to end through the real launch and resume surfaces: the stamp is
// written once, and the resume funds from IT rather than from the workflow it
// is handed.
//
// Both directions on one bench, because the freeze has two failure modes and
// each hides the other: a resume that re-derives would GAIN a credential when
// the source gained a pin, and LOSE one when the source dropped it.
func TestSubmitResume_FundsFromTheLaunchStampNotTheResumedSource(t *testing.T) {
	for _, tc := range []struct {
		name            string
		launchPins      string
		resumeSourcePin string
		wantPinnedKey   string
	}{
		// The launch pinned nothing: a source that GAINED a moonshot pin
		// must not win the run a credential its launch never had.
		{"source_gained_a_pin", "", "moonshot", ""},
		// The launch pinned moonshot: a source that DROPPED the pin must not
		// take the credential away from the run that was granted it.
		{"source_dropped_the_pin", "moonshot", "", "platform-moonshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "platform-anthropic")
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderMoonshot, "platform-moonshot")
			var published *queue.RunMessage
			p := &Publisher{
				store:      st,
				apiKeys:    keys,
				runSecrets: secrets.NewMemoryRunSecretsStore(),
				sealer:     sealer,
				logger:     testLogger(),
				publishRun: func(_ context.Context, m *queue.RunMessage) error { published = m; return nil },
			}
			rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
			ctx := store.WithIdentity(context.Background(), "team-a", "u1")
			src := "workflow wf:\n  start -> done\n"
			cs := &runview.CompiledSource{Hash: "hash"}

			if _, err := p.SubmitLaunch(ctx, "run-1",
				runview.LaunchSpec{FilePath: "wf.bot", Source: src}, wfPinning(tc.launchPins), cs); err != nil {
				t.Fatalf("SubmitLaunch: %v", err)
			}
			// The stamp, which the resume is supposed to replay.
			r, err := st.LoadRun(ctx, "run-1")
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			wantStamp := derivePinnedProviders(wfPinning(tc.launchPins), model.ModelOverrides{}, nil)
			if len(r.PinnedProviders) != len(wantStamp) {
				t.Fatalf("run.PinnedProviders = %v, want %v — the launch did not freeze its pinned set", r.PinnedProviders, wantStamp)
			}

			if err := st.UpdateRunStatus(ctx, "run-1", store.RunStatusPausedWaitingHuman, ""); err != nil {
				t.Fatalf("pause: %v", err)
			}
			published = nil
			// The resume is handed a DIFFERENT program — the source moved.
			if err := p.SubmitResume(ctx,
				runview.ResumeSpec{RunID: "run-1", FilePath: "wf.bot", Source: src},
				wfPinning(tc.resumeSourcePin), cs); err != nil {
				t.Fatalf("SubmitResume: %v", err)
			}
			if published == nil || published.SecretsRef == "" {
				t.Fatal("the resume published no bundle, so there is nothing to read the funding from")
			}
			rec, err := rs.Get(store.WithTenant(ctx, "team-a"), published.SecretsRef)
			if err != nil {
				t.Fatalf("RunSecrets.Get: %v", err)
			}
			b, err := secrets.OpenRunBundle(sealer, "run-1", rec.SealedBundle)
			if err != nil {
				t.Fatalf("OpenRunBundle: %v", err)
			}
			if got := b.PinnedAPIKeys[secrets.ProviderMoonshot]; got != tc.wantPinnedKey {
				t.Errorf("resumed PinnedAPIKeys[moonshot] = %q, want %q — the resume funded from the source it was handed, not from the launch stamp", got, tc.wantPinnedKey)
			}
			// The family winner is unchanged either way.
			if got := b.APIKeys[secrets.ProviderAnthropic]; got != "platform-anthropic" {
				t.Errorf("resumed APIKeys[anthropic] = %q, want the platform key", got)
			}
		})
	}
}
