package runtime

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The hosts a bound secret names in its AllowedHosts open the run's egress
// allowlist. The forge-native ticket lookup of a review on a self-hosted
// forge reads no preset: without this join the CONNECT proxy refuses the
// very host the credential's binding pins — measured live, run
// 01a1bdf3 (honorabilite MR !270), `CONNECT tunnel failed, response 403`
// on the forge the review was reviewing.

func TestABoundSecretsAllowedHostsOpenTheRunEgress(t *testing.T) {
	echoAddr := echoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}

	// The workflow names no rule for the binding's host — the alias must
	// tunnel BECAUSE the secret names it, and nothing else.
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: allowlist
      rules: ["unrelated.example.com"]`))
	drv, err := noop.New()
	if err != nil {
		t.Fatalf("noop driver: %v", err)
	}
	emit := func(store.EventType, map[string]any) error { return nil }

	prx, endpoint, _, err := startNetworkProxy(spec, drv, "run-secret-egress", nil, emit, nil, "", []string{"host.docker.internal"})
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	if prx == nil {
		t.Fatal("an allowlist policy started no proxy — the join was never reached")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	token := tokenFromEndpoint(t, endpoint)

	t.Run("the binding's host tunnels through", func(t *testing.T) {
		line, conn := connect(t, prx.Addr().String(), "host.docker.internal:"+echoPort, token)
		if !strings.Contains(line, "200") {
			t.Fatalf("binding-host CONNECT = %q, want 200", line)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write through tunnel: %v", err)
		}
		got := make([]byte, 4)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read through tunnel: %v", err)
		}
		if string(got) != "ping" {
			t.Fatalf("tunnel echoed %q, want ping", got)
		}
	})

	t.Run("a host nothing names stays refused", func(t *testing.T) {
		if line, _ := connect(t, prx.Addr().String(), "blocked.example.com:443", token); strings.Contains(line, "200") {
			t.Fatalf("an unrelated host tunneled: %q", line)
		}
	})
}

// A workflow `!` exclusion lands AFTER the bound secrets' hosts in the rule
// list, so it stays the final word: a credential's host is opened for the
// run, never forced past an author's refusal. The target carries the echo
// port so an ALLOWED tunnel would answer 200 — the refusal must be the
// policy's, not a dead port's.
func TestAWorkflowExclusionStaysTheFinalWordOnABindingHost(t *testing.T) {
	echoAddr := echoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}

	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: allowlist
      rules: ["!host.docker.internal"]`))
	drv, err := noop.New()
	if err != nil {
		t.Fatalf("noop driver: %v", err)
	}
	emit := func(store.EventType, map[string]any) error { return nil }

	prx, endpoint, _, err := startNetworkProxy(spec, drv, "run-secret-excl", nil, emit, nil, "", []string{"host.docker.internal"})
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	token := tokenFromEndpoint(t, endpoint)

	if line, _ := connect(t, prx.Addr().String(), "host.docker.internal:"+echoPort, token); strings.Contains(line, "200") {
		t.Fatalf("the workflow's exclusion lost to the binding host: %q", line)
	}
}

// secretEgressHostCandidates unions, dedupes and sorts the binding hosts;
// an entry the rule compiler refuses (a bare negation, a malformed CIDR) is
// dropped (fail closed), the rest carry — compileRule reads almost any
// plain word as a literal pattern, harmless for a host no request names.
func TestSecretEgressHostCandidates(t *testing.T) {
	got := secretEgressHostCandidates(map[string][]string{
		"forge_token": {"  b.example.forge ", "a.example.forge", "a.example.forge"},
		"tracker":     {"c.example.com", "", "!"},
		"junk":        {"10.0.0.0/999"},
	}, nil)
	want := []string{"a.example.forge", "b.example.forge", "c.example.com"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if candidates := secretEgressHostCandidates(nil, nil); candidates != nil {
		t.Fatalf("no secrets held = %v, want nil", candidates)
	}
}
