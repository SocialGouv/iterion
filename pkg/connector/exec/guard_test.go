package exec_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
)

// The refusal has to land BEFORE anything is sent, and it has to be about the
// mark and nothing else — so the SAME client, marked, must go through.
// Asserting only the refusal would leave a Call that refuses every client
// looking exactly as correct as one that checks.
func TestAnUnmarkedClientIsRefusedBeforeTheVendorIsReached(t *testing.T) {
	reached := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	pkg := probe(srv.URL)
	op := opOf(t, pkg, "probe.issue.get")
	args := map[string]any{"owner": "acme", "repo": "widgets", "index": 1}

	// Usable in every respect a call cares about. The one thing it lacks is
	// anything having declared it fit to carry a connector call.
	unmarked := &exec.Executor{Client: srv.Client(), UserAgent: "iterion-test"}
	_, err := unmarked.Call(context.Background(), pkg, op, args, creds())
	if err == nil {
		t.Fatal("an unmarked client must be refused: nothing else in the path looks at what the dialer permits")
	}
	if !strings.Contains(err.Error(), "LocalHTTPClient") {
		t.Errorf("err = %v — a refusal that does not name the constructor of a fit client sends the reader looking for the wrong defect", err)
	}
	if reached != 0 {
		t.Errorf("the vendor was reached %d time(s): the refusal must land before the request is sent, or the unguarded call has already happened by the time it is reported", reached)
	}

	// The other direction, on the same client: the rule is about the mark, not
	// about refusing.
	marked := &exec.Executor{Client: exec.MarkGuarded(srv.Client()), UserAgent: "iterion-test"}
	res, err := marked.Call(context.Background(), pkg, op, args, creds())
	if err != nil {
		t.Fatalf("call on a marked client: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want a marked client to go through", res.Err)
	}
	if reached != 1 {
		t.Errorf("the vendor was reached %d time(s), want exactly 1", reached)
	}
}

// forwardingTransport is the shape a later wrapper is expected to have: it
// delegates, and stays transparent about what it wraps.
type forwardingTransport struct{ base http.RoundTripper }

func (t *forwardingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r)
}

func (t *forwardingTransport) Unwrap() http.RoundTripper { return t.base }

// opaqueTransport is the shape that does not.
type opaqueTransport struct{}

func (opaqueTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// A transport wrapper added above the mark must not hide it — and one that
// tells nothing about what it wraps must cost a REFUSAL rather than a silent
// pass, since from the outside it is indistinguishable from a default client.
func TestAWrapperDecidesWhetherTheMarkStaysReachable(t *testing.T) {
	c := exec.MarkGuarded(&http.Client{Transport: http.DefaultTransport})
	if !exec.IsGuarded(c) {
		t.Fatal("MarkGuarded must produce a client IsGuarded accepts, or nothing downstream can be wired at all")
	}

	c.Transport = &forwardingTransport{base: c.Transport}
	if !exec.IsGuarded(c) {
		t.Error("a wrapper that forwards Unwrap must keep the mark reachable: a diagnostic layer changes nothing about the dialer underneath")
	}

	c.Transport = opaqueTransport{}
	if exec.IsGuarded(c) {
		t.Error("a transport that says nothing about what it wraps must not be taken for a guarded one")
	}
}

// A nil client was already refused; the mark must not turn that into a panic on
// the way to the message.
func TestTheMarkIsSafeOnNothing(t *testing.T) {
	if exec.MarkGuarded(nil) != nil {
		t.Error("marking nothing must stay nothing")
	}
	if exec.IsGuarded(nil) {
		t.Error("nothing is not guarded")
	}
	if exec.IsGuarded(&http.Client{}) {
		t.Error("a zero client uses http.DefaultTransport, which is the unguarded one")
	}

	// And it must not mark what the caller did not hand over. A client passed
	// here can be one the caller does not own — http.DefaultClient is the case
	// that made this matter — and marking it in place would put the mark on
	// every unrelated request in the process, which is the opposite of what
	// this rule is for.
	original := &http.Client{Transport: http.DefaultTransport}
	if marked := exec.MarkGuarded(original); !exec.IsGuarded(marked) {
		t.Fatal("the returned client must carry the mark")
	}
	if exec.IsGuarded(original) {
		t.Error("MarkGuarded must leave its argument alone")
	}
}
