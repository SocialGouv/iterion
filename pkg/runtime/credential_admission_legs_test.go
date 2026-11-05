package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// clawAgent is a claw agent node on model under the raw provider field.
func clawAgent(id, spec, provider string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: "claw", Model: spec, Provider: provider},
	}
}

// A judged run with several nodes no credential funds is refused once,
// naming every such node — sorted, each with its backend and the reason its
// chain ends on — and carrying the first one's code; a funded node beside
// them is not named. Mutant — the admission returning on the first refused
// node: the second name is missing.
func TestTheAdmissionNamesEveryRefusedNodeSorted(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod-admission")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "zai-run"},
	})
	wf := &ir.Workflow{
		Name:    "named",
		Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto), HostState: "none"},
		Nodes: map[string]ir.Node{
			"zeta":  clawAgent("zeta", "anthropic/claude-sonnet-4", ""),
			"alpha": clawAgent("alpha", "anthropic/claude-sonnet-4", ""),
			"glm":   clawAgent("glm", "anthropic/glm-5.3", ""),
		},
	}
	err := credentialAdmissionStart(t, ctx, wf, store.RunTrustFork, model.RouteView{})
	var rerr *RuntimeError
	if !errors.As(err, &rerr) {
		t.Fatalf("a stamped run with two unfunded nodes = %v, want the typed refusal", err)
	}
	if rerr.Code != ErrCodeCredentialAmbientRefused || rerr.NodeID != "alpha" {
		t.Errorf("code %q node %q, want %q on the first node in order (alpha)", rerr.Code, rerr.NodeID, ErrCodeCredentialAmbientRefused)
	}
	a, z := strings.Index(rerr.Message, "alpha (claw: ambient-stamp)"), strings.Index(rerr.Message, "zeta (claw: ambient-stamp)")
	if a < 0 || z < 0 || a > z {
		t.Errorf("the refusal names %q — want alpha then zeta, each with its backend and reason", rerr.Message)
	}
	if strings.Contains(rerr.Message, "glm") {
		t.Errorf("the refusal names the node its run's z.ai key funds: %q", rerr.Message)
	}
	var refusal *credroute.Refusal
	if !errors.As(err, &refusal) || refusal.Reason != credroute.ReasonAmbientStamp {
		t.Errorf("the refusal does not unwrap to the first node's resolver refusal: %v", err)
	}
}

// A `fallbacks:` route no credential funds, and a supervisor whose own spec
// none funds, are warned at launch — the node's own chain decides the
// admission, so the run is not refused for them. Mutant — the admission
// skipping the fallback (or the supervisor) reading: no warning names it.
func TestTheAdmissionWarnsOnARefusedFallbackAndSupervisor(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
	})
	node := clawAgent("impl", "anthropic/claude-sonnet-4", "")
	node.Fallbacks = []ir.Fallback{{Name: "rescue", Model: "openai/gpt-5.5"}}
	wf := &ir.Workflow{
		Name:        "warned",
		Sandbox:     &ir.SandboxSpec{Mode: string(sandbox.ModeNone)},
		Nodes:       map[string]ir.Node{"impl": node},
		Supervisors: []*ir.Supervisor{{Name: "watch", Model: "openai/gpt-5.5"}},
	}
	var events []store.Event
	err := credentialAdmissionRunRoutes(t, ctx, wf, store.RunTrustDefault, model.RouteView{}, &events)
	if err != nil {
		t.Fatalf("a stamped run whose node its own key funds = %v, want the launch to proceed", err)
	}
	var warned []string
	for _, ev := range events {
		if ev.Type == store.EventSandboxNoCredentialWarning {
			if names, ok := ev.Data["nodes"].([]string); ok {
				warned = append(warned, names...)
			}
		}
	}
	joined := strings.Join(warned, " | ")
	if !strings.Contains(joined, `impl fallback "rescue" (claw: ambient-stamp)`) {
		t.Errorf("no warning names the refused fallback route: %q", joined)
	}
	if !strings.Contains(joined, `supervisor "watch" (openai/gpt-5.5: ambient-stamp)`) {
		t.Errorf("no warning names the refused supervisor: %q", joined)
	}
}

