package runner

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type runFilesRunnerGrant struct {
	store.RunFilesGrant
	closes int
}

func (g *runFilesRunnerGrant) Close() error { g.closes++; return g.RunFilesGrant.Close() }

type runFilesRunnerStore struct {
	*store.FilesystemRunStore
	t              *testing.T
	grant          *runFilesRunnerGrant
	opens, uploads int
}

func (s *runFilesRunnerStore) OpenRunFilesGrant(ctx context.Context, id string) (store.RunFilesGrant, error) {
	g, e := s.FilesystemRunStore.OpenRunFilesGrant(ctx, id)
	if e != nil {
		return nil, e
	}
	s.opens++
	s.grant = &runFilesRunnerGrant{RunFilesGrant: g}
	return s.grant, nil
}
func (s *runFilesRunnerStore) UploadRunFiles(context.Context, string) (int, error) {
	s.t.Error("legacy upload chosen for strict run")
	return 0, nil
}
func (s *runFilesRunnerStore) UploadRunFilesFrom(ctx context.Context, id string, g store.RunFilesGrant) (int, error) {
	s.uploads++
	if g != s.grant || s.grant.closes != 0 {
		s.t.Error("original grant not live at upload")
	}
	root, e := g.OpenRoot(ctx)
	if e != nil {
		s.t.Error(e)
	} else {
		root.Close()
	}
	r, e := s.LoadRun(ctx, id)
	if e != nil || r.Status != store.RunStatusFinished {
		s.t.Fatalf("upload before terminal: %+v %v", r, e)
	}
	return 0, nil
}
func TestRunFilesPrincipalRunnerClosesAfterUpload(t *testing.T) {
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		t.Skip(err)
	}
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := store.WithIdentity(context.Background(), "team", "owner")
	base, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	row, e := base.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "main", Format: 1, DataProtocol: store.IncarnationRunData})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: 1, Protocol: 1, BirthID: row.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunDataBinding(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "r", Format: 1})
	if e != nil {
		t.Fatal(e)
	}
	p := parsertest.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, e := ast.MarshalFile(p.File)
	if e != nil {
		t.Fatal(e)
	}
	s := &runFilesRunnerStore{FilesystemRunStore: base, t: t}
	r := &Runner{cfg: Config{Store: s, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
	if e = r.executeRun(ctx, &queue.RunMessage{RunID: "r", TenantID: "team", OwnerID: "owner", WorkflowName: "main", IRCompiled: body}, nil, nil); e != nil {
		t.Fatal(e)
	}
	if s.opens != 1 || s.uploads != 1 || s.grant == nil || s.grant.closes != 1 {
		t.Fatalf("owner lifecycle opens%d uploads%d grant%+v", s.opens, s.uploads, s.grant)
	}
}
