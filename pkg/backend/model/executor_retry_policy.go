package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ErrCompactionUnsupported is the sentinel ClawExecutor.Compact returns
// when the backend has no in-process conversation handle to drop. The
// runtime re-exports it (runtime.ErrCompactionUnsupported is an alias)
// so the engine's `errors.Is` check works without importing model
// directly. Lives here because runtime imports model, not the reverse.
var ErrCompactionUnsupported = errors.New("model: compaction not supported by executor")

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

// DefaultMaxAttempts is the default number of LLM call attempts (initial + retries).
const DefaultMaxAttempts = 3

// DefaultMaxAttemptsTransient is the attempt budget for connectivity/transient
// failures. Larger than DefaultMaxAttempts so a brief internet/API outage is
// ridden out rather than aborting a long run: with a 1s base and the capped
// exponential backoff below, 6 attempts span roughly a minute of retrying.
const DefaultMaxAttemptsTransient = 6

// DefaultBackoffBase is the base duration for exponential backoff.
const DefaultBackoffBase = time.Second

// defaultRouterModel is the last-resort model for LLM routers when no model
// is configured and ITERION_DEFAULT_SUPERVISOR_MODEL is unset. Routing
// decisions are lightweight, so a fast/cheap model is sufficient.
const defaultRouterModel = "anthropic/claude-sonnet-5"

// RetryPolicy controls automatic retry on transient LLM errors.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts (1 = no retry). Default: 3.
	MaxAttempts int
	// MaxAttemptsTransient is the attempt budget for connectivity/transient
	// failures (network blips, upstream 5xx). Default: 6. Falls back to
	// max(MaxAttempts, DefaultMaxAttemptsTransient) so it is never smaller
	// than the standard budget.
	MaxAttemptsTransient int
	// BackoffBase is the base delay for exponential backoff. Default: 1s.
	BackoffBase time.Duration
}

// RetryPolicyFromEnv builds the in-executor per-node retry budget from the
// environment, falling back to the built-in defaults (DefaultMaxAttempts /
// DefaultMaxAttemptsTransient) for any dimension left unset. This is the
// LAYER-1 knob: it bounds how many times a transient backend failure
// (rate-limit, session-limit, idle-watchdog, network/5xx) is retried
// IN-EXECUTOR with exponential backoff before the failure bubbles up to a
// run-level failed_resumable (which the LAYER-2 auto-resume then handles).
//
// The env values are RETRY counts (they exclude the initial attempt), so
// ITERION_NODE_MAX_TRANSIENT_RETRIES=8 yields 9 attempts. A value of 0 means
// "no retry" (fail-fast); a negative or non-numeric value is ignored (keeps
// the default) rather than silently disabling retries.
//
//   - ITERION_NODE_MAX_TRANSIENT_RETRIES → the connectivity/transient budget
//     (network blips, upstream 5xx, rate-limit, idle hang). Default: 5 retries
//     (6 attempts).
//   - ITERION_NODE_MAX_RETRIES → the standard budget for deterministic-but-
//     retryable errors (signal kill). Default: 2 retries (3 attempts).
func RetryPolicyFromEnv() RetryPolicy {
	rp := RetryPolicy{}
	if n, ok := envRetryCount("ITERION_NODE_MAX_RETRIES"); ok {
		rp.MaxAttempts = n + 1
	}
	if n, ok := envRetryCount("ITERION_NODE_MAX_TRANSIENT_RETRIES"); ok {
		rp.MaxAttemptsTransient = n + 1
	}
	return rp
}

// envRetryCount reads a non-negative retry count from env. Returns (0, false)
// when unset, blank, non-numeric, or negative — the caller then keeps the
// built-in default rather than treating a typo as "disable retries".
func envRetryCount(key string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (rp RetryPolicy) maxAttempts() int {
	if rp.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return rp.MaxAttempts
}

// maxAttemptsTransient returns the retry budget for transient/connectivity
// errors. An explicit value wins (clamped up so it is never smaller than the
// standard budget). When unset, it inflates to DefaultMaxAttemptsTransient
// ONLY if MaxAttempts is also unset — the production-default path. A caller
// that pinned MaxAttempts (a fail-fast config, or a test) keeps that cap
// rather than having network errors silently retried beyond what was asked.
func (rp RetryPolicy) maxAttemptsTransient() int {
	if rp.MaxAttemptsTransient > 0 {
		n := rp.MaxAttemptsTransient
		if std := rp.maxAttempts(); n < std {
			n = std
		}
		return n
	}
	if rp.MaxAttempts > 0 {
		return rp.maxAttempts()
	}
	return DefaultMaxAttemptsTransient
}

// effectiveMaxAttempts picks the attempt budget for err: the larger transient
// budget for network/connectivity failures, the standard budget otherwise.
func (rp RetryPolicy) effectiveMaxAttempts(err error) int {
	if delegate.IsNetworkError(err) {
		return rp.maxAttemptsTransient()
	}
	return rp.maxAttempts()
}

func (rp RetryPolicy) backoffBase() time.Duration {
	if rp.BackoffBase <= 0 {
		return DefaultBackoffBase
	}
	return rp.BackoffBase
}

// backoff returns the delay for attempt n (0-indexed) with jitter.
func (rp RetryPolicy) backoff(attempt int) time.Duration {
	base := float64(rp.backoffBase()) * math.Pow(2, float64(attempt))
	maxDelay := float64(60 * time.Second)
	if base > maxDelay {
		base = maxDelay
	}
	// Jitter: 0.5x to 1.5x.
	jitter := 0.5 + rand.Float64()
	return time.Duration(base * jitter)
}

// ---------------------------------------------------------------------------
// Interaction errors
// ---------------------------------------------------------------------------

// ErrNeedsInteraction is returned by the executor when a delegate or LLM
// signals that it needs user input to continue. The runtime engine should
// handle this by pausing (interaction: human), auto-responding (interaction: llm),
// or deciding (interaction: llm_or_human) based on the node's InteractionMode.
type ErrNeedsInteraction struct {
	NodeID    string
	Questions map[string]any // question_key → question text
	SessionID string         // delegate session ID for re-invocation
	Backend   string         // delegate backend name (empty for claw direct)

	// Conversation is the persisted backend-specific conversation history
	// captured at the pause point (claw: marshalled []api.Message). The
	// runtime relays this opaque blob into the checkpoint so that resume
	// can rehydrate the LLM's mid-tool-loop state instead of restarting
	// from system+user prompts. Backends that cannot persist conversation
	// state (CLI: claude_code, codex) leave this nil.
	Conversation json.RawMessage
	// PendingToolUseID is the ID of the tool_use block in Conversation
	// that is awaiting an answer. Required when Conversation is non-nil.
	PendingToolUseID string

	// SessionStateBlob is a packed CLI session (ADR-089). Transient: the
	// runtime Puts it then drops the slice. Never logged or checkpointed.
	SessionStateBlob []byte `json:"-"`
	// SessionStateRef is set when reconstructing ni from a pause checkpoint.
	SessionStateRef string `json:"-"`
}

func (e *ErrNeedsInteraction) Error() string {
	return fmt.Sprintf("model: node %q needs user interaction (%d questions)", e.NodeID, len(e.Questions))
}
