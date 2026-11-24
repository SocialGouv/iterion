package httpdial

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// A credentialed ambient proxy for the whole test binary, planted before any
// transport can freeze net/http's once-read proxy environment: a guarded
// transport that consulted the environment (the #2337 / #2338 regression)
// picks THIS proxy up, and the witnesses below see its credential or its
// steering. The proxy address itself is never dialed — the pinned dialers
// connect to their validated target regardless.
func TestMain(m *testing.M) {
	os.Setenv("HTTP_PROXY", "http://proxy-user:proxy-secret@127.0.0.1:1")
	os.Setenv("HTTPS_PROXY", "http://proxy-user:proxy-secret@127.0.0.1:1")
	os.Exit(m.Run())
}

// #2337: a pinned delivery never crosses a proxy. Under one, the dialer
// would connect to the pinned target while net/http wrote a proxy-form
// request for the PROXY's address — handing the run-chosen receiver the
// ambient proxy's Proxy-Authorization, readable and replayable against the
// real proxy. The witness replays exactly that route: with the binary-wide
// credentialed proxy (TestMain) in force, the target must see an
// origin-form request and no proxy credential. Restoring
// http.ProxyFromEnvironment on PinnedTransport turns both assertions red
// (the request arrives as "GET http://run-chosen.example/..." with the
// Basic credential attached).
func TestPinnedTransport_PinsTheDialAndLeaksNoProxyCredential(t *testing.T) {
	type sighting struct{ uri, proxyAuth string }
	seen := make(chan sighting, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- sighting{uri: r.RequestURI, proxyAuth: r.Header.Get("Proxy-Authorization")}
	}))
	defer srv.Close()

	// The request names a host that resolves nowhere: only the pinned dial
	// can connect it, so a served response proves the pin.
	client := &http.Client{Transport: PinnedTransport(srv.Listener.Addr().String())}
	resp, err := client.Get("http://run-chosen.example/callback")
	if err != nil {
		t.Fatalf("pinned delivery failed: %v", err)
	}
	defer resp.Body.Close()

	s := <-seen
	if s.uri != "/callback" {
		t.Fatalf("target saw request URI %q, want origin-form /callback — a proxy-form request means an ambient proxy shaped the delivery", s.uri)
	}
	if s.proxyAuth != "" {
		t.Fatalf("target received Proxy-Authorization %q — the operator's proxy credentials leaked to a run-chosen receiver", s.proxyAuth)
	}
}
