package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func repairScripts(t *testing.T, commands []string) [][]api.StreamEvent {
	t.Helper()
	scripts := make([][]api.StreamEvent, 0, len(commands))
	for _, command := range commands {
		body, err := json.Marshal(map[string]string{"corrected_command": command})
		if err != nil {
			t.Fatal(err)
		}
		scripts = append(scripts, toolUseEvents("repair", "structured_output", string(body), 1, 1))
	}
	return scripts
}

// Both counters matter: a comparison after execution can stop generations while
// already having repeated the effect. Every command appends one real byte, and
// the self_repair tool hook independently identifies corrected executions.
func TestVerifiedAction_RepairProgressAndBudget(t *testing.T) {
	t.Parallel()
	constant := "printf x >> effects; printf same"
	movingOut := "printf x >> effects; wc -c < effects"
	movingErr := "printf x >> effects; wc -c < effects >&2"
	movingTail := "printf x >> effects; printf '%05000d' 0; wc -c < effects"
	for _, tc := range []struct {
		name, initial, command         string
		limit, generations, executions int
		distinct, trim, stopped        bool
	}{
		{name: "unchanged", initial: constant, command: constant, limit: 5, generations: 2, executions: 1, stopped: true},
		{name: "last_command_changes", initial: constant + " # original", command: constant, limit: 5, generations: 3, executions: 2, stopped: true},
		{name: "distinct_budget", initial: constant, command: constant, limit: 3, generations: 3, executions: 3, distinct: true},
		{name: "stdout_changes", initial: movingOut, command: movingOut, limit: 3, generations: 3, executions: 3},
		{name: "stderr_changes", initial: movingErr, command: movingErr, limit: 3, generations: 3, executions: 3},
		{name: "evidence_past_prompt_cutoff", initial: movingTail, command: movingTail, limit: 3, generations: 3, executions: 3},
		{name: "executed_command_is_trimmed", initial: constant, command: constant, limit: 5, generations: 2, executions: 1, stopped: true, trim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			commands := make([]string, tc.limit+1)
			for i := range commands {
				commands[i] = tc.command
				if tc.distinct {
					commands[i] += fmt.Sprintf(" # proposal%d", i)
				}
				if tc.trim && i%2 == 1 {
					commands[i] = " \n" + commands[i] + "\n "
				}
			}
			mock := newMockClient(repairScripts(t, commands)...)
			reg := NewRegistry()
			reg.Register("progress_probe", func(string) (api.APIClient, error) { return mock, nil })
			var logs bytes.Buffer
			correctedExecutions := 0
			exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir),
				WithLogger(iterlog.New(iterlog.LevelInfo, &logs)),
				WithEventHooks(EventHooks{OnToolCall: func(_ string, info LLMToolCallInfo) {
					if info.ToolName == "self_repair:repair" {
						correctedExecutions++
					}
				}}))
			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "repair"}, Command: tc.initial,
				Postcondition: "false", Policy: ir.PolicyRecover,
				Recovery: &ir.RecoverySpec{Model: "progress_probe/fake", MaxRepairAttempts: tc.limit},
			}
			_, err := exec.Execute(t.Context(), node, nil)
			if err == nil || !strings.Contains(err.Error(), "postcondition not met (policy: recover)") {
				t.Fatalf("unmet goal must reach policy failure: %v", err)
			}
			effects, readErr := os.ReadFile(filepath.Join(dir, "effects"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(mock.getCalls()) != tc.generations || correctedExecutions != tc.executions || len(effects) != 1+tc.executions {
				t.Fatalf("generations=%d corrected executions=%d shell effects=%d, want %d/%d/%d", len(mock.getCalls()), correctedExecutions, len(effects), tc.generations, tc.executions, 1+tc.executions)
			}
			stop := "proposal and failure evidence unchanged; duplicate command not executed"
			if strings.Contains(logs.String(), stop) != tc.stopped {
				t.Fatalf("progress-stop diagnostic present=%v, want %v", strings.Contains(logs.String(), stop), tc.stopped)
			}
		})
	}
}

func TestVerifiedAction_RepairHistoryIsInvocationLocalAndAgentIsOptIn(t *testing.T) {
	t.Parallel()
	for _, agent := range []bool{false, true} {
		t.Run(fmt.Sprintf("agent_%v", agent), func(t *testing.T) {
			dir := t.TempDir()
			command := "printf x >> effects; printf same"
			scripts := repairScripts(t, []string{command, command, command, command})
			runs, attempts, wantCalls := 2, 0, 4
			if agent {
				scripts = append(scripts[:2], textEvents("agent finished", 1, 1))
				runs, attempts, wantCalls = 1, 1, 3
			}
			mock := newMockClient(scripts...)
			reg := NewRegistry()
			reg.Register("progress_probe", func(string) (api.APIClient, error) { return mock, nil })
			var delegates []string
			exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir), WithEventHooks(EventHooks{
				OnDelegateStarted: func(id string, _ DelegateInfo) { delegates = append(delegates, id) },
			}))
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "repair"}, Command: command,
				Postcondition: "false", Policy: ir.PolicyRecover,
				Recovery: &ir.RecoverySpec{Model: "progress_probe/fake", MaxRepairAttempts: 5, MaxAgentAttempts: attempts}}
			for range runs {
				if _, err := exec.Execute(t.Context(), node, nil); err == nil {
					t.Fatal("unmet goal was accepted")
				}
			}
			effects, err := os.ReadFile(filepath.Join(dir, "effects"))
			if err != nil {
				t.Fatal(err)
			}
			if len(mock.getCalls()) != wantCalls || len(effects) != runs*2 || len(delegates) != attempts {
				t.Fatalf("calls=%d effects=%d delegates=%v, want %d/%d/%d", len(mock.getCalls()), len(effects), delegates, wantCalls, runs*2, attempts)
			}
			if agent && delegates[0] != "repair__recover" {
				t.Fatalf("wrong recovery node: %v", delegates)
			}
		})
	}
}

func TestSelfRepairFingerprintPreservesEachEvidenceField(t *testing.T) {
	t.Parallel()
	base := [4]string{"command", "stdout", "stderr", "previous"}
	fingerprint := func(v [4]string) string { return selfRepairFingerprint(v[0], v[1], v[2], v[3]) }
	for i := range base {
		changed := base
		changed[i] += "\x00\xff\n"
		if fingerprint(changed) == fingerprint(base) {
			t.Fatalf("field %d does not affect proposal identity", i)
		}
	}
	for _, pair := range [][2][4]string{
		{{"a", "bc", "", ""}, {"ab", "c", "", ""}},
		{{"cmd", "\xff", "", ""}, {"cmd", "\xfe", "", ""}},
		{{"cmd", strings.Repeat("x", 5000) + "A", "", ""}, {"cmd", strings.Repeat("x", 5000) + "Z", "", ""}},
	} {
		if fingerprint(pair[0]) == fingerprint(pair[1]) {
			t.Fatal("distinct evidence was collapsed")
		}
	}
}
