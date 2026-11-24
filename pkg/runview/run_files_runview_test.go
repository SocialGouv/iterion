package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"path/filepath"
	"testing"
)

type runFilesViewGrant struct {
	store.RunFilesGrant
	closed int
}

func (g *runFilesViewGrant) Close() error { g.closed++; return g.RunFilesGrant.Close() }

type runFilesViewStore struct {
	store.RunStore
	base  *store.FilesystemRunStore
	grant *runFilesViewGrant
	opens int
}

func (s *runFilesViewStore) OpenRunFilesGrant(ctx context.Context, id string) (store.RunFilesGrant, error) {
	g, e := s.base.OpenRunFilesGrant(ctx, id)
	if e != nil {
		return nil, e
	}
	s.opens++
	s.grant = &runFilesViewGrant{RunFilesGrant: g}
	return s.grant, nil
}

type runFilesEventOnly struct{ model.EventEmitter }
type runFilesViewNoCapability struct{ store.RunStore }

func runFilesViewSpec(t *testing.T, strict bool) (ExecutorSpec, *runFilesViewStore) {
	t.Helper()
	if strict {
		if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
			t.Skip(err)
		}
	}
	s, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	protocol := store.HistoricalRunData
	if strict {
		protocol = store.IncarnationRunData
	}
	r, e := s.CreateRunWithValueFormat(context.Background(), store.RunCreateSpec{ID: "r", WorkflowName: "w", Format: 2, DataProtocol: protocol})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: 2, Protocol: r.DataProtocol, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: r.ID, Format: 2})
	if e != nil {
		t.Fatal(e)
	}
	wrapped := &runFilesViewStore{RunStore: s, base: s}
	wf := &ir.Workflow{Name: "w", Entry: "done", Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}}
	return ExecutorSpec{Ctx: ctx, Workflow: wf, Store: runFilesEventOnly{s}, Runs: wrapped, RunID: "r", WorkDir: t.TempDir(), Logger: iterlog.Nop()}, wrapped
}
func TestRunFilesBuildGrantTransfersThroughRuns(t *testing.T) {
	spec, s := runFilesViewSpec(t, true)
	e, err := BuildExecutor(spec)
	if err != nil {
		t.Fatal(err)
	}
	if s.opens != 1 || e.RunFilesGrant() != s.grant || s.grant.closed != 0 {
		t.Fatal("grant not lent intact")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if s.grant.closed != 1 {
		t.Fatal("owner did not close once")
	}
}
func TestRunFilesBuildFailureReleasesGrant(t *testing.T) {
	spec, s := runFilesViewSpec(t, true)
	spec.WorkDir = filepath.Join(t.TempDir(), "missing")
	e, err := BuildExecutor(spec)
	if e != nil {
		e.Close()
	}
	if err == nil || s.opens != 1 || s.grant.closed != 1 {
		t.Fatalf("post-allocation failure leaks: %v opens%d grant%+v", err, s.opens, s.grant)
	}
}
func TestRunFilesBuildMissingStrictCapability(t *testing.T) {
	spec, s := runFilesViewSpec(t, true)
	spec.Runs = runFilesViewNoCapability{s.base}
	e, err := BuildExecutor(spec)
	if e != nil {
		e.Close()
	}
	if !errors.Is(err, store.ErrRunDataCapability) || s.opens != 0 {
		t.Fatal("strict capability disappeared", err)
	}
}
func TestRunFilesBuildLegacyWithoutCapability(t *testing.T) {
	spec, s := runFilesViewSpec(t, false)
	spec.Runs = runFilesViewNoCapability{s.base}
	spec.Store = s.base
	e, err := BuildExecutor(spec)
	if err != nil {
		t.Fatal("legacy refused", err)
	}
	defer e.Close()
	if e.RunFilesGrant() != nil || s.opens != 0 {
		t.Fatal("legacy acquired strict grant")
	}
}
