//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/liveledger"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestLive_Feat_ClaudeCodeBackgroundWork proves, against the
// REAL CLI, what only a real model can: a node whose agent launches a
// background subagent and ends its turn gets the subagent's result into its
// final structured output — and, with the lifecycle off, does not (the
// pre-fix behaviour, measured on a Doki run as 4 of 5 wasted passes). The
// second half is the witness: without it a green first half could mean the
// model happened to wait in-turn.
//
// ITERION_LIVE_CLAUDE_CLI pins the binary — point it at the version the
// runner image pins (docker/llm-clis/package.json); `claude` on PATH
// otherwise. ITERION_LIVE_CLAUDE_MODEL overrides the cheap default.
//
// Cost: two short haiku sessions, ~$0.4 a run.
func TestLive_Feat_ClaudeCodeBackgroundWork(t *testing.T) {
	_ = liveledger.Track(t) // #1422: record last-green ledger row for this target on t.Cleanup
	cli := os.Getenv("ITERION_LIVE_CLAUDE_CLI")
	if cli == "" {
		p, err := exec.LookPath("claude")
		if err != nil {
			t.Skip("no claude CLI: set ITERION_LIVE_CLAUDE_CLI or put claude on PATH")
		}
		cli = p
	}
	model := os.Getenv("ITERION_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	run := func(t *testing.T, lifecycle string) (answer string, phases []delegate.BackgroundPhase) {
		// Subagents run in the foreground unless background work is asked
		// for; this test is about background work.
		t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_TASKS", "on")
		t.Setenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE", lifecycle)
		nonce := fmt.Sprintf("NONCE-%d", time.Now().UnixNano())
		var mu sync.Mutex
		task := delegate.Task{
			NodeID:  "live-bg",
			Command: cli,
			Model:   model,
			WorkDir: t.TempDir(),
			UserPrompt: "Step 1: call the Agent tool ONCE with subagent_type 'general-purpose', run_in_background true, " +
				"description 'fetcher', prompt: 'Run the bash command `sleep 5 && echo " + nonce + "` and reply with its exact output only.'. " +
				"Step 2: right after launching it, end your turn by replying only the word WAITING — do not wait for it and do not call TaskOutput. " +
				"Step 3: when you are later notified that the agent finished, report its exact output as `answer`.",
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
		}
		task.Hooks.OnBackgroundWork = func(w delegate.BackgroundWork) {
			mu.Lock()
			defer mu.Unlock()
			phases = append(phases, w.Phase)
		}
		b := &delegate.ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, io.Discard)}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := b.Execute(ctx, task)
		if err != nil {
			// A failed call proves nothing either way — least of all the
			// witness, which an error would otherwise turn green.
			t.Fatalf("Execute (%s): %v", lifecycle, err)
		}
		a, _ := res.Output["answer"].(string)
		t.Logf("lifecycle=%s answer=%q phases=%v cost=%v terminated=%v",
			lifecycle, a, phases, res.Output["_cost_usd"], res.TerminatedBackgroundTasks)
		if !strings.Contains(a, nonce) {
			return "", phases
		}
		return a, phases
	}

	t.Run("lifecycle on: the final report carries the background agent's result", func(t *testing.T) {
		answer, phases := run(t, "on")
		if answer == "" {
			t.Fatalf("the final answer does not carry the background agent's output (phases %v)", phases)
		}
	})
	t.Run("lifecycle off: the witness — the result dies with the session", func(t *testing.T) {
		if answer, _ := run(t, "off"); answer != "" {
			t.Fatalf("with the lifecycle off the answer still carried the agent's output (%q): the model waited in-turn, and the first half proves nothing", answer)
		}
	})
}
