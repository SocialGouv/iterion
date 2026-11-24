package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRunnerComparesTheEffectiveStoreAndBindsWireContext(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	for _, probe := range []string{"required", "optional", "wire binding"} {
		t.Run(probe, func(t *testing.T) {
			a, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(t.Context(), "team-a", "u1")
			ref, err := store.ResolveStoreContextRef(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			ref.Required = probe != "optional"
			row, err := a.CreateRun(ctx, "runner-engine", "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			policy := store.ContextPolicyEnforce
			if probe == "wire binding" {
				policy = store.ContextPolicyReport
			}
			row.Status = store.RunStatusQueued
			row.WorkflowHash = "h"
			row.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: policy, RunStore: ref, Workflow: store.WorkflowContext{WorkflowRevision: "h"}}
			if err := a.SaveRun(ctx, row); err != nil {
				t.Fatal(err)
			}
			parsed := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
			body, err := ast.MarshalFile(parsed.File)
			if err != nil {
				t.Fatal(err)
			}
			msg := &queue.RunMessage{RunID: row.ID, TenantID: "team-a", OwnerID: "u1", WorkflowName: "main", WorkflowHash: "h", IRCompiled: body, ExecutionContext: row.ExecutionContext.Clone()}
			if probe == "wire binding" {
				msg.ExecutionContext.Workflow.WorkflowRevision = "wire-only-revision"
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
			before, err := b.LoadRun(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.ExecutionContext, row.ExecutionContext) {
				t.Fatal("migration changed context")
			}
			actual, err := store.ResolveStoreContextRef(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			if actual.ID == ref.ID {
				t.Fatal("A equals B")
			}
			r := &Runner{cfg: Config{Store: b, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
			runErr := r.executeRun(ctx, msg, nil, nil)
			after, err := b.LoadRun(ctx, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("case=%s A=%s B=%s runError=%v admission=%+v", probe, ref.ID, actual.ID, runErr, after.Admission)
			if after.Admission == nil {
				t.Fatal("admission absent")
			}
			switch probe {
			case "required":
				if runErr == nil || after.Admission.Decision != "denied" || after.Admission.Code != "context_mismatch" {
					t.Fatal("A→B required admitted")
				}
			case "optional":
				if runErr != nil || after.Admission.Code != "context_store_optional" {
					t.Fatal("optional hatch inert or unobserved")
				}
			case "wire binding":
				if runErr != nil || after.Admission.Code != "context_report_only" || !strings.Contains(after.Admission.Reason, "runner context workflow revision differs from persisted context") {
					t.Fatal("wire context did not reach actual engine")
				}
			}
		})
	}
}
