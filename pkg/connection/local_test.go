package connection_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
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

// TestTheGuardsRefusalIsNotAnUndecidedMutation.
//
// The refusal above is the FIRST thing a self-hosted Forgejo meets, and until
// this client said so the executor read it as "the request was sent and no
// answer came back": `unknown_outcome`, no retry, AMBIGUOUS_EFFECT, a run
// parked terminally and off the auto-resume list — on a call that never left
// the machine. The executor recognises an ordinary DNS or dial failure by
// type; the guard's own refusal is an opaque string, so the fact is attached
// where it is known, in the dialer this client owns.
//
// Driven through LocalHTTPClient against a real loopback server, because a
// client built by the test would not have the wrapper under test.
func TestTheGuardsRefusalIsNotAnUndecidedMutation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guard is closed, so the vendor must never be reached")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(connection.AllowPrivateHostsEnv, "")

	pkg := tierPackage(srv.URL, "POST", "/repos/{owner}/{repo}/issues/comments")
	op, ok := pkg.Operation("probe.issue.comment")
	if !ok {
		t.Fatal("probe.issue.comment is absent from the fixture")
	}
	if !op.Effect.Mutating() {
		t.Fatal("the fixture must MUTATE, or the classification under test never runs")
	}
	args := map[string]any{"owner": "acme", "repo": "widgets"}

	e := &exec.Executor{Client: connection.LocalHTTPClient()}
	res, err := e.Call(context.Background(), pkg, op, args, exec.Credential{SchemeID: "token", Value: "s3cret"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Err == nil {
		t.Fatal("the guard is closed, so the call must fail")
	}
	if res.Err.Class == spec.ErrUnknownOutcome || res.Err.AmbiguousEffect() {
		t.Errorf("err = %v — a call the guard refused never left the host, so there is no effect to reconcile", res.Err)
	}
	if !res.Err.NotSent {
		t.Error("the guard's refusal must reach the executor as a fact, not as an opaque string it has to match on")
	}
	if !res.Err.Retryable(op, args) {
		t.Error("nothing was sent, so repeating the call duplicates nothing")
	}
	// And the remedy still travels: classifying it correctly must not cost the
	// operator the one line that says how to permit their own instance.
	if !strings.Contains(res.Err.Cause.Error(), connection.AllowPrivateHostsEnv) {
		t.Errorf("cause = %v, want it to still name %s", res.Err.Cause, connection.AllowPrivateHostsEnv)
	}
}
