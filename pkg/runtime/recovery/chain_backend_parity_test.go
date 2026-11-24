package recovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// Backend parity on the usage window (#3032, #3127): the same z.ai [1308]
// refusal must park the run whichever backend served the route. claw meets
// it as an HTTP 429 or a stream error frame, codex as a limit notice in its
// result text, a host relay as flattened text — and the chain reading, the
// single-route classification and the in-node retry all key on the typed
// usage window the claude_code and pi backends raise. Every claw route here
// is produced by the REAL claw backend (model.ClawBackend.Execute) against a
// provider client that refuses, and classified by the REAL recovery
// classifier.

const zaiWindowWords = "Usage limit reached for 5 hour. Your limit will reset at 2026-11-04 00:01:00"

// refusingClient is a provider that refuses every call — with an HTTP error,
// or with a stream whose only frame is an error.
type refusingClient struct {
	mu    sync.Mutex
	err   error
	frame string
	calls int
}

func (c *refusingClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	ch := make(chan api.StreamEvent, 1)
	ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: c.frame}
	close(ch)
	return ch, nil
}

// clawRoute runs the claw backend once against client and returns the error
// its Execute hands the executor — the route's error as the chain records it.
func clawRoute(t *testing.T, client *refusingClient) error {
	t.Helper()
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	b := model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{})
	_, err := b.Execute(context.Background(), delegate.Task{NodeID: "review", Model: "anthropic/claude-opus-5-5", UserPrompt: "review the diff"})
	if err == nil {
		t.Fatal("the claw route succeeded against a provider that refuses every call")
	}
	return err
}

func clawHTTPWindow(t *testing.T) error {
	return clawRoute(t, &refusingClient{err: &api.APIError{Provider: "anthropic", StatusCode: 429, Message: "[1308][" + zaiWindowWords + "]", Retryable: true}})
}

// clawOpenAIFrameWindow is the facade's refusal on an OpenAI-wire error frame,
// which names its own code (1308) — no HTTP status.
func clawOpenAIFrameWindow(t *testing.T) error {
	return clawRoute(t, &refusingClient{frame: `openai stream error: {"error":{"code":"1308","message":"` + zaiWindowWords + `"}}`})
}

// clawAnthropicFrameWindow is the same refusal on an Anthropic-wire error
// frame, which carries the provider's message alone.
func clawAnthropicFrameWindow(t *testing.T) error {
	return clawRoute(t, &refusingClient{frame: zaiWindowWords})
}

// codexWindow is codex's plan-window notice, typed by the definition codex's
// terminal-failure reading shares with every backend
// (delegate.RateLimitedFromRefusal; codexTerminalFailure's own witness is in
// pkg/backend/delegate).
func codexWindow() error {
	return delegate.RateLimitedFromRefusal(delegate.BackendCodex, "You've hit your usage limit. Try again later.", time.Now())
}

// flattenedWindow is a typed refusal a hop formatted with %v: only its
// rendering — the rate_limited marker and the provider's words — survives.
func flattenedWindow() error {
	typed := &delegate.ErrRateLimited{Provider: "zai", Kind: delegate.RateLimitKindUsageWindow, Detail: "[1308][" + zaiWindowWords + "]"}
	return fmt.Errorf("claw backend: runner: %v", typed)
}

// spendCeiling and fairUsage are refusals of the ACCOUNT: no reset is named,
// and no wait is known to cure them — a human raises the ceiling, or the
// provider relents on its own clock.
func spendCeiling() error {
	return &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Kind: delegate.RateLimitKindUsageWindow,
		Detail: "You've hit your org's monthly spend limit · ask your admin to raise it at claude.ai/settings/usage"}
}

func fairUsage() error {
	return &delegate.ErrRateLimited{Provider: "zai", Kind: delegate.RateLimitKindUsageWindow,
		Detail: "Your account's request frequency has been limited under our fair usage policy"}
}

