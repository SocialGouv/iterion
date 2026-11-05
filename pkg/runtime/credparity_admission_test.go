package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestCredParity_Admission drives every row of ADR-161's parity table
// (internal/credparity) through the start site its run passes and judges the
// launch's verdict against the row — the row's call site of its kind, which
// the admission reads through model.CallSites: the production
// resolveAndStartSandbox for a run of its own — `sandbox: none` on the host,
// an `auto` sandbox on the canary driver whose Prepare refuses otherwise —
// under the routes its dispatch reads (the run's provider allowlist), and
// startSandbox's adopt branch for a subbot child under a sandboxed parent. A run ADR-161 judges
// (the fork lane, every stamped run) is refused, with the code decision 7
// assigns, exactly where no leg funds its node; every other start proceeds.
//
// The run credentials lookups are the runner's (cmd/iterion installs them at
// boot): the claw arm's in-process walk reads the run's keys through them.
func TestCredParity_Admission(t *testing.T) {
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
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
			// The row's call site, of its kind: an agent node, an LLM router,
			// a human node's LLM half, a Verified Action's rung.
			node := row.Node.IR()

			var err error
			var events []store.Event
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
				if row.Role == credparity.SupervisorRole {
					// The supervisor alone: its evaluation is the row's call.
					wf.Nodes = map[string]ir.Node{}
					wf.Supervisors = []*ir.Supervisor{{Name: parityRowSupervisor, Model: row.Node.Model}}
				}
				trust := store.RunTrustDefault
				if row.Trust == credparity.Fork {
					trust = store.RunTrustFork
				}
				// The dispatch's own pick for a claw node naming no model:
				// the executor's detector, under the run's allowlist.
				detected := model.NewClawExecutor(model.NewRegistry(), wf, model.WithAllowedProviders(row.Run.Allowlist)).DetectedClawSpec
				err = credentialAdmissionDetecting(t, ctx, wf, trust, model.RouteView{AllowedProviders: row.Run.Allowlist}, detected, &events)
			}

			got := "launches"
			var rerr *RuntimeError
			switch {
			case err == nil, errors.Is(err, errCanaryStop):
				if row.Role == credparity.SupervisorRole && warnsOn(events, parityRowSupervisor) {
					got = "launches, warned"
				}
			case errors.As(err, &rerr):
				got = "refused " + string(rerr.Code)
			default:
				t.Fatalf("the start failed past any verdict: %v", err)
			}
			credparity.Check(t, row, credparity.Admission, got, func(s credparity.Spend) string {
				switch {
				case !row.Judged() || !s.Refused():
					return "launches"
				case row.Role == credparity.SupervisorRole:
					// A supervisor the resolver refuses is warned, never
					// the launch's refusal (ADR-161 decision 4).
					return "launches, warned"
				}
				return "refused " + credparity.AdmissionCode(s.Refusal)
			})
		})
	}
}

// parityRowSupervisor names a supervisor row's declared supervisor.
const parityRowSupervisor = "parity_supervisor"

// warnsOn reports whether a sandbox_no_credential_warning names name.
func warnsOn(events []store.Event, name string) bool {
	for _, e := range events {
		if e.Type != store.EventSandboxNoCredentialWarning {
			continue
		}
		if reason, _ := e.Data["reason"].(string); strings.Contains(reason, name) {
			return true
		}
	}
	return false
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
