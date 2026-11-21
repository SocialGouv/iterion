package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFilesystemStoreAdmissionSurvivesNativePathTransport(t *testing.T) {
	for _, required := range []bool{true, false} {
		for _, segment := range []string{"valid-é", "raw-\xff", "raw-\xfe", "raw-\ufffd", "raw-%FF", "raw-%25FF"} {
			t.Run(fmt.Sprintf("%q/required=%v", segment, required), func(t *testing.T) {
				fs, e := store.New(filepath.Join(t.TempDir(), segment))
				if e != nil {
					t.Fatal(e)
				}
				wf := &ir.Workflow{Name: "main", Entry: "done", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
				ref, e := store.ResolveStoreContextRef(t.Context(), fs)
				if e != nil {
					t.Fatal(e)
				}
				ref.Required = required
				c, e := ResolveExecutionContext(t.Context(), fs, "wire-identity", LaunchSpec{ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: ref}}, wf, "h", store.ContextPolicyEnforce, "")
				if e != nil {
					t.Fatal(e)
				}
				row, e := fs.CreateRun(t.Context(), "wire-identity", wf.Name, nil)
				if e != nil {
					t.Fatal(e)
				}
				row.Status = store.RunStatusQueued
				row.WorkflowHash = "h"
				row.ExecutionContext = c
				if e := fs.SaveRun(t.Context(), row); e != nil {
					t.Fatal(e)
				}
				loaded, e := fs.LoadRun(t.Context(), row.ID)
				if e != nil {
					t.Fatal(e)
				}
				t.Logf("before=%+v after=%+v sameIdentity=%v", c.RunStore, loaded.ExecutionContext.RunStore, store.SameContextRefIdentity(c.RunStore, loaded.ExecutionContext.RunStore))
				eng := runtime.New(wf, fs, nil, runtime.WithExecutionContext(c), runtime.WithWorkflowHash("h"), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none"))
				runErr := eng.Run(t.Context(), row.ID, nil)
				after, e := fs.LoadRun(t.Context(), row.ID)
				if e != nil {
					t.Fatal(e)
				}
				t.Logf("Run error=%v status=%s admission=%+v", runErr, after.Status, after.Admission)
				if !store.SameContextRefIdentity(c.RunStore, loaded.ExecutionContext.RunStore) || runErr != nil || after.Admission == nil || after.Admission.Code != "context_match" || after.ExecutionContext.RunStore.Required != required {
					t.Fatalf("same native store identity did not survive launch persistence: runErr=%v", runErr)
				}
			})
		}
	}
}

type transportLegacyStore struct{ store.RunStore }
type transportCapabilityStore struct {
	store.RunStore
	ref store.ContextRef
}

func (s transportCapabilityStore) ContextIdentity(context.Context) (store.ContextRef, error) {
	return s.ref, nil
}
func TestPersistedStoreOptOutRemainsExactThroughTransport(t *testing.T) {
	root := t.TempDir()
	for _, legacy := range []bool{false, true} {
		for _, name := range []string{"raw-\xff", "raw-\xfe", "raw-\ufffd", "raw-%FF", "raw-%FE", "raw-%25FF", "valid-é", "with space"} {
			t.Run(name+map[bool]string{false: "/capability", true: "/legacy"}[legacy], func(t *testing.T) {
				fs, e := store.New(filepath.Join(root, name))
				if e != nil {
					t.Fatal(e)
				}
				var st store.RunStore = fs
				if legacy {
					st = transportLegacyStore{fs}
				}
				ref, e := store.ResolveStoreContextRef(t.Context(), st)
				if e != nil {
					t.Fatal(e)
				}
				ref.Required = false
				seed := &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: ref}
				c, e := ResolveExecutionContext(t.Context(), st, "parent", LaunchSpec{ExecutionContext: seed}, nil, "", store.ContextPolicyEnforce, "")
				if e != nil {
					t.Fatal(e)
				}
				if legacy && ref.ID != store.StableContextID(fmt.Sprintf("%T", st), st.Root()) {
					t.Fatal("legacy ID changed")
				}
				id := fmt.Sprintf("parent-%v", legacy)
				row, e := st.CreateRun(t.Context(), id, "main", nil)
				if e != nil {
					t.Fatal(e)
				}
				row.ExecutionContext = c
				if e := st.SaveRun(t.Context(), row); e != nil {
					t.Fatal(e)
				}
				persisted, e := st.LoadRun(t.Context(), id)
				if e != nil {
					t.Fatal(e)
				}
				raw, e := json.Marshal(persisted.ExecutionContext)
				if e != nil {
					t.Fatal(e)
				}
				var loaded store.ExecutionContext
				if e := json.Unmarshal(raw, &loaded); e != nil {
					t.Fatal(e)
				}
				child, e := ResolveExecutionContext(t.Context(), st, "child", LaunchSpec{ExecutionContext: &loaded}, nil, "", store.ContextPolicyEnforce, "")
				if e != nil {
					t.Fatal(e)
				}
				t.Logf("legacy=%v namespaceBefore=%q namespaceAfter=%q inheritedRequired=%v", legacy, c.RunStore.Namespace, loaded.RunStore.Namespace, child.RunStore.Required)
				if child.RunStore.Required || !store.SameContextRefIdentity(ref, loaded.RunStore) {
					t.Errorf("legitimate persisted optional declaration becomes required after wire transport")
				}
				wf := &ir.Workflow{Name: "main", Entry: "done", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
				childID := fmt.Sprintf("child-%v", legacy)
				runErr := runtime.New(wf, st, nil, runtime.WithExecutionContext(child), runtime.WithWorkDir(t.TempDir()), runtime.WithSandboxOverride("none")).Run(t.Context(), childID, nil)
				result, e := st.LoadRun(t.Context(), childID)
				if e != nil {
					t.Fatal(e)
				}
				wantCode := "context_match"
				if legacy {
					wantCode = "context_store_optional"
				}
				if runErr != nil || result.Admission == nil || result.Admission.Code != wantCode || result.Admission.Decision != "allowed" || result.ExecutionContext.RunStore.Required {
					t.Errorf("inherited hatch inert after real persistence: err=%v context=%+v admission=%+v", runErr, result.ExecutionContext, result.Admission)
				}
			})
		}
	}
}
func TestFilesystemStoreNamespaceIsLossless(t *testing.T) {
	root := t.TempDir()
	seenID := map[string]string{}
	seenNS := map[string]string{}
	for _, name := range []string{"raw-\xff", "raw-\xfe", "raw-\ufffd", "raw-%FF", "raw-%FE", "raw-%25FF", "raw-%EF%BF%BD", "valid-é", "with space", "hash#question?"} {
		physical := filepath.Join(root, name)
		fs, e := store.New(physical)
		if e != nil {
			t.Fatal(e)
		}
		ref, e := store.ResolveStoreContextRef(t.Context(), fs)
		if e != nil {
			t.Fatal(e)
		}
		if !utf8.ValidString(ref.Namespace) {
			t.Errorf("namespace remains invalid UTF8 for %q", name)
		}
		decoded, e := url.PathUnescape(ref.Namespace)
		if e != nil || decoded != physical {
			t.Errorf("namespace not reversible for %q: namespace=%q decoded=%q err=%v", name, ref.Namespace, decoded, e)
		}
		if prev, ok := seenID[ref.ID]; ok {
			t.Errorf("ID collision %q / %q", name, prev)
		}
		if prev, ok := seenNS[ref.Namespace]; ok {
			t.Errorf("namespace collision %q / %q", name, prev)
		}
		seenID[ref.ID] = name
		seenNS[ref.Namespace] = name
	}
}
func TestStoreCapabilityRefusesLossyIdentityBeforeLaunch(t *testing.T) {
	fs, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"ID", "Kind", "Namespace", "Revision"} {
		t.Run(field, func(t *testing.T) {
			ref := store.NewStoreContextRef("fixture", "scope")
			switch field {
			case "ID":
				ref.ID += "\xff"
			case "Kind":
				ref.Kind += "\xff"
			case "Namespace":
				ref.Namespace += "\xff"
			case "Revision":
				ref.Revision = "secret://fixture\xff"
			}
			st := transportCapabilityStore{fs, ref}
			got, e := ResolveExecutionContext(t.Context(), st, "malformed", LaunchSpec{}, nil, "", store.ContextPolicyEnforce, "")
			if e == nil || got != nil {
				t.Fatalf("malformed capability field %s accepted before lossy serialization: result=%+v error=%v", field, got, e)
			}
			if strings.Contains(e.Error(), "secret://") || strings.Contains(e.Error(), "\xff") {
				t.Fatalf("refusal echoes unsafe capability value: %v", e)
			}
		})
	}
}

func TestStoreCapabilityPreservesSerializableIdentity(t *testing.T) {
	fs, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ref := store.ContextRef{ID: "store-v1:fixture-é\x00", Kind: "fixture-é\x00", Namespace: "scope/\x00/😀/%FF", Revision: "revision-\x00-é", Required: true}
	st := transportCapabilityStore{fs, ref}
	decl := ref
	decl.Required = false
	c, e := ResolveExecutionContext(t.Context(), st, "valid-capability", LaunchSpec{ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce, RunStore: decl}}, nil, "", store.ContextPolicyEnforce, "")
	if e != nil || c.RunStore != decl {
		t.Fatalf("serializable capability refused or changed: %+v %v", c, e)
	}
	raw, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var after store.ExecutionContext
	if e := json.Unmarshal(raw, &after); e != nil {
		t.Fatal(e)
	}
	if after.RunStore != decl {
		t.Fatalf("valid UTF8 contract changed during JSON transport: %+v", after.RunStore)
	}
}
