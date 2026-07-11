package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
