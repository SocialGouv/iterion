package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestIsDelegateRetryable_Classification is the consolidated truth table for
// the LAYER-1 in-executor retry classifier. It pins exactly which backend
// failures are retried in-executor (genuinely transient: rate-limit,
// session-limit, idle-watchdog, network/5xx, signal kills) versus which fail
// loud so a logic error / misconfig is never silently retried forever.
func TestIsDelegateRetryable_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// --- typed transient signals -------------------------------------
		{"typed ErrTransient network", &delegate.ErrTransient{Provider: delegate.BackendClaudeCode, Reason: "network"}, true},
		{"typed ErrRateLimited (session/forfait quota)", &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Detail: "hit your session limit"}, true},

		// --- network / connectivity markers ------------------------------
		{"dns flap", errors.New("dial tcp: lookup api.anthropic.com: no such host"), true},
		{"connection reset mid-request", errors.New("read: connection reset by peer"), true},
		{"failed to open socket (anthropic CLI)", errors.New("API Error: Unable to connect to API (FailedToOpenSocket)"), true},
		{"i/o timeout", errors.New("dial tcp 1.2.3.4:443: i/o timeout"), true},

		// --- process-level transient -------------------------------------
		{"signal kill (OOM/SIGTERM)", errors.New("signal: killed"), true},
		{"exit 137 (128+SIGKILL)", errors.New("exit status 137"), true},
		{"exit 143 (128+SIGTERM)", errors.New("exit status 143"), true},
		{"failed to start (resource exhaustion)", errors.New("fork/exec: failed to start subprocess"), true},
		{"reading stdout (broken pipe)", errors.New("reading stdout: broken pipe"), true},
		{"idle watchdog abort", errors.New("claude session idle for 4m0s (thinking phase) — aborting"), true},

		// --- terminal: must NOT retry ------------------------------------
		{"nil error", nil, false},
		{"exit 1 application error", errors.New("exit status 1"), false},
		{"exit 2 misuse", errors.New("exit status 2"), false},
		{"exit 127 command not found", errors.New("exit status 127"), false},
		{"schema validation (wrong shape)", errors.New("missing required field stats"), false},
		{"plain logic error", errors.New("node produced an invalid verdict"), false},
		{"auth rejected (not transient)", errors.New("API error 401: authentication token is expired"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDelegateRetryable(c.err); got != c.want {
				t.Errorf("isDelegateRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestRetryPolicyFromEnv verifies the env knobs map onto the attempt budget
// (retries+1) and that unset / malformed values fall back to the defaults
// rather than silently disabling retries.
func TestRetryPolicyFromEnv(t *testing.T) {
	cases := []struct {
		name          string
		std           string // ITERION_NODE_MAX_RETRIES
		transient     string // ITERION_NODE_MAX_TRANSIENT_RETRIES
		wantStd       int    // effectiveMaxAttempts for a non-network error
		wantTransient int    // effectiveMaxAttempts for a network error
	}{
		{"unset → defaults", "", "", DefaultMaxAttempts, DefaultMaxAttemptsTransient},
		{"explicit both", "4", "9", 5, 10},
		{"zero retries = fail-fast", "0", "0", 1, 1},
		{"malformed ignored → default", "abc", "-3", DefaultMaxAttempts, DefaultMaxAttemptsTransient},
	}

	networkErr := errors.New("dial tcp: no such host")
	otherErr := errors.New("signal: killed")

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ITERION_NODE_MAX_RETRIES", c.std)
			t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", c.transient)
			rp := RetryPolicyFromEnv()
			if got := rp.effectiveMaxAttempts(otherErr); got != c.wantStd {
				t.Errorf("standard budget = %d, want %d", got, c.wantStd)
			}
			if got := rp.effectiveMaxAttempts(networkErr); got != c.wantTransient {
				t.Errorf("transient budget = %d, want %d", got, c.wantTransient)
			}
		})
	}
}

// ensure the exit-code helper matches the doc'd 128+ boundary at the edges.
func TestExtractExitCode_Boundary(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want int
	}{
		{"exit status 127", 127},
		{"exit status 128", 128},
		{fmt.Sprintf("exit status %d", 130), 130},
		{"no code here", -1},
	} {
		if got := extractExitCode(c.msg); got != c.want {
			t.Errorf("extractExitCode(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}
