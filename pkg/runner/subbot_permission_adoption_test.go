package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// copyOnlySandbox is a copy-based parent sandbox that executes nothing.
type copyOnlySandbox struct {
	mu    sync.Mutex
	execs int
}

func (f *copyOnlySandbox) Driver() string { return "fake-copy" }
func (f *copyOnlySandbox) Command(ctx context.Context, _ []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}
func (f *copyOnlySandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	f.mu.Lock()
	f.execs++
	f.mu.Unlock()
	return sandbox.ExecResult{}, nil
}
func (f *copyOnlySandbox) Cleanup(context.Context) error { return nil }
func (f *copyOnlySandbox) RefreshWorkspaceFile(context.Context, string, []byte) error {
	return nil
}

// agentChildBot is a one-agent child whose node declares what the test says.
func agentChildBot(declared string) string {
	return "schema empty:\n  ok: bool\n\nagent act:\n  model: \"claude-sonnet-4-5\"\n  output: empty\n" + declared +
		"\nworkflow child:\n  entry: act\n  act -> done\n"
}

// TestSubbotRunner_aGateThatCanAskKeepsTheChildOutOfACopyBasedParent: a pod's
// child executor is built with the run's permission (child := *msg). A gate
// that can ask — imposed at launch as much as declared on the node — keeps
// the child out of the parent's copy-based sandbox: parked there, it could
// only be resumed on its own, off the parent's tree. A gate that cannot ask
// (deny) does not.
func TestSubbotRunner_aGateThatCanAskKeepsTheChildOutOfACopyBasedParent(t *testing.T) {
	for _, tc := range []struct {
		name, declared, runLevel string
		adopted                  bool
	}{
		{"ask imposed at launch, nothing declared", "", "ask", false},
		{"ask declared on the node", "  permission: ask\n", "", false},
		{"deny imposed at launch", "", "deny", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st := subbotTestRunner(t)
			dir := t.TempDir()
			parentDir := filepath.Join(dir, "parent")
			writeSubbotFixture(t, parentDir, "main.bot", subbotTestParent)
			childPath := writeSubbotFixture(t, parentDir, "child.bot", agentChildBot(tc.declared))
			msg := &queue.RunMessage{RunID: "run-parent", TenantID: "t1", OwnerID: "u1", BotID: "parent", Permission: tc.runLevel}

			childWf, _, _, err := runview.CompileWorkflowPath(childPath)
			if err != nil {
				t.Fatal(err)
			}
			child := *msg
			child.RunID = "run-child-spec"
			spec, _, err := r.executorSpec(store.WithIdentity(context.Background(), "t1", "u1"), &child, childWf, iterlog.Nop(), nil)
			if err != nil || spec.Permission != tc.runLevel {
				t.Fatalf("precondition: the child's executor spec: Permission=%q err=%v, want the run's %q", spec.Permission, err, tc.runLevel)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			run := r.subbotRunnerFor(msg, parentDir, dir, iterlog.Nop())
			_, _ = run(ctx, runtime.SubbotRequest{
				Source: "child.bot", ParentRunID: msg.RunID, NodeID: "run_ticket", ReattachKey: "run_ticket",
				ParentSandbox: &runtime.SharedSandbox{Run: &copyOnlySandbox{}, WorkspaceFolder: dir},
			})
			idCtx := store.WithIdentity(context.Background(), "t1", "u1")
			ids, err := st.ListRuns(idCtx)
			if err != nil {
				t.Fatal(err)
			}
			adopted, children := false, 0
			for _, id := range ids {
				c, err := st.LoadRun(idCtx, id)
				if err != nil || c.ParentRunID != msg.RunID {
					continue
				}
				children++
				evs, err := st.LoadEvents(idCtx, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range evs {
					if ev.Type == store.EventSandboxShared && ev.Data["adopted"] == true {
						adopted = true
					}
				}
			}
			if children == 0 {
				t.Fatal("precondition: no child run was created")
			}
			if adopted != tc.adopted {
				t.Fatalf("the child adopted into the copy-based parent = %v, want %v", adopted, tc.adopted)
			}
		})
	}
}
