package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The sandbox runner's registry is the one registry that keeps claw's
// ambient header read: the runner's process env IS the crossing's forwarded
// env — forwardableProviderEnv composed ANTHROPIC_CUSTOM_HEADERS there,
// credential lines stripped, identity lines (x-tenant and friends) sent on
// purpose. A revert of the runner's constructor to NewRegistry() sets
// NoAmbientHeaders in this process and silently drops the vetted block on
// every sandboxed claw node; the field witnesses stay green on it (the
// constructor test pins the semantics the constructor already has, not the
// call site), so this witness drives the real runClawRunner and reads the
// WIRE — the test's own env stands in for the env the launcher forwarded,
// runClawRunner runs in-process.
func TestClawRunner_TheForwardedHeaderBlockReachesTheWire(t *testing.T) {
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "x-tenant: acme")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

	hdrCh := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hdrCh <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	res := driveClawRunner(t, nil, delegate.IOTask{NodeID: "copi", Model: "openai/gpt-test", UserPrompt: "hello"})
	if res.Error != "" {
		t.Fatalf("the runner ended on %q, want the answer", res.Error)
	}

	var hdr http.Header
	select {
	case hdr = <-hdrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub never received a request")
	}
	if got := hdr.Get("X-Tenant"); got != "acme" {
		t.Errorf("the forwarded header block never reached the sandboxed wire: x-tenant = %q — the runner's registry no longer keeps claw's ambient read (NewSandboxRunnerRegistry), and the crossing's vetted identity lines are dropped (request headers: %v)", got, hdr)
	}
}
