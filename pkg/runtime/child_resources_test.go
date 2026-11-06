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
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
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
			storetest.SeedParentRun(t, st, "parent")
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

// A child in place borrows its parent's workspace BEFORE any mirror refusal
// has run: beginRunResources' snapshot restore (RemoveAll + copy back) and
// hideParentSkillCollisions' removals all descend into `.claude/`. Through a
// symlink they would delete and rewrite in the link's TARGET. The refusal
// fires at the head of the block, typed like the mirror's (#1569).
func TestBeginRunResourcesRefusesAClaudeSymlink(t *testing.T) {
	ws, target := claudeSymlinkWorkspace(t)
	eng := &Engine{workDir: ws}

	_, _, err := eng.beginRunResources(context.Background(), "run_child", true)
	var linkErr *claudeSymlinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("beginRunResources with a .claude symlink = %v, want the typed *claudeSymlinkError refusal", err)
	}
	assertLinkTargetUntouched(t, target)
}

// The snapshot's own read side: a `.claude/skills` entry that resolves OUT of
// the workspace would copy the operator's files into the backup — kept on
// disk, its path in the run record, on a failed restore. On a checkout the
// operator did not write the entry is refused typed BEFORE any byte moves;
// the trusted lane keeps the operator's own arrangement (the link is copied
// as a link, nothing followed).
func TestSnapshotChildResources_RefusesAnEscapingEntry(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "operator-file.md"), "operator content not for the backup\n")

	build := func(t *testing.T) string {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws, ".claude", "skills")); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	ws := build(t)
	_, backup, err := snapshotChildResources(ws, store.RunTrustFork)
	if !errors.Is(err, ErrSkillsDestEscapesWorkspace) {
		t.Fatalf("snapshot with an escaping entry = %v, want the typed %v refusal", err, ErrSkillsDestEscapesWorkspace)
	}
	if backup != "" {
		if raw, rerr := os.ReadFile(filepath.Join(backup, "skills", "operator-file.md")); rerr == nil {
			t.Fatalf("the operator's file landed in the backup %s: %q", backup, raw)
		}
	}

	// Control arm: the SAME fixture on the trusted lane snapshots — the
	// refusal above is the trust floor, not a broken arrangement.
	_, trustedBackup, err := snapshotChildResources(build(t), store.RunTrustDefault)
	if err != nil {
		t.Fatalf("control: the trusted snapshot failed: %v", err)
	}
	if raw, rerr := os.ReadFile(trustedBackup + string(filepath.Separator) + "skills"); rerr != nil {
		if _, lerr := os.Lstat(filepath.Join(trustedBackup, "skills")); lerr != nil {
			t.Fatalf("control: the backup holds no skills entry: %v", lerr)
		}
	} else if string(raw) != "operator content not for the backup\n" {
		_ = raw // a real directory copied through — still the operator's own bytes
	}
}

// The prune recurses and removes bottom-up: descending into a symlinked
// DIRECTORY removes the link target's empty subdirectories — the operator's
// own tree, under a skill name the public bundle catalog hands the attacker.
// The guard is Lstat-based (a real directory, never a link) on BOTH lanes:
// the rooted handle would refuse the escape at Remove, but the trusted
// passthrough follows it — the guard is its half of the pair.
func TestRemoveEmptyResourceDirs_NeverDescendsALink(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "witness.md"), "not the run's to touch\n")

	ws := t.TempDir()
	link := filepath.Join(ws, ".claude", "skills", "stem")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// The trusted handle is the lane where nothing but this guard stands
	// between the recursion and the operator's tree.
	wr, err := openWorkspaceRoot(ws, store.RunTrustDefault)
	if err != nil {
		t.Fatal(err)
	}
	defer wr.Close()
	removeEmptyResourceDirs(wr, link)

	if _, err := os.Stat(filepath.Join(outside, "inner")); err != nil {
		t.Errorf("the recursion descended the link and removed the operator's empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "witness.md")); err != nil {
		t.Errorf("the operator's file is gone: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link itself should have been left alone: %v", err)
	}

	// Control arm: a REAL empty directory is pruned — without it the intact
	// tree above could mean the prune never ran.
	real := filepath.Join(ws, ".claude", "skills", "realstem", "inner")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	removeEmptyResourceDirs(wr, filepath.Join(ws, ".claude", "skills", "realstem"))
	if _, err := os.Stat(filepath.Join(ws, ".claude", "skills", "realstem")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control: the real empty directory was not pruned (err=%v) — the prune proves nothing", err)
	}
}

// Defense in depth behind that refusal: the link swapped in mid-run, after
// the snapshot was taken. The restore must fail loudly — never RemoveAll
// through the link — and leave the backup for recovery.
func TestSnapshotChildResourcesRestoreRefusesAMidRunSymlinkSwap(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws, ".claude", "skills", "parent.md"), parentClaudeBody)
	restore, backup, err := snapshotChildResources(ws, store.RunTrustDefault)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The mid-run swap: `.claude` becomes a link to a sibling tree holding
	// a witness the restore would otherwise delete.
	target := filepath.Join(ws, "target-dir")
	if err := os.MkdirAll(filepath.Join(target, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(target, "skills", "witness.md")
	writeFile(t, witness, "not the run's to touch\n")
	if err := os.RemoveAll(filepath.Join(ws, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-dir", filepath.Join(ws, ".claude")); err != nil {
		t.Fatal(err)
	}

	restoreErr := restore()
	var linkErr *claudeSymlinkError
	if !errors.As(restoreErr, &linkErr) {
		t.Fatalf("restore through a mid-run symlink swap = %v, want the typed *claudeSymlinkError", restoreErr)
	}
	if !strings.Contains(restoreErr.Error(), backup) {
		t.Fatalf("the restore error must name the backup for recovery: %v", restoreErr)
	}
	assertBackupKeptTheParentsEntry(t, filepath.Join(backup, "skills", "parent.md"))
	raw, readErr := os.ReadFile(witness)
	if readErr != nil || string(raw) != "not the run's to touch\n" {
		t.Fatalf("the restore reached through the link: witness=%q, %v", raw, readErr)
	}
}