// A subbot child adopting its parent's sandbox is judged on the home mount
// that sandbox carries (SharedSandbox.CredentialHome): a fork-lane child whose
// claw node reaches its credential only through the operator's ~/.codex the
// parent mounted is admitted, and refused where the parent mounted nothing.
// Mutant — the adopt branch's admission reading no home: the mounted arm is
// refused.
func TestAnAdoptedChildIsJudgedOnItsParentsHomeMount(t *testing.T) {
	clearCredentialEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{})
	for _, tc := range []struct {
		name  string
		home  credroute.SandboxHomeState
		admit bool
	}{
		{"the parent mounts the operator's home", credroute.SandboxHomeState{Mounted: true, HomeDir: home}, true},
		{"the parent mounts nothing of it", credroute.SandboxHomeState{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tmpStore(t)
			workDir := t.TempDir()
			wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{"impl": clawAgent("impl", "openai/gpt-5.5", "")}}
			e := New(wf, st, &sandboxCapturingExecutor{stubExecutor: newStubExecutor()},
				WithWorkDir(workDir),
				WithParentRunID("run-parent"),
				WithTrust(store.RunTrustFork, ""),
				WithSharedSandbox(&SharedSandbox{Run: &sharedFakeRun{}, WorkspaceFolder: t.TempDir(), SharedStateDir: "/shared", CredentialHome: tc.home}),
			)
			e.resourceScope = &runResourceScope{path: workDir, borrowed: true, owned: map[string]string{}}
			if _, err := st.CreateRun(ctx, "run-child", "child", nil); err != nil {
				t.Fatal(err)
			}
			cleanup, err := e.startSandbox(ctx, "run-child", workDir, "", nil)
			cleanup()
			var rerr *RuntimeError
			switch {
			case tc.admit && err != nil:
				t.Fatalf("the child reaching the parent's mounted login = %v, want it adopted", err)
			case !tc.admit && (!errors.As(err, &rerr) || rerr.Code != ErrCodeSandboxCredentialUnreachable):
				t.Fatalf("the child with no channel = %v, want %s", err, ErrCodeSandboxCredentialUnreachable)
			}
		})
	}
}

// The interim guard's refusal at the launch (ADR-161 decision 9): a stamped
// run's pi node is refused CREDENTIAL_AMBIENT_REFUSED with the guard's own
// remedy — the arm-pending reason named, the node routed to claw or
// claude_code, never "attach a credential", which pi would not spend — and
// its cause unwraps to the stamp's sentinel. An own-config node on the same
// run refuses with the stamp's remedy, which names its backend family. An
// unstamped run's own-config node is admitted with the warning naming it.
func TestTheGuardAndTheOwnConfigArmsRefuseAtTheLaunchWithTheirRemedies(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod-admission")
	stamped := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	node := func(backend, spec string) *ir.Workflow {
		return &ir.Workflow{
			Name:    "guard",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeNone), HostState: "none"},
			Nodes: map[string]ir.Node{"impl": &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: backend, Model: spec},
			}},
		}
	}

	err := credentialAdmissionStart(t, stamped, node("pi", "anthropic/claude-sonnet-4"), store.RunTrustDefault, model.RouteView{})
	var rerr *RuntimeError
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeCredentialAmbientRefused {
		t.Fatalf("a stamped pi node = %v, want CREDENTIAL_AMBIENT_REFUSED", err)
	}
	if !errors.Is(err, credroute.ErrAmbientRefused) {
		t.Errorf("the guard's refusal does not unwrap to credroute.ErrAmbientRefused: %v", err)
	}
	for _, want := range []string{"impl (pi: arm-pending)", "ADR-161 U4"} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("the refusal %q does not name %q", rerr.Message, want)
		}
	}
	if !strings.Contains(rerr.Hint, "claw or claude_code") || strings.Contains(rerr.Hint, "attach a credential") {
		t.Errorf("the guard's remedy %q is not its own — route the node, never attach a credential pi would not spend", rerr.Hint)
	}

	err = credentialAdmissionStart(t, stamped, node("kimi", "kimi-k2"), store.RunTrustDefault, model.RouteView{})
	if !errors.As(err, &rerr) || rerr.Code != ErrCodeCredentialAmbientRefused || !strings.Contains(rerr.Message, "impl (kimi: ambient-stamp)") {
		t.Fatalf("a stamped kimi node = %v, want CREDENTIAL_AMBIENT_REFUSED naming it ambient-stamp", err)
	}
	if !strings.Contains(rerr.Hint, "kimi, grok, opencode") {
		t.Errorf("the stamp's remedy %q does not tell an own-config backend from a missing credential", rerr.Hint)
	}

	var events []store.Event
	if err := credentialAdmissionRunRoutes(t, secrets.WithCredentials(context.Background(), secrets.Credentials{}), func() *ir.Workflow {
		wf := node("kimi", "kimi-k2")
		wf.Sandbox.Mode = string(sandbox.ModeAuto)
		return wf
	}(), store.RunTrustFork, model.RouteView{}, &events); err != nil && !errors.Is(err, errCanaryStop) {
		t.Fatalf("an unstamped fork-lane kimi node = %v, want it admitted", err)
	}
	if !warnsOn(events, "impl (kimi)") {
		t.Errorf("an unstamped own-config node was admitted unwarned: %+v", events)
	}
}
