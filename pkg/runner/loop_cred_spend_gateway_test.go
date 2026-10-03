package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayCreds is a run that DOES hold a resolvable anthropic credential —
// plus a key pinned for the gateway wire itself — so a route mis-booked off
// the gateway lands somewhere visible instead of vanishing with the same
// "" the guard produces.
func gatewayCreds() secrets.Credentials {
	return secrets.Credentials{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-test"},
		Fingerprints:  map[string]string{string(secrets.ProviderAnthropic): "fp-ant"},
		PinnedAPIKeys: map[secrets.Provider]string{"openai_compatible": "gw-key"},
	}
}

// TestGatewayRoute_IsEnvFundedNoWireNoSlot pins the pair of reads a gateway
// route answers at metering time: its wire is the gateway's own, and its
// credential slot is nobody's — on claw AND claude_code, while the same
// credentials still resolve a vendor route. Mutations that must redden it:
//   - delete wireForRoute's gateway branch → the gateway route reads as the
//     backend's anthropic default and the wire assertions redden;
//   - delete credentialSlotForRoute's gateway branch → the pinned
//     openai_compatible key resolves a slot and the slot assertions redden.
func TestGatewayRoute_IsEnvFundedNoWireNoSlot(t *testing.T) {
	creds := gatewayCreds()
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		if got := wireForRoute(backend, "openai_compatible/m"); got != gatewayWire {
			t.Errorf("wireForRoute(%q, openai_compatible/m) = %q, want %q — a gateway route runs on its own wire", backend, got, gatewayWire)
		}
		if got := credentialSlotForRoute(creds, backend, "openai_compatible/m"); got != "" {
			t.Errorf("credentialSlotForRoute(%q, openai_compatible/m) = %q, want \"\" — the deployment's gateway books on nobody", backend, got)
		}
	}
	// The narrowing is visible: the SAME credentials resolve a vendor route.
	if got := wireForRoute(delegate.BackendClaw, "anthropic/claude-x"); got != anthropicWire {
		t.Errorf("wireForRoute(claw, anthropic/claude-x) = %q, want %q — the gateway guard must narrow, not blank every read", got, anthropicWire)
	}
	if got := credentialSlotForRoute(creds, delegate.BackendClaw, "anthropic/claude-x"); got != string(secrets.ProviderAnthropic) {
		t.Errorf("credentialSlotForRoute(claw, anthropic/claude-x) = %q, want anthropic", got)
	}
}

// TestRecordCredentialSpend_GatewayRouteIsSkippedAsEnvFunded pins the
// per-credential meter's gateway branch: the route books no row on any
// credential, and the drop is said once, at Info, naming the run — the same
// visibility the declined routes get. Mutations that must redden it:
//   - delete the gateway skip in recordCredentialSpend → the route falls to
//     the declined path: the log line loses "env-funded";
//   - delete the gateway guard in credentialSlotForRoute too (or pin the
//     gateway key without it) → the pinned key is charged a row.
func TestRecordCredentialSpend_GatewayRouteIsSkippedAsEnvFunded(t *testing.T) {
	var buf bytes.Buffer
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelInfo, &buf), CredUsage: counter}}
	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "gw",
		Data: map[string]any{"model": "openai_compatible/m"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "gw",
		Data: map[string]any{"backend": delegate.BackendClaw, "tokens": float64(1200), "cost_usd": 0.1}})

	ctx := secrets.WithCredentials(context.Background(), gatewayCreds())
	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "gw-run", TenantID: "team-a"}, usage, now)

	rows, err := counter.List(ctx, now, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the deployment's gateway spend was metered on %d credential row(s): %+v — an env-funded route books on nobody", len(rows), rows)
	}
	logged := buf.String()
	if !strings.Contains(logged, "gw-run") || !strings.Contains(logged, "env-funded") {
		t.Errorf("the env-funded skip left no line an operator can read:\n%q", logged)
	}
	if n := strings.Count(logged, "env-funded"); n != 1 {
		t.Errorf("the skip said env-funded %d times, want once per route:\n%q", n, logged)
	}
}
