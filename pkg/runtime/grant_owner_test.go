package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type privateGrantStore struct {
	store.RunStore
	grant store.RunFilesGrant
	opens int
}

func (s *privateGrantStore) OpenRunFilesGrant(context.Context, string) (store.RunFilesGrant, error) {
	s.opens++
	return s.grant, nil
}

type privateGrant struct {
	path   string
	closed int
	err    error
}

func (g *privateGrant) HostPath() string {
	if g.path != "" {
		return g.path
	}
	return "/private"
}
func (g *privateGrant) Bind(c context.Context, id string) (context.Context, error) { return c, g.err }
func (g *privateGrant) ValidateHostPath(context.Context) error                     { return g.err }
func (g *privateGrant) OpenRoot(context.Context) (*os.Root, error)                 { return nil, g.err }
func (g *privateGrant) Close() error                                               { g.closed++; return nil }

type privateProvider struct {
	NodeExecutor
	g store.RunFilesGrant
}

func (p privateProvider) RunFilesGrant() store.RunFilesGrant { return p.g }
func privateBinding(t *testing.T) context.Context {
	t.Helper()
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: 1, Protocol: 1, BirthID: "birth"})
	if e != nil {
		t.Fatal(e)
	}
	c, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestRunFilesGrantNilCapability(t *testing.T) {
	e := &Engine{store: &privateGrantStore{}}
	_, _, close, err := e.runFilesForSandbox(privateBinding(t), "r")
	close()
	if !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatal(err)
	}
}
func TestRunFilesGrantOwnership(t *testing.T) {
	for _, borrow := range []bool{false, true} {
		g := &privateGrant{}
		s := &privateGrantStore{grant: g}
		e := &Engine{store: s}
		if borrow {
			e.executor = privateProvider{g: g}
		}
		_, got, close, err := e.runFilesForSandbox(privateBinding(t), "r")
		if err != nil || got != g {
			t.Fatal(got, err)
		}
		close()
		want := 1
		if borrow {
			want = 0
		}
		if g.closed != want {
			t.Fatalf("borrow %v close %d", borrow, g.closed)
		}
		if borrow && s.opens != 0 {
			t.Fatal("borrow reacquired")
		}
	}
	g := &privateGrant{err: store.ErrRunDataIdentityChanged}
	s := &privateGrantStore{}
	e := &Engine{store: s, executor: privateProvider{g: g}}
	_, _, close, err := e.runFilesForSandbox(privateBinding(t), "r")
	close()
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || s.opens != 0 || g.closed != 0 {
		t.Fatal(err, s.opens, g.closed)
	}
}

type privateInheritedRun struct {
	sharedFakeRun
	artifactDir string
}

func (r *privateInheritedRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := r.sharedFakeRun.Command(ctx, cmd, opts)
	c.Env = append(c.Env, "ITERION_ARTIFACT_FILES_DIR="+r.artifactDir)
	return c
}
func TestRunFilesGrantSharedChildWritesParent(t *testing.T)  { privateSharedChild(t, false) }
func TestRunFilesGrantSharedChildStrictRefusal(t *testing.T) { privateSharedChild(t, true) }
func privateSharedChild(t *testing.T, strict bool) {
	if strict {
		if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
			t.Skip(err)
		}
	}
	st := tmpStore(t)
	ctx := context.Background()
	protocol := store.HistoricalRunData
	if strict {
		protocol = store.IncarnationRunData
	}
	created, err := store.CreateRunWithValueFormat(ctx, st, store.RunCreateSpec{ID: "child", WorkflowName: "child", Format: 1, DataProtocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	parentDir := t.TempDir()
	childDir := t.TempDir()
	fake := &privateInheritedRun{artifactDir: parentDir}
	work := t.TempDir()
	g := &privateGrant{path: childDir}
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}
	realExecutor := model.NewClawExecutor(nil, wf, model.WithWorkDir(work), model.WithArtifactFilesDir(childDir))
	defer realExecutor.Close()
	executor := privateModelProvider{ClawExecutor: realExecutor, g: g}
	e := New(&ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}}, st, executor, WithWorkDir(work), WithParentRunID("parent"), WithSharedSandbox(&SharedSandbox{Run: fake, WorkspaceFolder: work}))
	e.resourceScope = &runResourceScope{path: work, borrowed: true, owned: map[string]string{}}
	if strict {
		b, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "child", Format: 1, Protocol: 1, BirthID: created.BirthID})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err = store.WithRunDataBinding(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
	}
	cleanup, err := e.startSandbox(ctx, "child", work, "", nil)
	if strict && errors.Is(err, store.ErrRunDataCapability) {
		if e.activeShare != nil || fake.commands != 0 || g.closed != 0 {
			t.Fatal("refusal happened after adoption effects")
		}
		return
	}

	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(ctx, &ir.ToolNode{BaseNode: ir.BaseNode{ID: "write-child"}, Command: `printf child > "$ITERION_ARTIFACT_FILES_DIR/report"`}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if b, err := os.ReadFile(filepath.Join(parentDir, "report")); err != nil || string(b) != "child" {
		t.Fatal(string(b), err)
	}
	if _, err := os.Stat(filepath.Join(childDir, "report")); !os.IsNotExist(err) {
		t.Fatal("child namespace unexpectedly populated")
	}
	if strict {
		t.Fatal("strict child wrote parent artifact-files through real executor")
	}
	if g.closed != 0 || fake.cleanups != 0 {
		t.Fatal("child closed borrowed owner", g.closed, fake.cleanups)
	}
}

type privateModelProvider struct {
	*model.ClawExecutor
	g store.RunFilesGrant
}

func (p privateModelProvider) RunFilesGrant() store.RunFilesGrant { return p.g }

func TestRunFilesGrantSharedNonAdoptStrict(t *testing.T) {
	st := tmpStore(t)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "r", "child", nil); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Name: "child", Nodes: map[string]ir.Node{}, Sandbox: &ir.SandboxSpec{Mode: "none"}}
	g := &privateGrant{path: t.TempDir()}
	work := t.TempDir()
	e := New(wf, st, privateProvider{NodeExecutor: newStubExecutor(), g: g}, WithWorkDir(work), WithSharedSandbox(&SharedSandbox{Run: bindOnly{&sharedFakeRun{}}, WorkspaceFolder: work}))
	e.resourceScope = &runResourceScope{path: work, borrowed: true, owned: map[string]string{}}
	close, err := e.startSandbox(privateBinding(t), "r", work, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	close()
	if e.activeShare != nil || g.closed != 0 {
		t.Fatal("non-adopt path adopted or closed borrowed grant")
	}
}

func TestRunFilesGrantFallbackFailureClosesOwned(t *testing.T) {
	g := &privateGrant{err: store.ErrRunDataIdentityChanged}
	e := &Engine{store: &privateGrantStore{grant: g}}
	_, _, close, err := e.runFilesForSandbox(privateBinding(t), "r")
	close()
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || g.closed != 1 {
		t.Fatal(err, g.closed)
	}
}
