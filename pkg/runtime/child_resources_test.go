package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func resourceBundle(t *testing.T, body string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow resources:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"skills/shared.md", "skills/directory/SKILL.md"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return &bundle.Bundle{Dir: dir, SourcePath: dir, SkillsDir: filepath.Join(dir, "skills")}
}

func resourceWorkflow(child string) *ir.Workflow {
	wf := &ir.Workflow{Name: "resource-scope", Worktree: "none", Entry: "before", Nodes: map[string]ir.Node{
		"before": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "before"}},
		"after":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "after"}},
		"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}, Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{}}
	if child != "" {
		wf.Nodes["child"] = &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "child"}, Source: child}
		wf.Edges = []*ir.Edge{{From: "before", To: "child"}, {From: "child", To: "after"}, {From: "after", To: "done"}}
	} else {
		wf.Edges = []*ir.Edge{{From: "before", To: "after"}, {From: "after", To: "done"}}
	}
	return wf
}

func assertResourceBody(t *testing.T, work, body string) {
	t.Helper()
	for _, rel := range []string{"shared.md", "shared/SKILL.md", "directory/SKILL.md"} {
		raw, err := os.ReadFile(filepath.Join(work, ".claude", "skills", rel))
		if err != nil || string(raw) != body {
			t.Errorf("%s=%q,%v, want %s", rel, raw, err, body)
		}
	}
}

func TestChildResourcesRestoreAcrossNestedRuns(t *testing.T) {
	work := t.TempDir()
	st := tmpStore(t)
	bundles := map[string]*bundle.Bundle{"parent": resourceBundle(t, "parent"), "child": resourceBundle(t, "child"), "grandchild": resourceBundle(t, "grandchild")}
	var launch func(context.Context, string, string) error
	launch = func(ctx context.Context, name, parent string) error {
		next := map[string]string{"parent": "child", "child": "grandchild"}[name]
		ex := newStubExecutor()
		for _, node := range []string{"before", "after"} {
			ex.on(node, func(map[string]any) (map[string]any, error) {
				assertResourceBody(t, work, name)
				return map[string]any{}, nil
			})
		}
		eng := New(resourceWorkflow(next), st, ex, WithWorkDir(work), WithBundle(bundles[name]), WithParentRunID(parent), WithSandboxOverride("none"), WithSubbotRunner(func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
			return map[string]any{}, launch(ctx, req.Source, req.ParentRunID)
		}))
		return eng.Run(ctx, name, nil)
	}
	if err := launch(context.Background(), "parent", ""); err != nil {
		t.Fatal(err)
	}
	assertResourceBody(t, work, "parent")
}

func TestChildResourcesRestoreOnFailureAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "cancel"}[cancelled], func(t *testing.T) {
			work := t.TempDir()
			st := tmpStore(t)
			parent := resourceBundle(t, "parent")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ex := newStubExecutor()
			ex.on("before", func(map[string]any) (map[string]any, error) {
				assertResourceBody(t, work, "child")
				if cancelled {
					cancel()
				}
				return nil, errors.New("child failed")
			})
			root := New(resourceWorkflow(""), st, newStubExecutor(), WithWorkDir(work), WithBundle(parent), WithSandboxOverride("none"))
			if err := root.Run(context.Background(), "parent", nil); err != nil {
				t.Fatal(err)
			}
			// Like a later standalone resume: no parent context is supplied. Flat
			// mirrors still have durable markers identifying their earlier owner.
			child := New(resourceWorkflow(""), st, ex, WithWorkDir(work), WithBundle(resourceBundle(t, "child")), WithParentRunID("parent"), WithSandboxOverride("none"))
			err := child.Run(ctx, "child", nil)
			if err == nil {
				t.Fatal("expected failure")
			}
			assertResourceBody(t, work, "parent")
		})
	}
}

func TestChildResourcesSerializeSharedWorkspace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		work := t.TempDir()
		st := tmpStore(t)
		parent := New(resourceWorkflow(""), st, newStubExecutor(), WithWorkDir(work), WithBundle(resourceBundle(t, "parent")), WithSandboxOverride("none"))
		if err := parent.Run(context.Background(), "parent", nil); err != nil {
			t.Fatal(err)
		}
		firstEntered := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondEntered := make(chan struct{})
		launch := func(name string, entered chan struct{}, wait <-chan struct{}) <-chan error {
			done := make(chan error, 1)
			b := resourceBundle(t, name)
			go func() {
				ex := newStubExecutor()
				ex.on("before", func(map[string]any) (map[string]any, error) {
					close(entered)
					if wait != nil {
						<-wait
					}
					return map[string]any{}, nil
				})
				eng := New(resourceWorkflow(""), st, ex, WithWorkDir(work), WithBundle(b), WithParentRunID("parent"), WithSandboxOverride("none"))
				done <- eng.Run(context.Background(), name, nil)
			}()
			return done
		}
		first := launch("first", firstEntered, releaseFirst)
		<-firstEntered
		second := launch("second", secondEntered, nil)
		synctest.Wait()
		select {
		case <-secondEntered:
			t.Fatal("second child entered while first owned parent resources")
		default:
		}
		close(releaseFirst)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		assertResourceBody(t, work, "parent")
	})
}

