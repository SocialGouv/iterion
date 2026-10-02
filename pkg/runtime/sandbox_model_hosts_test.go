package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/noop"
	"github.com/SocialGouv/iterion/pkg/store"
)

// inspectingDriver is the noop driver claiming TLS-inspection support, so
// startNetworkProxy runs the inspecting proxy on the host.
type inspectingDriver struct{ sandbox.Driver }

func (d inspectingDriver) Capabilities() sandbox.Capabilities {
	c := d.Driver.Capabilities()
	c.SupportsTLSInspection = true
	return c
}

// recordingRewriter records every text the proxy asks it to materialise.
type recordingRewriter struct {
	mu   sync.Mutex
	seen []string
}

func (r *recordingRewriter) MaterializeForHostWithin(s, _ string, limit int) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
	return s, len(s) <= limit
}

func (r *recordingRewriter) ExfiltratesTo(string, string) bool { return false }

func (r *recordingRewriter) materialised(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ITERION_SANDBOX_MODEL_HOSTS reaches the run's inspecting proxy: a request
// body bound for a declared model gateway is never materialised, another
// host's is.
func TestADeclaredModelGatewaysBodyIsNeverMaterialised(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_MODEL_HOSTS", "llm-gw.invalid:8443, https://other-gw.invalid/v1")
	drv, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: open`))
	rw := &recordingRewriter{}
	prx, endpoint, caPEM, err := startNetworkProxy(spec, inspectingDriver{drv}, "run-gw", rw, func(store.EventType, map[string]any) error { return nil }, nil)
	if err != nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	if prx == nil || caPEM == nil {
		t.Fatal("scenario broken: no inspecting proxy")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	proxyURL, _ := url.Parse("http://iterion:" + tokenFromEndpoint(t, endpoint) + "@" + prx.Addr().String())
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	for _, host := range []string{"llm-gw.invalid:8443", "other-gw.invalid", "tool-api.invalid"} {
		// The upstream never resolves: what matters is what the proxy asked
		// the rewriter before forwarding.
		if resp, err := client.Post("https://"+host+"/generate-text", "text/plain", strings.NewReader("body-for-"+host)); err == nil {
			_ = resp.Body.Close()
		}
	}
	for _, host := range []string{"llm-gw.invalid:8443", "other-gw.invalid"} {
		if rw.materialised("body-for-" + host) {
			t.Errorf("the body bound for the declared model gateway %s was materialised", host)
		}
	}
	if !rw.materialised("body-for-tool-api.invalid") {
		t.Fatal("scenario broken: the proxy materialised no body")
	}
}

func TestSandboxModelHostsSplitsOnCommasAndSpaces(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_MODEL_HOSTS", " a.example , b.example\tc.example,,")
	got := sandboxModelHosts()
	if strings.Join(got, "|") != "a.example|b.example|c.example" {
		t.Fatalf("sandboxModelHosts() = %q", got)
	}
}

// An ITERION_SANDBOX_MODEL_HOSTS entry that is not a host pattern fails the
// run's proxy: ignored, the gateway's conversation would be substituted.
func TestAnInvalidModelGatewayFailsTheProxy(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_MODEL_HOSTS", "gw.corp/v1")
	drv, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: open`))
	prx, _, _, err := startNetworkProxy(spec, inspectingDriver{drv}, "run-bad-gw", &recordingRewriter{}, func(store.EventType, map[string]any) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "model host #1") {
		if prx != nil {
			_ = prx.Shutdown(context.Background())
		}
		t.Fatalf("startNetworkProxy with an invalid model host: err = %v, want a refusal naming it", err)
	}
}

// The operator's model hosts are validated whenever the run's proxy starts —
// an allowlist without inspection included.
func TestAnInvalidModelGatewayFailsTheProxyWithoutInspection(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_MODEL_HOSTS", "gw.corp/v1")
	drv, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: allowlist
      rules: ["api.github.com"]`))
	prx, _, _, err := startNetworkProxy(spec, drv, "run-bad-gw-2", nil, func(store.EventType, map[string]any) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "model host #1") {
		if prx != nil {
			_ = prx.Shutdown(context.Background())
		}
		t.Fatalf("startNetworkProxy without inspection: err = %v, want the model host refused", err)
	}
}

// ITERION_SANDBOX_INSPECT_MAX_BODY: a byte count or a KiB/MiB/GiB size; unset
// keeps the proxy's default; anything else fails the run's start.
func TestTheInspectionBoundIsReadFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"", 0, true},
		{"268435456", 268435456, true},
		{"256MiB", 256 << 20, true},
		{"1gib", 1 << 30, true},
		{" 512 KiB ", 512 << 10, true},
		{"100B", 100, true},
		{"0", 0, false},
		{"-5", 0, false},
		{"12XB", 0, false},
		{"MiB", 0, false},
		{"99999999999GiB", 0, false},
	} {
		t.Setenv("ITERION_SANDBOX_INSPECT_MAX_BODY", c.raw)
		got, err := sandboxInspectMaxBody()
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%q: %d, %v; want %d (ok %v)", c.raw, got, err, c.want, c.ok)
		}
	}
}

// ITERION_SANDBOX_INSPECT_MAX_BODY reaches the run's proxy: a body over the
// operator's bound is refused (413) there, one under it is not; a value that
// is no size fails the proxy's start, naming the variable.
func TestTheInspectionBoundReachesTheRunsProxy(t *testing.T) {
	drv, err := noop.New()
	if err != nil {
		t.Fatal(err)
	}
	spec := compileSandboxSpec(t, netPolicySource(`    network:
      mode: open`))
	t.Setenv("ITERION_SANDBOX_INSPECT_MAX_BODY", "nope")
	if prx, _, _, err := startNetworkProxy(spec, inspectingDriver{drv}, "run-bad-bound", &recordingRewriter{}, func(store.EventType, map[string]any) error { return nil }, nil); err == nil || !strings.Contains(err.Error(), "ITERION_SANDBOX_INSPECT_MAX_BODY") {
		if prx != nil {
			_ = prx.Shutdown(context.Background())
		}
		t.Fatalf("an invalid bound: err = %v, want a refusal naming the variable", err)
	}
	t.Setenv("ITERION_SANDBOX_INSPECT_MAX_BODY", "32")
	prx, endpoint, caPEM, err := startNetworkProxy(spec, inspectingDriver{drv}, "run-bound", &recordingRewriter{}, func(store.EventType, map[string]any) error { return nil }, nil)
	if err != nil || prx == nil || caPEM == nil {
		t.Fatalf("startNetworkProxy: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	proxyURL, _ := url.Parse("http://iterion:" + tokenFromEndpoint(t, endpoint) + "@" + prx.Addr().String())
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	status := func(n int) int {
		resp, err := client.Post("https://tool-api.invalid/upload", "text/plain", strings.NewReader(strings.Repeat("x", n)))
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(33); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the operator's bound: status %d, want 413", got)
	}
	if got := status(32); got == http.StatusRequestEntityTooLarge {
		t.Errorf("a body at the operator's bound was refused as too large")
	}
}
