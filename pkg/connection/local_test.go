package connection_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// A self-hosted Forgejo is the whole point of the connector this catalog
// ships, and it lives on a loopback or an RFC1918 address. These tests drive
// the PRODUCTION client — the one pkg/cli wires into every run — at a real
// loopback server, because the defect they guard was invisible to any test
// that built its own client.

func TestLocalHTTPClientRefusesAPrivateHostByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(connection.AllowPrivateHostsEnv, "")

	_, err := connection.LocalHTTPClient().Get(srv.URL)
	if err == nil {
		t.Fatal("a loopback host must be refused while the hatch is closed")
	}
	// The refusal has to carry its own remedy: naming the address and not the
	// way to permit it is what left an operator with a self-hosted instance
	// and no next step.
	if !strings.Contains(err.Error(), connection.AllowPrivateHostsEnv) {
		t.Errorf("the refusal must name %s, got: %v", connection.AllowPrivateHostsEnv, err)
	}
}

func TestLocalHTTPClientReachesAPrivateHostWhenPermitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	t.Setenv(connection.AllowPrivateHostsEnv, "1")

	resp, err := connection.LocalHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("the hatch is open, so the call must go out: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d — the answer must be the server's own", resp.StatusCode, http.StatusTeapot)
	}
}

// Only "1" opens it. A truthy-looking value that does not is the failure mode
// where an operator believes the hatch is open and reads the guard's refusal
// as a bug in the connector.
func TestLocalHTTPClientHatchTakesOnlyOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	for _, v := range []string{"true", "yes", "0"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(connection.AllowPrivateHostsEnv, v)
			if _, err := connection.LocalHTTPClient().Get(srv.URL); err == nil {
				t.Fatalf("%s=%q must not open the guard", connection.AllowPrivateHostsEnv, v)
			}
		})
	}
}

// A host that does not resolve must NOT be told to open a security guard: the
// hint is for an address the guard refused, not for a typo.
func TestLocalHTTPClientDoesNotSuggestTheHatchForAnUnresolvableHost(t *testing.T) {
	t.Setenv(connection.AllowPrivateHostsEnv, "")
	_, err := connection.LocalHTTPClient().Get("http://no-such-host.invalid/v1")
	if err == nil {
		t.Fatal("an unresolvable host must fail")
	}
	if strings.Contains(err.Error(), connection.AllowPrivateHostsEnv) {
		t.Errorf("a DNS failure must not name %s, got: %v", connection.AllowPrivateHostsEnv, err)
	}
}
