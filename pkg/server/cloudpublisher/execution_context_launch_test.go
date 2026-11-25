package cloudpublisher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

type contextLaunchStore struct{ store.RunStore }

func (s contextLaunchStore) ContextIdentity(context.Context) (store.ContextRef, error) {
	return store.ContextRef{}, errors.New("test fixture identity failure")
}
func TestExecutionContextCloudRefusesBeforePersistence(t *testing.T) {
	for _, identityFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid context", true: "identity error"}[identityFailure], func(t *testing.T) {
			p, st, ctx := sourceTestPublisher(t)
			published := 0
			p.publishRun = func(context.Context, *queue.RunMessage) error { published++; return nil }
			spec := runview.LaunchSpec{Source: "workflow wf:\n  entry: survey\n  survey -> done\n", FilePath: "wf.bot"}
			expected := "unsupported execution context version"
			if identityFailure {
				p.store = contextLaunchStore{st}
				expected = "test fixture identity failure"
			} else {
				spec.ExecutionContext = &store.ExecutionContext{Version: 2}
			}
			_, err := p.SubmitLaunch(ctx, "context-launch-cloud-refusal", spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "h"})
			doc, loadErr := st.LoadRun(ctx, "context-launch-cloud-refusal")
			t.Logf("error=%v; published=%d; persisted=%v; loadErr=%v", err, published, doc != nil, loadErr)
			if err == nil || !strings.Contains(err.Error(), expected) || published != 0 || doc != nil {
				t.Fatal("cloud refusal contract failed")
			}
		})
	}
}

// TestSubmitLaunchStampsTheResolvedReliabilityMode pins the default policy a
// cloud launch stamps: the resolved rollout mode of the launching process —
// the same resolution the CLI and runview launch surfaces apply, capped per
// tenant by the legacy exemption list — on BOTH carriers the run travels
// with, the queued row and the wire message. A policy declared on the
// launch spec keeps winning over the resolved default.
func TestSubmitLaunchStampsTheResolvedReliabilityMode(t *testing.T) {
	const main = "workflow wf:\n  entry: survey\n  survey -> done\n"

	t.Run("the resolved mode is the stamped default, on row and wire", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			mode     string
			alias    string
			tenants  string
			tenant   string
			declared store.ContextPolicy
			want     store.ContextPolicy
		}{
			{name: "neither variable set", mode: "", alias: "", tenants: "", tenant: "team-a", want: store.ContextPolicyLegacy},
			{name: "mode report", mode: "report", alias: "", tenants: "", tenant: "team-a", want: store.ContextPolicyReport},
			{name: "mode enforce", mode: "enforce", alias: "", tenants: "", tenant: "team-a", want: store.ContextPolicyEnforce},
			{name: "alias alone", mode: "", alias: "enforce", tenants: "", tenant: "team-a", want: store.ContextPolicyEnforce},
			{name: "mode legacy over an enforcing alias", mode: "legacy", alias: "enforce", tenants: "", tenant: "team-a", want: store.ContextPolicyLegacy},
			{name: "enforce capped for a listed tenant", mode: "enforce", alias: "", tenants: "team-a", tenant: "team-a", want: store.ContextPolicyLegacy},
			{name: "enforce stands for an unlisted tenant", mode: "enforce", alias: "", tenants: "team-a", tenant: "team-b", want: store.ContextPolicyEnforce},
			{name: "a declared policy beats the resolved default", mode: "enforce", alias: "", tenants: "", tenant: "team-a", declared: store.ContextPolicyReport, want: store.ContextPolicyReport},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(reliability.EnvMode, tc.mode)
				t.Setenv(reliability.EnvContextPolicyAlias, tc.alias)
				t.Setenv(reliability.EnvLegacyTenants, tc.tenants)
				p, st, baseCtx := sourceTestPublisher(t)
				ctx := store.WithIdentity(baseCtx, tc.tenant, "u1")
				var wire *queue.RunMessage
				p.publishRun = func(_ context.Context, msg *queue.RunMessage) error { wire = msg; return nil }
				spec := runview.LaunchSpec{FilePath: "wf.bot", Source: main}
				if tc.declared != "" {
					spec.ExecutionContext = &store.ExecutionContext{Policy: tc.declared}
				}
				if _, err := p.SubmitLaunch(ctx, "run-"+tc.name, spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "h"}); err != nil {
					t.Fatalf("SubmitLaunch: %v", err)
				}
				row, err := st.LoadRun(ctx, "run-"+tc.name)
				if err != nil {
					t.Fatalf("LoadRun: %v", err)
				}
				if row.ExecutionContext == nil || row.ExecutionContext.Policy != tc.want {
					t.Fatalf("row policy = %v, want %q", row.ExecutionContext, tc.want)
				}
				if wire == nil || wire.ExecutionContext == nil || wire.ExecutionContext.Policy != tc.want {
					t.Fatalf("wire policy = %+v, want %q", wire.ExecutionContext, tc.want)
				}
			})
		}
	})
}

