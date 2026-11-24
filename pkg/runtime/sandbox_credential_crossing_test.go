package runtime

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The proxy's half of the crossing (ADR-162 decision 7), driven through the
// REAL netproxy: the rewriter the engine arms (composeCredentialCrossing over
// credentialCrossings) substitutes the container's placeholder for the real
// key toward the credential's OWN host, and refuses a request that carries
// anything but the placeholder — a foreign key or a smuggled value, toward
// the provider host itself or any other. The credential is a gateway pair
// whose URL names example.com, the host the httptest certificate serves.
//
// Mutants: MaterializeForHostWithin losing the crossingHostAllowed check →
// the last row; ExfiltratesTo losing the raw-value match → the middle row;
// composeCredentialCrossing swapping nothing → the first.
func TestTheProxySubstitutesThePlaceholderTowardTheCredentialsOwnHost(t *testing.T) {
	const key = "sk-gw-proxy-crossing-witness"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:     map[secrets.Provider]string{secrets.ProviderOpenAICompatible: key},
		APIBaseURLs: map[secrets.Provider]string{secrets.ProviderOpenAICompatible: "https://example.com"},
	})
	ph := secrets.CredentialPlaceholder(key)
	if ph == "" {
		t.Fatal("the witness key derives no placeholder")
	}
	rw := composeCredentialCrossing(nil, credentialCrossings(ctx))
	if rw == nil {
		t.Fatal("the engine composed no rewriter for a run with a crossing credential")
	}

	var gotAuth atomic.Value
	gotAuth.Store("")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())

	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatalf("NewEphemeralCA: %v", err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	prx, err := netproxy.New(netproxy.Options{
		Policy: pol, InspectCA: ca, Rewriter: rw,
		InspectUpstreamTLS: &tls.Config{RootCAs: upstreamPool},
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) {
			return net.Dial(network, upstream.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("netproxy.New: %v", err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("client failed to trust the ephemeral CA")
	}
	proxyURL, _ := url.Parse("http://" + prx.Addr().String())
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientPool}},
	}

	// 1) The container's request — the placeholder the spawn env crossed —
	//    reaches the credential's own host authenticating as the real key.
	req, _ := http.NewRequest("GET", "https://example.com/v1/chat", nil)
	req.Header.Set("Authorization", "Bearer "+ph)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("the placeholder's request never reached upstream: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if auth, _ := gotAuth.Load().(string); auth != "Bearer "+key {
		t.Errorf("upstream Authorization = %q, want the substituted real key — the proxy did not materialise the placeholder toward the credential's own host", auth)
	}

	// 2) A request authenticating with anything but the placeholder — here
	//    the RAW value, a key the container must not hold — is refused, not
	//    forwarded: toward the credential's own host it is a foreign key.
	req, _ = http.NewRequest("GET", "https://example.com/v1/chat", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("the raw value's request: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a request authenticating with the raw value toward the credential's host answered %d, want 403 — only the placeholder may authenticate", res.StatusCode)
	}

	// 3) Host scoping: the placeholder substitutes toward no host the
	//    credential's egress table does not name (the network floor refuses
	//    the dial before it happens; the rewriter must not resolve the token
	//    even if one slips through). A parent-domain look-alike is the
	//    hostile case the exact/suffix match exists to refuse.
	for _, host := range []string{"api.attacker.tld", "example.com.attacker.tld", "notexample.com"} {
		if out, ok := rw.MaterializeForHostWithin("Bearer "+ph, host, 1<<20); !ok || out != "Bearer "+ph {
			t.Errorf("toward %s: out = %q (ok=%v), want the placeholder untouched — a host the credential does not name resolves nothing", host, out, ok)
		}
	}
	if !strings.Contains(ph, "__ITERION_SECRET_") {
		t.Errorf("placeholder %q lost its marker shape", ph)
	}
}
