package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
)

// fakeTimeoutErr implements net.Error with Timeout()==true to exercise the
// canonical-interface branch of IsNetworkError without a real socket.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o deadline reached" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

func TestIsNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled (self-inflicted)", context.Canceled, false},
		{"context deadline (own deadline)", context.DeadlineExceeded, false},
		{"plain schema error", errors.New("missing required field candidates"), false},
		{"permanent exit 1", errors.New("exit status 1"), false},
		{"command not found", errors.New("exit status 127"), false},

		{"net.Error timeout", fakeTimeoutErr{}, true},
		{"ECONNRESET errno", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"ECONNREFUSED errno", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"ETIMEDOUT errno", &net.OpError{Op: "read", Err: syscall.ETIMEDOUT}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},

		{"dial no such host", errors.New(`Post "https://api.anthropic.com/v1/messages": dial tcp: lookup api.anthropic.com: no such host`), true},
		{"node fetch failed", errors.New("fetch failed"), true},
		{"node socket hang up", errors.New("request failed: socket hang up"), true},
		{"node ECONNRESET string", errors.New("read ECONNRESET"), true},
		{"node getaddrinfo EAI_AGAIN", errors.New("getaddrinfo EAI_AGAIN api.anthropic.com"), true},
		{"upstream 503", errors.New("API error: 503 Service Unavailable"), true},
		{"anthropic overloaded", errors.New(`{"type":"overloaded_error"}`), true},
		{"connection reset by peer", errors.New("read tcp 10.0.0.1:443: connection reset by peer"), true},

		{"wrapped network in fmt.Errorf", fmt.Errorf("delegate: claude-code failed: %w", errors.New("fetch failed")), true},
		{"ErrTransient network passes through", &ErrTransient{Provider: BackendClaudeCode, Reason: "network", Detail: "fetch failed"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNetworkError(tc.err); got != tc.want {
				t.Fatalf("IsNetworkError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMatchesNetworkSignature(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"fetch failed", true},
		{"claude-code [stderr]: Error: getaddrinfo ENOTFOUND api.anthropic.com", true},
		{"upstream connect error or disconnect/reset before headers", true},
		{"context canceled", false}, // self-inflicted — never a network fault
		{"missing required field stats", false},
		{"all good here", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.s, func(t *testing.T) {
			if got := MatchesNetworkSignature(tc.s); got != tc.want {
				t.Fatalf("MatchesNetworkSignature(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

// TestMatchesTransportStderr pins the CLI-agent stderr contract: only the
// CLI's OWN transport error report counts — never a stack frame, a
// codeframe quoting source, or an application-level error message that
// happens to contain a signature's prose.
func TestMatchesTransportStderr(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool
	}{
		{
			// Measured on opencode 1.1.19: a credential-less host dies
			// DETERMINISTICALLY with this Bun crash; "unexpected eof"
			// matching was the bug — three retries burned on a config
			// failure, then an *ErrTransient wrap.
			"opencode JSON parse crash (measured)",
			"error: JSON Parse error: Unexpected EOF\n" +
				"      at /$bunfs/root/opencode:2:20\n" +
				"      at loadConfig (/$bunfs/root/opencode:9:11)\n",
			false,
		},
		{
			// A codeframe quotes arbitrary source — even an errno string
			// in quoted source is not a transport fault.
			"errno in quoted source only",
			"  42 |   if (err.code === \"ECONNRESET\") {\n" +
				"                                ^\n" +
				"error: Failed to render preview\n" +
				"      at /x.ts:42:5\n",
			false,
		},
		{
			// An application-level 5xx render is prose, not a transport
			// marker: provider errors reach the retry classifiers through
			// the protocol's structured error event, not a crash dump.
			"app-rendered 503 prose",
			"error: API request failed: 503 Service Unavailable\n" +
				"      at /x.ts:1:1\n",
			false,
		},
		{
			// The genuine undici crash: the column-0 header carries the
			// match; the indented [cause]: chain is read too.
			"undici fetch failed with cause",
			"TypeError: fetch failed\n" +
				"    at Object.fetch (node:internal/deps/undici/undici:11576:11)\n" +
				"  [cause]: Error: connect ECONNREFUSED 140.82.112.5:443\n" +
				"      at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1636:16)\n",
			true,
		},
		{
			// An app-wrapped error whose own header names no marker: the
			// indented [cause]: line still testifies.
			"errno only in the cause chain",
			"Error: request to upstream failed\n" +
				"  [cause]: Error: getaddrinfo ENOTFOUND api.x.ai\n" +
				"      at GetAddrInfoReqWrap.onlookup (node:dns:118:26)\n",
			true,
		},
		{
			// Node's uncaught-exception render echoes the throwing SOURCE
			// line at column 0 (file:line, source verbatim, caret, then the
			// error header). For a minified single-line bundle that echo
			// holds nearly every string literal in the program — "fetch
			// failed" here — and the line-length cap must exclude it. The
			// echo is built at 520 chars, JUST over the cap, so a cap raise
			// past 520 (or its deletion) reddens this case.
			"node minified source echo at column 0",
			"/app/dist/cli.js:1\n" +
				"!function(){\"" + strings.Repeat("a", 490) + "fetch failed\"}();\n" +
				"^\n" +
				"SyntaxError: Unexpected token\n" +
				"    at /app/dist/cli.js:1:3\n",
			false,
		},
		{
			// Boundary: a genuine header of EXACTLY the cap length still
			// classifies (the skip is strictly-greater), so a cap drop
			// below a long DNS-name lookup line reddens this case. The 500
			// is deliberately a literal — building from the constant would
			// make the line track the cap and the case unfalsifiable.
			"genuine header at exactly the cap",
			"Error: getaddrinfo ENOTFOUND " +
				strings.Repeat("a", 500-len("Error: getaddrinfo ENOTFOUND ")) + "\n",
			true,
		},
		{"empty", "", false},
		{"self-inflicted cancel", "error: fetch failed: context canceled\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesTransportStderr(tc.s); got != tc.want {
				t.Fatalf("matchesTransportStderr(%q) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

// TestIsNetworkErrorTypedNotTextual pins the structural layer: typed
// network errors classify as transient by TYPE, independent of their
// message — a wording change upstream (or a non-English locale) must not
// demote a resolver flap or reset to the fatal retry budget.
func TestIsNetworkErrorTypedNotTextual(t *testing.T) {
	cases := []error{
		&net.DNSError{Err: "?", Name: "api.example", IsTemporary: true},
		&net.DNSError{Err: "?", Name: "api.example", IsNotFound: true},
		fmt.Errorf("wrapped: %w", syscall.ENETRESET),
		fmt.Errorf("wrapped: %w", syscall.EHOSTDOWN),
	}
	for _, err := range cases {
		if !IsNetworkError(err) {
			t.Errorf("IsNetworkError(%T %v) = false, want true (typed classification)", err, err)
		}
	}
}
