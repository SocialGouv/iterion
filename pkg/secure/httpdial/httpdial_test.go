package httpdial

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.0.0.5", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"169.254.169.254", false}, // AWS/Azure metadata
		{"169.254.1.2", false},     // link-local
		{"100.100.100.200", false}, // Alibaba metadata
		{"0.0.0.0", false},         // unspecified
		{"224.0.0.1", false},       // multicast
		{"::1", false},
		{"fe80::1", false},
		{"fd00::1", false}, // ULA
		{"2606:4700:4700::1111", true},
		{"2001:4860:4860::8888", true},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", tc.ip)
		}
		if got := IsPublicUnicast(ip); got != tc.want {
			t.Errorf("IsPublicUnicast(%s)=%v want %v", tc.ip, got, tc.want)
		}
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, b := range []string{"", "127.0.0.1", "localhost", "::1", "[::1]"} {
		if !IsLoopbackBind(b) {
			t.Errorf("IsLoopbackBind(%q) should be true", b)
		}
	}
	for _, b := range []string{"0.0.0.0", "10.0.0.1", "example.com"} {
		if IsLoopbackBind(b) {
			t.Errorf("IsLoopbackBind(%q) should be false", b)
		}
	}
}

func TestResolvePublicHost_Literals(t *testing.T) {
	ctx := context.Background()
	// Public literal passes strict.
	if _, err := ResolvePublicHost(ctx, "8.8.8.8", true); err != nil {
		t.Errorf("public literal rejected: %v", err)
	}
	// Private literal rejected strict, allowed non-strict.
	if _, err := ResolvePublicHost(ctx, "10.0.0.1", true); err == nil {
		t.Errorf("private literal should be rejected in strict mode")
	}
	if _, err := ResolvePublicHost(ctx, "10.0.0.1", false); err != nil {
		t.Errorf("private literal should pass in non-strict mode: %v", err)
	}
	// Cluster-internal alias refused in strict mode without DNS.
	for _, h := range []string{"foo.svc.cluster.local", "kubernetes.default", "metadata.google.internal"} {
		if _, err := ResolvePublicHost(ctx, h, true); err == nil {
			t.Errorf("alias %q should be refused in strict mode", h)
		}
	}
	// Empty host always errors.
	if _, err := ResolvePublicHost(ctx, "", false); err == nil {
		t.Errorf("empty host should error")
	}
}

func TestSafeClient_RejectsPrivateDial(t *testing.T) {
	client := SafeClient(true, 0)
	// Dialing a private literal must be refused by the guarded transport.
	_, err := client.Get("http://10.1.2.3/")
	if err == nil {
		t.Fatalf("expected dial to private IP to be refused")
	}
	if !strings.Contains(err.Error(), "public unicast") {
		t.Errorf("expected public-unicast refusal, got %v", err)
	}
}

// PlainDialer is SafeTransport's dialer without the guard: its promise to a
// caller dialing a proxy is SafeTransport's bounds — a connect timeout and
// a TCP keep-alive.
func TestPlainDialer_KeepsSafeTransportsBounds(t *testing.T) {
	d := PlainDialer()
	if d.Timeout <= 0 {
		t.Errorf("PlainDialer().Timeout = %v, want a connect bound", d.Timeout)
	}
	if d.KeepAlive <= 0 {
		t.Errorf("PlainDialer().KeepAlive = %v, want SafeTransport's keep-alive period", d.KeepAlive)
	}
}

func TestSafeClient_NoRedirectFollow(t *testing.T) {
	c := SafeClient(true, 0)
	if c.CheckRedirect == nil {
		t.Fatal("SafeClient must set CheckRedirect")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("expected ErrUseLastResponse, got %v", err)
	}
}
