package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// ambientSourceFP renders the routing label the delegate stamps on a session
// that inherited an ambient credential. Spelled here rather than imported
// (the renderer is unexported) because it is the WIRE SHAPE this package
// reads: if the delegate and the runner ever disagree on the spelling, the
// rows below are what says so.
func ambientSourceFP(fp string) string {
	return "anthropic-env:" + fp
}

// The ambient label keys on the PLATFORM meter under the fingerprint the
// delegate computed: the pod's key is ONE account for the whole deployment,
// so what one tenant's run measures must reach every other borrower's
// pre-flight — and a node's own key, another fingerprint, never merges with
// the pod key it sits beside. Keying it on the run's scope would open one
// ledger per borrower of the same account — the split usageCapCredKeys
// exists to prevent for shared credentials.
func TestUsageCapCredKeys_TheAmbientLabelIsOnePlatformAccount(t *testing.T) {
	label := ambientSourceFP("fp-pod")
	// The label must be one the delegate actually answers, or this bench is
	// measuring its own spelling rather than the meter.
	if _, ok := delegate.AmbientSourceFingerprint(label); !ok {
		t.Fatalf("inert bench: AmbientSourceFingerprint(%q) does not parse", label)
	}
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-pod")
	teamA := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team-a"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai-a"},
	}), &queue.RunMessage{TenantID: "team-a"})
	teamB := usageCapCredKeys(secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team-b"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai-b"},
	}), &queue.RunMessage{TenantID: "team-b"})

	if got := teamA.forSource(label); got != want {
		t.Errorf("forSource(%q) = %q, want the platform meter %q — the pod's key is one account for every tenant", label, got, want)
	}
	if got := teamB.forSource(label); got != want {
		t.Errorf("team B reads %q where team A wrote — one account, one meter, want %q", got, want)
	}
	// The base URL a label may carry is routing, not the account.
	suffixed := ambientSourceFP("fp-pod") + ":https://gateway.internal/anthropic"
	if got := teamA.forSource(suffixed); got != want {
		t.Errorf("forSource(%q) = %q, want %q — the URL is routing, not the account", suffixed, got, want)
	}
	// A node's own key is another account: another fingerprint, another
	// meter, never merged with the pod's.
	nodeKey := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-node")
	if got := teamA.forSource(ambientSourceFP("fp-node")); got != nodeKey {
		t.Errorf("forSource(node label) = %q, want %q", got, nodeKey)
	}
	if nodeKey == want {
		t.Fatal("inert bench: the pod and the node fingerprints collided")
	}
	// The bare label stays the credential-less meter of the run's scope.
	if got, wantScope := teamA.forSource("anthropic-env"), usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-a"), ""); got != wantScope {
		t.Errorf("forSource(anthropic-env) = %q, want %q", got, wantScope)
	}
}

// The whole walk on the real label path: the run holds a tenant z.ai key,
// one of its nodes pins `provider: anthropic` and inherits the pod's
// ANTHROPIC_API_KEY, tenant A's session spends it and publishes its wall —
// and tenant B's pre-flight, walking the same pinned route, reads A's
// refusal and parks. One account, one meter, wherever the borrower sits.
func TestTheAmbientMeterCarriesAWallAcrossTenants(t *testing.T) {
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ZAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	creds := secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-team"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai"},
	}
	ctx := secrets.WithCredentials(context.Background(), creds)
	// The `anthropic` pin with no Anthropic key in the bundle is the route
	// that spends the pod's env: the delegate names the account it inherits.
	label, refused := delegate.AnthropicRouteSource(ctx, "anthropic")
	if refused {
		t.Fatal("the anthropic-pinned route refused — the pod's ambient key is spendable on an unstamped run")
	}
	if want := ambientSourceFP(secrets.FingerprintSHA256("sk-pod")); label != want {
		t.Fatalf("the delegate labeled the pod-env route %q, want %q", label, want)
	}

	store := usagecap.NewMemStore()
	teamA := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-a"})
	rejected := usagecap.Reading{
		Window:      usagecap.WindowFiveHour,
		Utilization: 1,
		Status:      usagecap.StatusRejected,
		ObservedAt:  time.Now().UTC(),
	}
	if err := store.Record(context.Background(), teamA.forSource(label), rejected); err != nil {
		t.Fatal(err)
	}

	teamB := usageCapCredKeys(ctx, &queue.RunMessage{TenantID: "team-b"})
	route := model.WireRoute{NodeID: "n1", Backend: delegate.BackendClaudeCode, Hint: "anthropic", Readable: true}
	key, metered := teamB.routeKey(ctx, route)
	if !metered {
		t.Fatal("the pod-env route meters nothing — the pre-flight would sail past the wall tenant A measured")
	}
	readings, err := store.Latest(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	pol := usagecap.Policy{FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeSoft}}
	if d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultTrust()); !d.Blocked {
		t.Fatalf("tenant B's pre-flight read %d readings and did not block — the ambient wall did not cross tenants", len(readings))
	}
}

// The spend ledger reads the same labels: a fingerprinted ambient label
// names the inherited env — no slot of the run — and must not fall through
// to the (backend, model) booking, which would charge the pod's or a node's
// own spend to the bundle default the session never touched.
func TestSlotOfSource_TheFingerprintedAmbientLabelNamesNoSlot(t *testing.T) {
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-platform"}}
	label := ambientSourceFP("fp-pod")
	slot, named := slotOfSource(label)
	if !named || slot != "" {
		t.Errorf("slotOfSource(%q) = %q, %v — want named, no slot", label, slot, named)
	}
	if got := routeSlot(creds, routeKey{backend: delegate.BackendClaudeCode, source: label}); got != "" {
		t.Errorf("routeSlot booked the ambient spend on %q — want nobody", got)
	}
}
