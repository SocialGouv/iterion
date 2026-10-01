//go:build live

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/liveledger"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The #2028 gateway live layer: a tool-using structured node whose model is
// an `openai_compatible/<gateway model id>` route, served from the
// deployment's environment (OPENAI_COMPATIBLE_BASE_URL / _API_KEY). The
// gateway is "a regional LiteLLM gateway" — the operator's env names it; the
// KEY is never printed by this test, and no gateway host or model id is
// asserted here beyond the route's own spelling.
//
// Proven here (the deterministic layer cannot): the gateway serves a real
// generation end to end, the routing identity holds on the wire
// (llm_request.model = the spec, wire_model = the gateway id verbatim), the
// usage account reaches the meter (input_tokens > 0 and NO usage_unreported
// — the gateway's stream usage was requested and answered), a tool call ran,
// and no vendor route was touched. Vendor credentials are CLEARED for the
// run, so a silent vendor crossing fails loudly (401) instead of passing.
//
// Requires: OPENAI_COMPATIBLE_BASE_URL + OPENAI_COMPATIBLE_API_KEY in the
// environment (or .env), docker + ghcr.io/socialgouv/iterion-sandbox-full:edge
// for the sandboxed half. Expected: ~1-3 min, ~$0.01.
//
// Known limits, stated: the sandboxed half runs with the sandbox's network
// policy unset — the allowlist cannot name an env-served host in a static
// bot — so "no vendor call" is enforced there by the cleared vendor env
// alone; the host-scoped allowlist lands when sandbox rules can carry the
// deployment's gateway host. The machine usage-cap does not see gateway
// spend (documented in docs/backends.md).

const gatewayModelEnv = "OPENAI_COMPATIBLE_TEST_MODEL"

func requireGateway(t *testing.T) {
	t.Helper()
	loadDotEnv(t)
	if os.Getenv("OPENAI_COMPATIBLE_BASE_URL") == "" {
		t.Skip("OPENAI_COMPATIBLE_BASE_URL is not set — no gateway to prove against")
	}
	if os.Getenv("OPENAI_COMPATIBLE_API_KEY") == "" {
		t.Skip("OPENAI_COMPATIBLE_API_KEY is not set — the gateway may be network-authenticated only, and this test does not guess")
	}
	// The deployment may serve several ids; the operator's env picks one
	// that exists (the fixture's default is the gateway's general model).
	if os.Getenv(gatewayModelEnv) == "" {
		t.Setenv(gatewayModelEnv, "gpt-oss-120b")
	}
}

// isolateFromVendors clears every vendor credential the in-process claw
// path could reach, so a silent crossing to a vendor fails with a 401
// instead of spending an account the gateway run was never meant to touch.
// Subscription OAuth is refused outright (a disk forfait would otherwise
// answer where the gateway should).
func isolateFromVendors(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY",
		"ZAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
}

// assertGatewayIdentity pins the routing identity and the usage presence on
// the persisted events: every llm_request of the run names the gateway spec,
// its wire_model is the bare gateway id, some step carries real input
// tokens with NO usage_unreported, and no llm_request names a vendor route.
func assertGatewayIdentity(t *testing.T, events []*store.Event) {
	t.Helper()
	spec := "openai_compatible/" + os.Getenv(gatewayModelEnv)
	requests, steps := 0, 0
	sawToolCall, sawInputTokens := false, false
	for _, e := range events {
		switch e.Type {
		case store.EventLLMRequest:
			requests++
			if got, _ := e.Data["model"].(string); got != spec {
				t.Errorf("llm_request.model = %q, want the gateway spec %q", got, spec)
			}
			if got, _ := e.Data["wire_model"].(string); got != os.Getenv(gatewayModelEnv) {
				t.Errorf("llm_request.wire_model = %q, want the gateway id verbatim", got)
			}
			if m, _ := e.Data["model"].(string); isVendorRoute(m) {
				t.Errorf("llm_request crossed to vendor route %q", m)
			}
		case store.EventLLMStepFinished:
			steps++
			if n, _ := e.Data["input_tokens"].(float64); n > 0 {
				sawInputTokens = true
			}
			if n, _ := e.Data["tool_calls"].(float64); n > 0 {
				sawToolCall = true
			}
			if e.Data["usage_unreported"] == true {
				t.Errorf("llm_step_finished carries usage_unreported — the gateway's usage did not reach the meter")
			}
		}
	}
	if requests == 0 {
		t.Fatal("no llm_request event: the run never reached the gateway")
	}
	if steps == 0 {
		t.Fatal("no llm_step_finished event")
	}
	if !sawInputTokens {
		t.Error("no step carried input_tokens > 0")
	}
	if !sawToolCall {
		t.Error("no step carried a tool call — the fixture asks for the bash tool")
	}
}

// isVendorRoute reports a model spec naming a vendor prefix. A gateway spec
// ("openai_compatible/…") is the only route this test may see.
func isVendorRoute(spec string) bool {
	for _, prefix := range []string{"anthropic/", "openai/", "xai/", "moonshot/", "zai/", "bedrock/", "vertex/", "foundry/"} {
		if strings.HasPrefix(spec, prefix) {
			return true
		}
	}
	return false
}

func TestLive_Feat_OpenAICompatible_InProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireGateway(t)
	isolateFromVendors(t)
	liveledger.Track(t)
	res := runGatewayBot(t, "feat_gateway_inprocess.bot")
	assertNodesFinished(t, res.events, "gw")
	assertGatewayIdentity(t, res.events)
}

func TestLive_Feat_OpenAICompatible_Sandboxed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-full:edge")
	requireGateway(t)
	isolateFromVendors(t)
	liveledger.Track(t)
	res := runGatewayBot(t, "feat_gateway_sandbox.bot")
	assertNodesFinished(t, res.events, "gw")
	assertGatewayIdentity(t, res.events)
}

// runGatewayBot runs one gateway fixture end to end. The liveledger hook
// lives in runBotLive.
func runGatewayBot(t *testing.T, botFile string) liveResult {
	t.Helper()
	workspaceDir, err := os.MkdirTemp("", "iterion-feat-gateway-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspaceDir) })
	return runBotLive(t, liveSpec{
		runIDBase:    "live-feat-gateway",
		botFile:      botFile,
		workspaceDir: workspaceDir,
		timeout:      10 * time.Minute,
		withWorkDir:  true, // sandbox-backed: mount the seeded workspace, not the CWD
	})
}
