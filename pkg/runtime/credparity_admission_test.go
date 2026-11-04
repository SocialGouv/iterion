package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestCredParity_Admission drives every row of ADR-161's parity table
// (internal/credparity) through the start site its run passes and judges the
// launch's verdict against the row: the production resolveAndStartSandbox for
// a run of its own — `sandbox: none` on the host, an `auto` sandbox on the
// canary driver whose Prepare refuses otherwise — and startSandbox's adopt
// branch for a subbot child under a sandboxed parent. A run ADR-161 judges
// (the fork lane, every stamped run) is refused, with the code decision 7
// assigns, exactly where no leg funds its node; every other start proceeds.
func TestCredParity_Admission(t *testing.T) {
	for _, row := range credparity.Rows() {
		t.Run(row.Label(), func(t *testing.T) {
			clearCredentialEnv(t)
			for _, name := range credparity.PostureNames {
				t.Setenv(name, "")
			}
			for name, v := range row.Posture {
				t.Setenv(name, v)
			}
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			forfaitDir := ""
			if row.Run.Forfait {
				forfaitDir = t.TempDir()
				if err := credparity.WriteForfait(forfaitDir); err != nil {
					t.Fatal(err)
				}
			}
			ctx := secrets.WithCredentials(context.Background(), row.Run.Credentials(forfaitDir))
			node := &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: row.Node.ID},
				LLMFields: ir.LLMFields{Backend: row.Node.Backend, Model: row.Node.Model, Provider: row.Node.Provider},
			}

			var err error
			if row.Adopted {
				err = adoptedParityStart(t, ctx, node)
			} else {
				mode := sandbox.ModeNone
				if row.Sandboxed {
					mode = sandbox.ModeAuto
				}
				wf := &ir.Workflow{
					Name:    "credential_parity",
					Sandbox: &ir.SandboxSpec{Mode: string(mode), HostState: "none"},
					Nodes:   map[string]ir.Node{row.Node.ID: node},
				}
				trust := store.RunTrustDefault
				if row.Trust == credparity.Fork {
					trust = store.RunTrustFork
				}
				err = credentialAdmissionRunCtx(t, ctx, wf, trust, nil)
			}

			got := "launches"
			var rerr *RuntimeError
			switch {
			case err == nil, errors.Is(err, errCanaryStop):
			case errors.As(err, &rerr):
				got = "refused " + string(rerr.Code)
			default:
				t.Fatalf("the start failed past any verdict: %v", err)
			}
			credparity.Check(t, row, credparity.Admission, got, func(s credparity.Spend) string {
				if row.Judged() && s.Refused() {
					return "refused " + credparity.AdmissionCode(s.Refusal)
				}
				return "launches"
			})
		})
	}
}

// adoptedParityStart starts a subbot child holding node in its parent's live
// sandbox: the child engine settles on the parent's handle.
func adoptedParityStart(t *testing.T, ctx context.Context, node ir.Node) error {
	t.Helper()
	st := tmpStore(t)
	workDir := t.TempDir()
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{node.NodeID(): node}}
	e := New(wf, st, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()},
		WithWorkDir(workDir),
		WithParentRunID("run-parent"),
		WithSharedSandbox(&SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: t.TempDir(), SharedStateDir: "/shared"}),
	)
	e.resourceScope = &runResourceScope{path: workDir, borrowed: true, owned: map[string]string{}}
	if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
		t.Fatal(err)
	}
	cleanup, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
	cleanup()
	if err == nil && !e.sandboxSettled {
		t.Fatal("inert bench: the child neither adopted the parent's sandbox nor failed")
	}
	return err
}
