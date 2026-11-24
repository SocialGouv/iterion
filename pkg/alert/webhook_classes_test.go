package alert

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// The alert sink's guard is never strict — its URL is operator-pinned
// deployment config, an internal receiver legitimate — so the shared (CGNAT)
// space and a non-public IPv6 transition address pass it, read past the
// guard on a dial whose context is already canceled (no packet leaves); an
// IPv4-compatible address, which no strictness admits, does not.
func TestWebhookSinkGuardIsNeverStrict(t *testing.T) {
	sink := NewWebhookSink("http://100.64.0.1:9/hook", nil)
	tr, ok := sink.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the sink's transport is %T; want the guarded *http.Transport", sink.client.Transport)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, target := range []string{"100.64.0.1:9", "[64:ff9b::7f00:1]:9"} {
		if _, err := tr.DialContext(canceled, "tcp", target); !errors.Is(err, context.Canceled) || errors.Is(err, httpdial.ErrNotPublic) {
			t.Errorf("dial %s = %v; want the dialer's cancellation, past the guard", target, err)
		}
	}
	if _, err := tr.DialContext(canceled, "tcp", "[::127.0.0.1]:9"); err == nil || !strings.Contains(err.Error(), "IPv4-compatible") {
		t.Errorf("dial [::127.0.0.1]:9 = %v; want the guard's IPv4-compatible refusal", err)
	}
}
