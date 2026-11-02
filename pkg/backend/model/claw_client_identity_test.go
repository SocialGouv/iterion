package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"
)

// The engine's in-process claw clients refuse the ambient header block
// (#2244): the registry resolves the identity itself and sets
// NoAmbientHeaders, and claw's anthropic provider threads it into the client
// it builds — an ambient credential line in the operator's shell must not
// authenticate a request the engine routed. The type assertion reads the
// VENDORED client, so the witness fails on a revert of either half: the flag
// dropped here, or the threading dropped upstream.
func TestEngineRegistryClientRefusesAmbientHeaderBlock(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Operator-Secret: s3cr3t")
	t.Setenv("CLAW_USER_AGENT", "operator-agent/1")

	r := NewRegistry()
	cfg := r.withClientIdentity(api.ProviderConfig{APIKey: "k", Model: "m"})
	if !cfg.NoAmbientHeaders {
		t.Fatal("the engine registry left NoAmbientHeaders unset — the ambient header block (#2244) rides engine-resolved routes again")
	}
	cli, err := anthropicprovider.New().NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c, ok := cli.(*api.Client)
	if !ok {
		t.Fatalf("client type %T, want *api.Client", cli)
	}
	if !c.NoAmbientHeaders {
		t.Fatal("the vendored anthropic provider dropped NoAmbientHeaders while threading the config — bump-claw carries a claw-code-go regression")
	}
	if ua := c.UserAgent; ua != "operator-agent/1" {
		t.Errorf("the documented UA chain broke: UserAgent = %q, want CLAW_USER_AGENT's value", ua)
	}
}

// TestEngineResolvedWireCarriesNoAmbientSecretLine is the WIRE-level witness
// of #2244: the field reads above pin the flag being set and threaded, this
// one pins what a request the engine routed actually carries. A claw bump
// whose identity pass re-derives the choice — ResolveIdentityFlagged
// ignoring NoAmbientHeaders and re-reading the operator's shell — or a drop
// of the flag at withClientIdentity leaves every field read green while the
// ambient credential line rides the request; the secret below reddens. The
// vendored wire tests live upstream and go mod vendor strips them, so this
// file carries the seam's only witness in-repo.
func TestEngineResolvedWireCarriesNoAmbientSecretLine(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Operator-Secret: s3cr3t")
	t.Setenv("CLAW_USER_AGENT", "operator-agent/1")
	t.Setenv("ITERION_LLM_USER_AGENT", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	hdrCh := make(chan http.Header, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hdrCh <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer ts.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)

	client, err := NewRegistry().Resolve("anthropic/claude-test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for range ch { // drain so the stream goroutine finishes
	}
	var hdr http.Header
	select {
	case hdr = <-hdrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub never received a request")
	}

	if got := hdr.Get("X-Operator-Secret"); got != "" {
		t.Errorf("the ambient header block reached the engine-routed wire: X-Operator-Secret = %q — NoAmbientHeaders no longer closes the read", got)
	}
	// The whole header set, not only the name the ambient line spells: a
	// re-derived identity pass can carry the value under another name.
	dumped, err := json.Marshal(hdr)
	if err != nil {
		t.Fatalf("marshal captured headers: %v", err)
	}
	if strings.Contains(string(dumped), "s3cr3t") {
		t.Errorf("the ambient credential's value rode the wire: %s", dumped)
	}
	if ua := hdr.Get("User-Agent"); ua != "operator-agent/1" {
		t.Errorf("User-Agent = %q, want the engine's named read of CLAW_USER_AGENT — the refusal closes the ambient read, not the channel's documented use", ua)
	}
}

// The sandbox runner's registry keeps claw's ambient read: its process env is
// the crossing's forwarded env — identity lines the launcher deliberately
// sent, credential lines already stripped — and refusing it there would drop
// the channel forwardableProviderEnv built. A future change that sets the
// flag unconditionally reddens here.
func TestSandboxRunnerRegistryKeepsTheForwardedChannel(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-tenant: acme")

	r := NewSandboxRunnerRegistry()
	cfg := r.withClientIdentity(api.ProviderConfig{Model: "m"})
	if cfg.NoAmbientHeaders {
		t.Fatal("the sandbox runner registry refused the ambient env — the crossing's forwarded block is the channel, not a leak")
	}
}

// The identity the engine resolves follows the documented precedence by its
// own named read: ITERION_LLM_USER_AGENT, then CLAW_USER_AGENT, empty = claw
// resolves its honest default.
func TestEngineClientUserAgentChain(t *testing.T) {
	t.Setenv("ITERION_LLM_USER_AGENT", "iterion-tool/1")
	t.Setenv("CLAW_USER_AGENT", "operator-agent/1")
	if got := engineClientUserAgent(); got != "iterion-tool/1" {
		t.Errorf("ITERION_LLM_USER_AGENT should win the chain, got %q", got)
	}

	t.Setenv("ITERION_LLM_USER_AGENT", "")
	if got := engineClientUserAgent(); got != "operator-agent/1" {
		t.Errorf("CLAW_USER_AGENT should carry the chain when ITERION_LLM_USER_AGENT is empty, got %q", got)
	}

	t.Setenv("CLAW_USER_AGENT", "")
	if got := engineClientUserAgent(); got != "" {
		t.Errorf("an empty chain must stay empty — claw resolves its honest default, got %q", got)
	}
}

// Both legs of the chain trim: claw's own read of CLAW_USER_AGENT trims
// (vendored identity.go ResolveIdentity), and the engine's named read must
// resolve the identity claw itself would from the same environment. A padded
// value kept verbatim rides the wire in both lanes — the sandbox runner sets
// the explicit UA too, so claw's trim never runs there either — and a
// whitespace-only CLAW_USER_AGENT would replace claw's honest default on the
// wire instead of falling through to it.
func TestEngineClientUserAgentTrimsBothLegs(t *testing.T) {
	t.Setenv("ITERION_LLM_USER_AGENT", "")
	t.Setenv("CLAW_USER_AGENT", "  operator-agent/1\t")
	if got := engineClientUserAgent(); got != "operator-agent/1" {
		t.Errorf("padded CLAW_USER_AGENT = %q, want trimmed operator-agent/1", got)
	}

	t.Setenv("CLAW_USER_AGENT", "   ")
	if got := engineClientUserAgent(); got != "" {
		t.Errorf("whitespace-only CLAW_USER_AGENT = %q, want empty — claw's honest default must carry", got)
	}

	t.Setenv("ITERION_LLM_USER_AGENT", "\titerion-tool/1\n")
	if got := engineClientUserAgent(); got != "iterion-tool/1" {
		t.Errorf("padded ITERION_LLM_USER_AGENT = %q, want trimmed iterion-tool/1", got)
	}
}