// The grandchild is resumed by a new engine/context while its child parent is
// still parked awaiting the human answer. It must not wait on the root lease.
func TestChildResourcesExternalResumeInsideLiveChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		work, st := t.TempDir(), tmpStore(t)
		grandBundle := resourceBundle(t, "grandchild")
		grandWF := resourceWorkflow("")
		grandWF.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
		grandWF.Edges = []*ir.Edge{{From: "before", To: "gate"}, {From: "gate", To: "after"}, {From: "after", To: "done"}}
		grand := func() *Engine {
			ex := newStubExecutor()
			for _, node := range []string{"before", "after"} {
				ex.on(node, func(map[string]any) (map[string]any, error) {
					assertResourceBody(t, work, "grandchild")
					return map[string]any{}, nil
				})
			}
			return New(grandWF, st, ex, WithWorkDir(work), WithBundle(grandBundle), WithParentRunID("child"), WithSandboxOverride("none"))
		}
		paused, answered := make(chan struct{}), make(chan struct{})
		var launch func(context.Context, string, string) error
		launch = func(ctx context.Context, name, parent string) error {
			ex := newStubExecutor()
			for _, node := range []string{"before", "after"} {
				ex.on(node, func(map[string]any) (map[string]any, error) {
					assertResourceBody(t, work, name)
					return map[string]any{}, nil
				})
			}
			next := map[string]string{"parent": "child", "child": "grandchild"}[name]
			eng := New(resourceWorkflow(next), st, ex, WithWorkDir(work), WithBundle(resourceBundle(t, name)), WithParentRunID(parent), WithSandboxOverride("none"), WithSubbotRunner(func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
				if req.Source != "grandchild" {
					return map[string]any{}, launch(ctx, req.Source, req.ParentRunID)
				}
				if err := grand().Run(ctx, "grandchild", nil); !errors.Is(err, ErrRunPaused) {
					return nil, errors.New("grandchild did not pause")
				}
				assertResourceBody(t, work, "child")
				close(paused)
				select {
				case <-answered:
					return map[string]any{}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}))
			return eng.Run(ctx, name, nil)
		}
		done := make(chan error, 1)
		go func() { done <- launch(ctx, "parent", "") }()
		select {
		case <-paused:
		case err := <-done:
			t.Fatalf("parent stopped before pause: %v", err)
		}
		err := grand().Resume(ctx, "grandchild", map[string]any{"approved": true})
		close(answered)
		if err != nil {
			t.Errorf("standalone grandchild resume: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		assertResourceBody(t, work, "parent")
	})
}

func TestChildResourcesDisjointWorkspacesRemainConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		entered, release := make(chan struct{}, 2), make(chan struct{})
		done := make(chan error, 2)
		for _, name := range []string{"one", "two"} {
			work, b, st := t.TempDir(), resourceBundle(t, name), tmpStore(t)
			go func() {
				ex := newStubExecutor()
				ex.on("before", func(map[string]any) (map[string]any, error) {
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					assertResourceBody(t, work, name)
					return map[string]any{}, nil
				})
				eng := New(resourceWorkflow(""), st, ex, WithWorkDir(work), WithBundle(b), WithParentRunID("parent"), WithSandboxOverride("none"))
				done <- eng.Run(ctx, name, nil)
			}()
		}
		for range 2 {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("disjoint workspaces serialized")
			}
		}
		close(release)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestChildResourcesHostDevboxIsPerRunAndBareFileHasNoBundle(t *testing.T) {
	rec := stubHostDevbox(t, nil)
	work, st := t.TempDir(), tmpStore(t)
	parentBundle, childBundle := resourceBundle(t, "parent"), resourceBundle(t, "child")
	for _, b := range []*bundle.Bundle{parentBundle, childBundle} {
		if err := os.WriteFile(filepath.Join(b.Dir, "devbox.json"), []byte(`{"packages":["test"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parentEx := &envRecordingExecutor{stubExecutor: newStubExecutor()}
	parentEx.on("before", func(map[string]any) (map[string]any, error) {
		// The staged bot project is a private per-run dir named after
		// the run (`iterion-devbox-<runID>-<random>`, directly under the
		// temp dir): the run id in the name is what tells the two apart.
		if len(parentEx.engineEnv) != 1 || !strings.Contains(parentEx.engineEnv[0], "/iterion-devbox-parent-") {
			return nil, errors.New("parent devbox missing")
		}
		return map[string]any{}, nil
	})
	parent := New(resourceWorkflow("child"), st, parentEx, WithWorkDir(work), WithBundle(parentBundle), WithSandboxOverride("none"), WithSubbotRunner(func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		parentEnv := append([]string(nil), parentEx.engineEnv...)
		for _, bare := range []bool{false, true} {
			id, b := "child", childBundle
			if bare {
				id, b = "bare", nil
			}
			ex := &envRecordingExecutor{stubExecutor: newStubExecutor()}
			ex.on("before", func(map[string]any) (map[string]any, error) {
				if bare {
					if len(ex.engineEnv) != 0 {
						return nil, errors.New("bare step inherited sibling bundle devbox")
					}
				} else if len(ex.engineEnv) != 1 || !strings.Contains(ex.engineEnv[0], "/iterion-devbox-child-") || strings.Contains(ex.engineEnv[0], "/iterion-devbox-parent-") {
					return nil, errors.New("child devbox not isolated")
				}
				return map[string]any{}, nil
			})
			child := New(resourceWorkflow(""), st, ex, WithWorkDir(work), WithBundle(b), WithFilePath(filepath.Join(childBundle.Dir, "step.bot")), WithParentRunID(req.ParentRunID), WithSandboxOverride("none"))
			if err := child.Run(ctx, id, nil); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(parentEnv, parentEx.engineEnv) {
				return nil, errors.New("child changed parent executor PATH")
			}
			assertResourceBody(t, work, "parent")
		}
		return map[string]any{}, nil
	}))
	if err := parent.Run(context.Background(), "parent", nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.installs) != 2 {
		t.Fatalf("installs = %v, want parent + child only", rec.installs)
	}
}
