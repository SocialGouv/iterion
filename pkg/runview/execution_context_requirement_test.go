package runview

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type requirementLegacyStore struct{ store.RunStore }

func TestResolveExecutionContextPreservesAnExactExplicitRequirement(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, tc := range []string{"optional", "required", "nil", "draft", "id", "kind", "namespace", "revision"} {
			name := "capability/" + tc
			if legacy {
				name = "legacy/" + tc
			}
			t.Run(name, func(t *testing.T) {
				fs, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				var s store.RunStore = fs
				if legacy {
					s = requirementLegacyStore{fs}
				}
				resolved, err := store.ResolveStoreContextRef(t.Context(), s)
				if err != nil {
					t.Fatal(err)
				}
				declared := resolved
				declared.Required = false
				switch tc {
				case "required":
					declared.Required = true
				case "draft":
					declared = store.ContextRef{}
				case "id":
					declared.ID = "store-v1:forged"
				case "kind":
					declared.Kind = "other"
				case "namespace":
					declared.Namespace += "/other"
				case "revision":
					declared.Revision = "other"
				}
				input := &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: declared}
				if tc == "nil" {
					input = nil
				}
				before := input.Clone()
				got, err := ResolveExecutionContext(t.Context(), s, "requirement", LaunchSpec{ExecutionContext: input}, nil, "", store.ContextPolicyEnforce, "")
				want := resolved
				want.Required = tc != "optional"
				if err != nil || got.RunStore != want {
					t.Fatalf("resolved=%+v err=%v want %+v", got, err, want)
				}
				if !reflect.DeepEqual(input, before) {
					t.Fatal("resolution changed the caller declaration")
				}
			})
		}
	}
}

func TestResolvedLegacyStoreOptOutRemainsUnvalidatedAtAdmission(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := requirementLegacyStore{fs}
	ref, err := store.ResolveStoreContextRef(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	ref.Required = false
	wf := &ir.Workflow{Name: "legacy", Entry: "done", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
	c, err := ResolveExecutionContext(t.Context(), s, "legacy-optional", LaunchSpec{ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: ref}}, wf, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.New(wf, s, nil, runtime.WithExecutionContext(c), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none")).Run(t.Context(), "legacy-optional", nil); err != nil {
		t.Fatal(err)
	}
	run, err := s.LoadRun(t.Context(), "legacy-optional")
	if err != nil {
		t.Fatal(err)
	}
	if run.Admission == nil || run.Admission.Code != "context_store_optional" || run.Admission.Decision != "allowed" || run.Admission.Policy != store.ContextPolicyEnforce || run.ExecutionContext.RunStore.Required {
		t.Fatalf("legacy opt-out is inert or falsely certified: %+v", run)
	}
}
