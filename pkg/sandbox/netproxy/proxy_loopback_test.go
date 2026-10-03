package netproxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The egress allowlist governs DESTINATIONS — and the proxy host's own
// loopback is not one of them: a proxied request to a loopback or
// link-local address would reach the PROXY HOST's local services, so it is
// refused before it dials. The sanctioned host alias (host.docker.internal)
// is exempt.
func TestLoopbackDestinationRefusal(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "127.1.2.3", "169.254.169.254", "fe80::1", "localhost", "foo.localhost"} {
		if err := loopbackDestinationRefusal(host); err == nil {
			t.Errorf("%s must be refused (it names the proxy host's own loopback)", host)
		}
	}
	for _, host := range []string{"8.8.8.8", "example.com", "host.docker.internal", "api.github.com"} {
		if err := loopbackDestinationRefusal(host); err != nil {
			t.Errorf("%s must NOT be refused: %v", host, err)
		}
	}
}

// End to end through a proxy on its DEFAULT dialer: a plain-HTTP request to
// a loopback destination is answered with the refusal (the dial error
// surfaces in the 502 body), not silently dialed against the proxy host.
func TestLoopbackDestinationRefusedThroughTheProxy(t *testing.T) {
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := New(Options{Policy: pol, InspectCA: ca})
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
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + prx.Addr().String())
		},
	}}
	resp, err := client.Get("http://127.0.0.1:9/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "loopback or link-local") {
		t.Fatalf("a loopback destination: status %d, body %q; want 502 naming the refusal", resp.StatusCode, string(body))
	}
}
