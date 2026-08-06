package runtime

import (
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `network: allowlist` is an egress boundary an operator declares in the
// .bot and never sees again — which is exactly how a silently-ignored flag
// survives: nothing in a passing run tells you the proxy was never started,
// or was started open. The pieces are each unit-tested (ResolveNetworkPolicy
// derives the mode, netproxy enforces a compiled policy) but the WIRE from
// the declaration to a running, enforcing proxy is what the promise rests on.
//
// The CONNECT proxy runs on the HOST, so that wire is exercisable without a
// container: compile the declaration from real .bot source, hand the derived
// spec to startNetworkProxy with the noop driver, and speak HTTP CONNECT to
// the proxy it returns. An allowed host tunnels to a loopback echo server; a
// denied one is refused AND surfaces the `network_blocked` event, which is
// the only thing an operator ever sees about a blocked call.

// compileSandboxSpec turns .bot source into the runtime-level sandbox spec,
// through the real parser + IR compiler.
func compileSandboxSpec(t *testing.T, src string) *sandbox.Spec {
	t.Helper()
	res := parser.Parse("net.bot", src)
	cr := ir.Compile(res.File)
	if cr.Workflow == nil {
		t.Fatalf("compile produced no workflow: %+v", cr.Diagnostics)
	}
	if cr.Workflow.Sandbox == nil {
		t.Fatal("workflow carries no sandbox spec — the declaration was dropped before the runtime saw it")
	}
	spec := fromIRSpec(cr.Workflow.Sandbox)
	return &spec
}

func netPolicySource(networkBlock string) string {
	return `
tool probe:
  command: "true"

workflow main:
  sandbox:
    image: "example/img:latest"
` + networkBlock + `
  entry: probe
  probe -> done
`
}

// echoServer is the "reachable host" the allowed CONNECT tunnels to.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// connect speaks one CONNECT request to the proxy and returns its status
// line plus the (possibly tunnelled) connection.
func connect(t *testing.T, proxyAddr, target, token string) (string, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if token != "" {
		req += "Proxy-Authorization: Bearer " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 256)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	line, _, _ := strings.Cut(string(buf[:n]), "\r\n")
	return line, conn
}

// tokenFromEndpoint extracts the proxy token iterion advertises to the
// container in HTTPS_PROXY (http://t:<token>@host:port).
func tokenFromEndpoint(t *testing.T, endpoint string) string {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	tok, _ := u.User.Password()
	if tok == "" {
		t.Fatalf("endpoint %q carries no proxy token", endpoint)
	}
	return tok
}

func TestSandboxNetworkAllowlist_DeclarationStartsAnEnforcingProxy(t *testing.T) {
	echoAddr := echoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}

	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: allowlist
      rules: ["localhost"]`))

	drv, err := noop.New()
	if err != nil {
		t.Fatalf("noop driver: %v", err)
	}
	var events []map[string]any
	var kinds []store.EventType
	emit := func(kind store.EventType, data map[string]any) error {
		kinds = append(kinds, kind)
		events = append(events, data)
		return nil
	}

	prx, endpoint, caPEM, err := startNetworkProxy(spec, drv, "run-net", nil, emit, nil)
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	if prx == nil {
		t.Fatal("an allowlist declaration started no proxy — the policy is unenforced")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	if caPEM != nil {
		t.Fatal("no secret rewriter was requested, so no inspection CA should be minted")
	}
	token := tokenFromEndpoint(t, endpoint)

	t.Run("an allowed host tunnels through", func(t *testing.T) {
		line, conn := connect(t, prx.Addr().String(), "localhost:"+echoPort, token)
		if !strings.Contains(line, "200") {
			t.Fatalf("allowed CONNECT = %q, want 200", line)
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

	t.Run("a host outside the allowlist is refused and reported", func(t *testing.T) {
		line, _ := connect(t, prx.Addr().String(), "blocked.example.com:443", token)
		if strings.Contains(line, "200") {
			t.Fatalf("blocked CONNECT was established: %q", line)
		}
		found := false
		for i, data := range events {
			if kinds[i] != store.EventNetworkBlocked {
				continue
			}
			if data["host"] == "blocked.example.com:443" || data["host"] == "blocked.example.com" {
				found = true
				if data["run_id"] != "run-net" {
					t.Fatalf("network_blocked event names run %v, want run-net", data["run_id"])
				}
			}
		}
		if !found {
			t.Fatalf("no network_blocked event for the refused host: %+v", events)
		}
	})

	t.Run("the proxy is not an open relay for other local processes", func(t *testing.T) {
		line, _ := connect(t, prx.Addr().String(), "localhost:"+echoPort, "")
		if strings.Contains(line, "200") {
			t.Fatalf("un-tokened CONNECT was established: %q", line)
		}
	})
}

// The off-state of the same flag: `network: open` (and the default) must
// leave the zero-overhead path — no proxy process, no endpoint to inject.
func TestSandboxNetworkOpen_StartsNoProxy(t *testing.T) {
	drv, err := noop.New()
	if err != nil {
		t.Fatalf("noop driver: %v", err)
	}
	emit := func(store.EventType, map[string]any) error { return nil }

	for _, tc := range []struct {
		name  string
		block string
	}{
		{"explicit open", `    network:
      mode: open`},
		{"no network block at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := compileSandboxSpec(t, netPolicySource(tc.block))
			prx, endpoint, _, err := startNetworkProxy(spec, drv, "run-open", nil, emit, nil)
			if err != nil {
				t.Fatalf("startNetworkProxy: %v", err)
			}
			if prx != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = prx.Shutdown(ctx)
				t.Fatal("open egress started a proxy anyway")
			}
			if endpoint != "" {
				t.Fatalf("open egress advertised proxy endpoint %q", endpoint)
			}
		})
	}
}

// A denylist declaration is the mirror rule: everything passes except what a
// NEGATED rule excludes (`!**.evil.site` — in denylist mode a bare pattern
// allows, the `!` prefix is what denies; the shipped iterion-default preset
// is written the same way). Nothing external is dialled here: the denied host
// is refused before the proxy resolves it, and the reachable leg is loopback.
func TestSandboxNetworkDenylist_BlocksOnlyTheListedHosts(t *testing.T) {
	echoAddr := echoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatalf("split echo addr: %v", err)
	}
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: denylist
      rules: ["!**.evil.site"]`))

	drv, err := noop.New()
	if err != nil {
		t.Fatalf("noop driver: %v", err)
	}
	blocked := map[string]bool{}
	emit := func(kind store.EventType, data map[string]any) error {
		if kind == store.EventNetworkBlocked {
			if h, ok := data["host"].(string); ok {
				blocked[h] = true
			}
		}
		return nil
	}
	prx, endpoint, _, err := startNetworkProxy(spec, drv, "run-deny", nil, emit, nil)
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	if prx == nil {
		t.Fatal("a denylist declaration started no proxy — the policy is unenforced")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	token := tokenFromEndpoint(t, endpoint)

	if line, _ := connect(t, prx.Addr().String(), "exfil.evil.site:443", token); strings.Contains(line, "200") {
		t.Fatalf("denylisted host was established: %q", line)
	}
	if len(blocked) == 0 {
		t.Fatal("denied host produced no network_blocked event")
	}
	if line, _ := connect(t, prx.Addr().String(), "localhost:"+echoPort, token); !strings.Contains(line, "200") {
		t.Fatalf("un-listed host refused under denylist: %q", line)
	}
}
