package delegate

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A delegation that ends badly still SPENT. The caps, the fallback chain's
// carried spend and a donor's ledger all read the cost from the output map,
// so a terminal return that skips the stamp records nothing — the money is
// gone either way, only the accounting disappears. `typedFailure` is the
// choke point that stamps it, and two terminal returns used to walk past it.
func TestTerminalReturnsCarryTheirSpend(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	task := Task{NodeID: "n", Iteration: 1, Model: "claude-fable-5"}
	billed := &claudesdk.ResultMessage{
		SessionID:    "s1",
		Usage:        &claudesdk.Usage{InputTokens: 900, OutputTokens: 120},
		TotalCostUSD: f(0.42),
	}
	backend := func() *ClaudeCodeBackend {
		return &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	}

	t.Run("a stream that broke after being billed", func(t *testing.T) {
		res, err := backend().buildStreamErrorResult(billed, sessionMeta{}, errors.New("session ended without result"),
			"fetch failed", 3*time.Second, task)
		if err == nil {
			t.Fatal("a broken stream must still return an error")
		}
		if res.Output == nil || res.Output["_cost_usd"] == nil || res.Output["_tokens"] == nil {
			t.Fatalf("the session was billed and the spend went unrecorded: %v", res.Output)
		}
		if got, _ := res.Output["_cost_usd"].(float64); got < 0.42 {
			t.Fatalf("the CLI's own figure was lost: _cost_usd=%v (want >= 0.42)", res.Output["_cost_usd"])
		}
		if res.Tokens != 1020 {
			t.Fatalf("tokens: got %d, want 1020", res.Tokens)
		}
	})

	t.Run("a stream that broke before any message", func(t *testing.T) {
		// No result message at all: nothing to bill, and nothing to crash on.
		res, err := backend().buildStreamErrorResult(nil, sessionMeta{}, errors.New("spawn failed"),
			"", time.Second, task)
		if err == nil {
			t.Fatal("want an error")
		}
		if res.Output == nil {
			t.Fatal("the output map must exist even at zero, or the caps read nothing")
		}
		if res.Tokens != 0 {
			t.Fatalf("tokens invented from nothing: %d", res.Tokens)
		}
	})

	t.Run("a hard CLI subtype is billed like a rendered refusal", func(t *testing.T) {
		// The path Execute takes: the result carries the pass's usage, and
		// the subtype is fatal.
		rm := &claudesdk.ResultMessage{IsError: true, Subtype: claudesdk.ResultErrorExecution,
			Usage: billed.Usage, TotalCostUSD: billed.TotalCostUSD}
		result := Result{ExitCode: 0, Tokens: 1020}
		errResult, errOut, fatal := backend().handleCLIErrorSubtype(rm, task, result, 900, 120)
		if !fatal || errOut == nil {
			t.Fatalf("error_during_execution must stay fatal: fatal=%v err=%v", fatal, errOut)
		}
		if errResult.Output == nil || errResult.Output["_cost_usd"] == nil || errResult.Output["_tokens"] == nil {
			t.Fatalf("a fatal subtype without its spend: %v", errResult.Output)
		}
		if got, _ := errResult.Output["_cost_usd"].(float64); got < 0.42 {
			t.Fatalf("the CLI's own figure was lost: %v", errResult.Output["_cost_usd"])
		}
	})

	t.Run("max turns stays a soft stop", func(t *testing.T) {
		rm := &claudesdk.ResultMessage{IsError: true, Subtype: claudesdk.ResultErrorMaxTurns}
		_, errOut, fatal := backend().handleCLIErrorSubtype(rm, task, Result{}, 0, 0)
		if fatal || errOut != nil {
			t.Fatalf("max_turns must stay a partial result, not a failure: fatal=%v err=%v", fatal, errOut)
		}
	})
}