// TestAdmissionJudgesThePolicyThePublisherStamped runs the two poles of the
// stamped policy through the REAL runner-side admission, on a foreign store
// (the execution_context_admission_test.go harness): under enforce a
// required-store mismatch is refused with context_mismatch; the same launch
// with the variables unset is allowed with legacy_context_unvalidated and
// counted in the A15 census — the launches enforce would refuse.
func TestAdmissionJudgesThePolicyThePublisherStamped(t *testing.T) {
	const main = "workflow main:\n  entry: done\n"
	parsed := parsertest.Parse("main.bot", main)
	compiled := ir.Compile(parsed.File)
	if compiled.HasErrors() {
		t.Fatal(compiled.Diagnostics)
	}
	for _, tc := range []struct {
		name               string
		mode               string
		wantDecision       string
		wantCode           string
		wantRunErr         bool
		wantWouldDenyCount int
	}{
		{name: "enforce refuses the mismatch", mode: reliability.EnvMode + "=enforce", wantDecision: "denied", wantCode: "context_mismatch", wantRunErr: true},
		{name: "unset allows it as legacy and censuses it", mode: "", wantDecision: "allowed", wantCode: "legacy_context_unvalidated", wantWouldDenyCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := strings.TrimPrefix(tc.mode, reliability.EnvMode+"=")
			t.Setenv(reliability.EnvMode, mode)
			t.Setenv(reliability.EnvContextPolicyAlias, "")
			t.Setenv(reliability.EnvLegacyTenants, "")
			p, a, ctx := sourceTestPublisher(t)
			actualA, err := store.ResolveStoreContextRef(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			declared := actualA
			declared.Required = true
			var wire *queue.RunMessage
			p.publishRun = func(_ context.Context, msg *queue.RunMessage) error { wire = msg; return nil }
			// No policy declared on the spec — the publisher's resolved default
			// is the only answer — but the required-store declaration stays, so
			// the foreign store the runner lands on is a VIOLATION, not the
			// waived optional hatch.
			if _, err := p.SubmitLaunch(ctx, "publisher-admission", runview.LaunchSpec{
				Source:           main,
				FilePath:         "main.bot",
				ExecutionContext: &store.ExecutionContext{RunStore: declared},
			}, compiled.Workflow, &runview.CompiledSource{Hash: "h"}); err != nil {
				t.Fatal(err)
			}
			row, err := a.LoadRun(ctx, "publisher-admission")
			if err != nil {
				t.Fatal(err)
			}
			if row.ExecutionContext == nil || row.ExecutionContext.Policy == "" {
				t.Fatalf("row carries no policy: %+v", row.ExecutionContext)
			}
			if wire == nil || wire.ExecutionContext == nil {
				t.Fatal("wire carries no context")
			}
			// The foreign store the run actually lands on: a different
			// identity, so the required declaration mismatches.
			b, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.CreateRun(ctx, row.ID, row.WorkflowName, nil); err != nil {
				t.Fatal(err)
			}
			runJSON, err := os.ReadFile(filepath.Join(a.Root(), "runs", row.ID, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.Root(), "runs", row.ID, "run.json"), runJSON, 0600); err != nil {
				t.Fatal(err)
			}
			actualB, err := store.ResolveStoreContextRef(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			if actualA.ID == actualB.ID {
				t.Fatal("fixture A equals B")
			}
			eng := runtime.New(compiled.Workflow, b, nil, runtime.WithExecutionContext(wire.ExecutionContext), runtime.WithWorkflowHash("h"), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"))
			runErr := eng.Run(ctx, row.ID, nil)
			after, err := b.LoadRun(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Admission == nil {
				t.Fatal("admission absent")
			}
			t.Logf("mode=%q runError=%v admission=%+v", mode, runErr, after.Admission)
			if (runErr != nil) != tc.wantRunErr {
				t.Fatalf("run error = %v, want error: %v", runErr, tc.wantRunErr)
			}
			if after.Admission.Decision != tc.wantDecision || after.Admission.Code != tc.wantCode {
				t.Fatalf("admission = %s/%s, want %s/%s", after.Admission.Decision, after.Admission.Code, tc.wantDecision, tc.wantCode)
			}
			if got := reliability.Summarize([]*store.Run{after}).AdmissionsWouldDeny; got != tc.wantWouldDenyCount {
				t.Fatalf("admissions_would_deny = %d, want %d (baseline = %+v)", got, tc.wantWouldDenyCount, reliability.Summarize([]*store.Run{after}))
			}
		})
	}
}
