package cloudpublisher

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPublishedStoreRequirementReachesEffectiveStoreAdmission(t *testing.T) {
	for _, required := range []bool{true, false} {
		t.Run(map[bool]string{true: "required", false: "optional"}[required], func(t *testing.T) {
			p, a, ctx := sourceTestPublisher(t)
			source := "workflow main:\n  entry: done\n"
			parsed := parsertest.Parse("main.bot", source)
			compiled := ir.Compile(parsed.File)
			if compiled.HasErrors() {
				t.Fatal(compiled.Diagnostics)
			}
			actualA, err := store.ResolveStoreContextRef(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			declared := actualA
			declared.Required = required
			var wire *queue.RunMessage
			p.publishRun = func(_ context.Context, msg *queue.RunMessage) error { wire = msg; return nil }
			_, err = p.SubmitLaunch(ctx, "publisher-engine", runview.LaunchSpec{Source: source, FilePath: "main.bot", ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: declared}}, compiled.Workflow, &runview.CompiledSource{Hash: "h"})
			if err != nil {
				t.Fatal(err)
			}
			row, err := a.LoadRun(ctx, "publisher-engine")
			if err != nil {
				t.Fatal(err)
			}
			if wire == nil || wire.ExecutionContext == nil || row.ExecutionContext == nil {
				t.Fatal("missing declaration")
			}
			if row.ExecutionContext.RunStore.ID != actualA.ID || row.ExecutionContext.RunStore.Required != required || !reflect.DeepEqual(wire.ExecutionContext, row.ExecutionContext) {
				t.Fatalf("publisher changed declaration: row=%+v wire=%+v", row.ExecutionContext, wire.ExecutionContext)
			}
			b, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.CreateRun(ctx, row.ID, row.WorkflowName, nil); err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(filepath.Join(a.Root(), "runs", row.ID, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.Root(), "runs", row.ID, "run.json"), bytes, 0600); err != nil {
				t.Fatal(err)
			}
			actualB, err := store.ResolveStoreContextRef(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			if actualA.ID == actualB.ID {
				t.Fatal("fixture A equals B")
			}
			before, err := b.LoadRun(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.ExecutionContext, row.ExecutionContext) {
				t.Fatal("migration changed context")
			}
			eng := runtime.New(compiled.Workflow, b, nil, runtime.WithExecutionContext(wire.ExecutionContext), runtime.WithWorkflowHash("h"), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"))
			runErr := eng.Run(ctx, row.ID, nil)
			after, err := b.LoadRun(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("required=%v A=%s B=%s runError=%v admission=%+v", required, actualA.ID, actualB.ID, runErr, after.Admission)
			if after.Admission == nil {
				t.Fatal("admission absent")
			}
			if required {
				if runErr == nil || after.Admission.Decision != "denied" || after.Admission.Code != "context_mismatch" {
					t.Fatal("A→B required admitted")
				}
			} else {
				if runErr != nil || after.Admission.Decision != "allowed" || after.Admission.Code != "context_store_optional" {
					t.Fatal("optional hatch inert or unobserved")
				}
			}
		})
	}
}
