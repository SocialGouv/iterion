package runner

import (
	"context"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"testing"
)

type runFilesBorrowedGrant struct {
	binding store.RunDataBinding
	closes  int
}

func (g *runFilesBorrowedGrant) HostPath() string                       { return "/unused" }
func (g *runFilesBorrowedGrant) ValidateHostPath(context.Context) error { return nil }
func (g *runFilesBorrowedGrant) OpenRoot(context.Context) (*os.Root, error) {
	return nil, store.ErrRunDataCapability
}
func (g *runFilesBorrowedGrant) Close() error { g.closes++; return nil }
func (g *runFilesBorrowedGrant) Bind(ctx context.Context, id string) (context.Context, error) {
	h := g.binding.Header()
	if id != h.RunID {
		return nil, store.ErrRunDataIdentityChanged
	}
	ctx, e := store.WithRunDataBinding(ctx, g.binding)
	if e != nil {
		return nil, e
	}
	return store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: id, Format: h.Format})
}

type runFilesGrantUploader struct {
	store.RunStore
	legacy, granted int
	got             *runFilesBorrowedGrant
	ctx             context.Context
}

func (s *runFilesGrantUploader) UploadRunFiles(context.Context, string) (int, error) {
	s.legacy++
	return 0, nil
}
func (s *runFilesGrantUploader) UploadRunFilesFrom(ctx context.Context, id string, g store.RunFilesGrant) (int, error) {
	s.granted++
	bound, e := g.Bind(ctx, id)
	if e != nil {
		return 0, e
	}
	s.ctx = bound
	s.got = g.(*runFilesBorrowedGrant)
	return 0, nil
}
func TestRunFilesUploadGrantGovernsUnboundContext(t *testing.T) {
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: 2, Protocol: 1, BirthID: "original"})
	if e != nil {
		t.Fatal(e)
	}
	g := &runFilesBorrowedGrant{binding: b}
	s := &runFilesGrantUploader{}
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.Nop()}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.uploadRunFilesWithGrant(ctx, &queue.RunMessage{RunID: "r", TenantID: "t", OwnerID: "o"}, g)
	if s.legacy != 0 || s.granted != 1 || s.got != g {
		t.Fatalf("grant discarded: legacy=%d granted=%d same=%v", s.legacy, s.granted, s.got == g)
	}
	if g.closes != 0 {
		t.Fatal("borrowed grant closed")
	}
}
func TestRunFilesUploadGrantRejectsSuccessorBeforeDispatch(t *testing.T) {
	h := store.RunDataHeader{RunID: "r", Format: 2, Protocol: 1, BirthID: "original"}
	b, _ := store.CaptureRunDataBinding(h)
	g := &runFilesBorrowedGrant{binding: b}
	h.BirthID = "successor"
	other, _ := store.CaptureRunDataBinding(h)
	ctx, e := store.WithRunDataBinding(context.Background(), other)
	if e != nil {
		t.Fatal(e)
	}
	s := &runFilesGrantUploader{}
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.Nop()}}
	r.uploadRunFilesWithGrant(ctx, &queue.RunMessage{RunID: "r", TenantID: "t", OwnerID: "o"}, g)
	if s.ctx != nil || s.legacy != 0 || s.granted != 0 {
		t.Fatal("successor obtained an upload")
	}
	if g.closes != 0 {
		t.Fatal("borrowed grant closed")
	}
}

type runFilesNewOnlyUploader struct {
	store.RunStore
	inner runFilesGrantUploader
}

func (s *runFilesNewOnlyUploader) UploadRunFilesFrom(ctx context.Context, id string, g store.RunFilesGrant) (int, error) {
	return s.inner.UploadRunFilesFrom(ctx, id, g)
}
func TestRunFilesUploadGrantNewCapabilityOnly(t *testing.T) {
	h := store.RunDataHeader{RunID: "r", Format: 2, Protocol: 1, BirthID: "original"}
	b, _ := store.CaptureRunDataBinding(h)
	g := &runFilesBorrowedGrant{binding: b}
	ctx, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	s := &runFilesNewOnlyUploader{}
	r := &Runner{cfg: Config{Store: s, Logger: iterlog.Nop()}}
	r.uploadRunFilesWithGrant(ctx, &queue.RunMessage{RunID: "r", TenantID: "t", OwnerID: "o"}, g)
	if s.inner.granted != 1 || s.inner.got != g {
		t.Fatalf("grant-aware-only store ignored: calls=%d", s.inner.granted)
	}
}