func TestClassify_AClawRouteOnAUsageWindowParksOnIt(t *testing.T) {
	for name, route := range map[string]func(*testing.T) error{
		"http 429":             clawHTTPWindow,
		"openai-wire frame":    clawOpenAIFrameWindow,
		"anthropic-wire frame": clawAnthropicFrameWindow,
	} {
		err := route(t)
		if !delegate.IsUsageWindow(err) {
			t.Errorf("%s: the claw backend returned %T %v, want the typed usage window every backend raises", name, err, err)
		}
		if got := Classify(fmt.Errorf("model: node %q: %w", "review", err)); got != runtime.ErrCodeUsageLimitBlocked {
			t.Errorf("%s: a single claw route on the window → %s, want %s — the durable park, not an in-node backoff into a five-hour wall", name, got, runtime.ErrCodeUsageLimitBlocked)
		}
	}
}

// The window shuts for hours: the claw backend's own retry loop does not
// re-issue the request within seconds of it.
func TestClawBackend_AUsageWindowIsNotRetriedInTheNode(t *testing.T) {
	client := &refusingClient{err: &api.APIError{Provider: "anthropic", StatusCode: 429, Message: "[1308][" + zaiWindowWords + "]", Retryable: true}}
	_ = clawRoute(t, client)
	if client.calls != 1 {
		t.Fatalf("the provider was called %d times, want 1 — a usage window is not a throttle", client.calls)
	}
	throttle := &refusingClient{err: &api.APIError{Provider: "anthropic", StatusCode: 429, Message: "rate limit exceeded, slow down", Retryable: true}}
	_ = clawRoute(t, throttle)
	if throttle.calls < 2 {
		t.Fatalf("a throttle 429 was called %d times — the in-node retry still owns a plain throttle", throttle.calls)
	}
}

func TestClassify_AUsageWindowFromAnyBackendWinsTheChainInEveryOrder(t *testing.T) {
	for name, window := range map[string]func() error{
		"claw http 429":             func() error { return clawHTTPWindow(t) },
		"claw openai-wire frame":    func() error { return clawOpenAIFrameWindow(t) },
		"claw anthropic-wire frame": func() error { return clawAnthropicFrameWindow(t) },
		"codex limit notice":        codexWindow,
		"flattened by a hop":        flattenedWindow,
	} {
		for _, multiset := range [][]func() error{
			{window, chainDeadCredential},
			{window, chainThrottle, chainDeadCredential},
		} {
			for _, order := range permutations(multiset) {
				routes := make([]error, len(order))
				labels := make([]string, len(order))
				for i, fn := range order {
					routes[i] = fn()
					labels[i] = firstWord(routes[i].Error())
				}
				if got := Classify(engineWrapped(routes...)); got != runtime.ErrCodeUsageLimitBlocked {
					t.Errorf("%s: chain %s → %s, want %s", name, strings.Join(labels, " | "), got, runtime.ErrCodeUsageLimitBlocked)
				}
			}
		}
	}
}

// An account refusal names no reset and no wait cures it: beside a dead
// credential it does not turn the human's pause into hourly blind parks —
// the human the pause calls can revive the dead route. Beside a window a
// wait does cure, the window still decides; alone, it parks as before.
func TestClassify_AnAccountRefusalDoesNotOutrankADeadCredential(t *testing.T) {
	cases := []struct {
		name   string
		routes []func() error
		want   runtime.ErrorCode
	}{
		{"spend ceiling + dead credential", []func() error{spendCeiling, chainDeadCredential}, runtime.ErrCodeAuthFailed},
		{"fair usage + dead credential", []func() error{fairUsage, chainDeadCredential}, runtime.ErrCodeAuthFailed},
		{"spend ceiling + window + dead credential", []func() error{spendCeiling, chainUsageWindow, chainDeadCredential}, runtime.ErrCodeUsageLimitBlocked},
		{"spend ceiling + throttle", []func() error{spendCeiling, chainThrottle}, runtime.ErrCodeUsageLimitBlocked},
	}
	for _, tc := range cases {
		for _, order := range permutations(tc.routes) {
			routes := make([]error, len(order))
			labels := make([]string, len(order))
			for i, fn := range order {
				routes[i] = fn()
				labels[i] = firstWord(routes[i].Error())
			}
			if got := Classify(engineWrapped(routes...)); got != tc.want {
				t.Errorf("%s: chain %s → %s, want %s", tc.name, strings.Join(labels, " | "), got, tc.want)
			}
		}
	}
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, ":"); i > 0 {
		return s[:i]
	}
	return s
}

var _ = errors.New
