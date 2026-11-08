package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestBuildTask_TheMemoryWithdrawalIsAnnouncedWhereMemoryIsHanded.
//
// A bot's `memory:` block is the tenant's space, and only the claw backend
// hands it to an agent. On a workspace its tenant did not write, a claw node
// says once that it runs without it; a node on another backend was never
// going to be handed it, and announcing a withdrawal there would tell the
// operator something that did not happen. The trusted claw node is the
// control that the announcement is keyed on the trust at all.
func TestBuildTask_TheMemoryWithdrawalIsAnnouncedWhereMemoryIsHanded(t *testing.T) {
	for _, row := range []struct {
		name    string
		trust   store.RunTrust
		backend string
		want    int
	}{
		{"fork, claw", store.RunTrustFork, delegate.BackendClaw, 1},
		{"trusted, claw (the control)", store.RunTrustDefault, delegate.BackendClaw, 0},
		{"fork, claude_code", store.RunTrustFork, delegate.BackendClaudeCode, 0},
		{"fork, pi", store.RunTrustFork, delegate.BackendPi, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			var got []string
			e := &ClawExecutor{logger: iterlog.Nop()}
			e.hooks.OnTrustWithdrawal = func(_ string, info TrustWithdrawalInfo) { got = append(got, info.What) }
			e.SetWorkspaceTrust(row.trust)
			node := &ir.AgentNode{}
			node.ID = "n"
			f := backendFields{id: "n", model: "anthropic/claude-bench", memory: &ir.Memory{Enabled: true, Scope: "notes", Read: true}}
			if _, err := e.buildTask(context.Background(), node, f, map[string]any{}, row.backend, &nodeBuildSession{}); err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			n := 0
			for _, w := range got {
				if w == "memory" {
					n++
				}
			}
			if n != row.want {
				t.Errorf("memory withdrawals announced = %d (all: %v), want %d", n, got, row.want)
			}
		})
	}
}
