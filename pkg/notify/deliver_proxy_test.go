package notify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// A credentialed ambient proxy for the whole test binary, planted before any
// transport can freeze net/http's once-read proxy environment: a delivery
// transport that consulted the environment (the #2337 leak) picks THIS proxy
// up, and the witness below sees its credential on the receiver. The proxy
// address is never dialed — the pinned dialer connects to the vetted target
// regardless.
func TestMain(m *testing.M) {
	os.Setenv("HTTP_PROXY", "http://proxy-user:proxy-secret@127.0.0.1:1")
	os.Setenv("HTTPS_PROXY", "http://proxy-user:proxy-secret@127.0.0.1:1")
	os.Exit(m.Run())
}

// #2337: behind a credentialed ambient proxy, a run-chosen plain-http
// callback received the operator's Proxy-Authorization — the pinned dialer
// connected to the TARGET while net/http wrote a proxy-form request for the
// PROXY's address. The delivery transport never consults a proxy now: the
// receiver must see an origin-form POST and no proxy credential, with the
// binary-wide credentialed proxy (TestMain) in force. The callback host is
// a non-loopback NAME (mapped onto the receiver by the injected resolver)
// because a loopback literal is exactly what the ambient-proxy rules would
// bypass, hiding the leak. Restoring http.ProxyFromEnvironment on the
// delivery transport turns both assertions red.
func TestDeliver_NeverLeaksProxyAuthorization(t *testing.T) {
	type sighting struct{ uri, proxyAuth string }
	seen := make(chan sighting, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- sighting{uri: r.RequestURI, proxyAuth: r.Header.Get("Proxy-Authorization")}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	port := receiver.Listener.Addr().(*net.TCPAddr).Port

	n := New(nil, 2*time.Second,
		WithAllowPrivate(true),
		WithResolver(newStubResolver("127.0.0.1")),
	)
	n.deliver(context.Background(),
		fmt.Sprintf("http://callback.receiver.example:%d/cb", port),
		CompletionPayload{RunID: "r"})

	s := <-seen
	if s.uri != "/cb" {
		t.Fatalf("receiver saw request URI %q, want origin-form /cb — a proxy-form request means an ambient proxy shaped the delivery", s.uri)
	}
	if s.proxyAuth != "" {
		t.Fatalf("receiver got Proxy-Authorization %q — the operator's proxy credentials leaked to a run-chosen target", s.proxyAuth)
	}
}
