package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestSessionResumeCapabilityHasAPacker: `session: persist` carries a
// conversation across visits by packing the CLI's transcript after each one,
// so a backend admitted with no session root to pack from would degrade every
// visit to a fresh session. claw is the exception: it packs its own envelope.
func TestSessionResumeCapabilityHasAPacker(t *testing.T) {
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow())
	backends := []string{
		delegate.BackendClaw, delegate.BackendClaudeCode, delegate.BackendCodex,
		delegate.BackendPi, delegate.BackendOpenCode, delegate.BackendKimi, delegate.BackendGrok,
	}
	for _, name := range backends {
		_, supported := exec.SessionResumeCapability(verdictJudge(name))
		if !supported || name == delegate.BackendClaw {
			continue
		}
		task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}
		if root := delegate.SessionFilesRoot(context.Background(), task, name); root == "" {
			t.Errorf("session: persist is admitted on %q, which has no session root to pack its transcript from", name)
		}
	}
}
