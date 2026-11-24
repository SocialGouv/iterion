package runner

import (
	"context"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type runFilesOldOnlyUploader struct {
	store.RunStore
	calls int
}

func (s *runFilesOldOnlyUploader) UploadRunFiles(context.Context, string) (int, error) {
	s.calls++
	return 0, nil
}
func TestRunFilesUploadLegacyAndCapabilityControls(t *testing.T) {
	msg := &queue.RunMessage{RunID: "r", TenantID: "t", OwnerID: "o"}
	old := &runFilesOldOnlyUploader{}
	r := &Runner{cfg: Config{Store: old, Logger: iterlog.Nop()}}
	r.uploadRunFilesWithGrant(context.Background(), msg, nil)
	if old.calls != 1 {
		t.Fatal("legacy lost")
	}
	h := store.RunDataHeader{RunID: "r", Format: 2, Protocol: 1, BirthID: "original"}
	b, _ := store.CaptureRunDataBinding(h)
	g := &runFilesBorrowedGrant{binding: b}
	ctx, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	old.calls = 0
	r.uploadRunFilesWithGrant(ctx, msg, g)
	if old.calls != 0 {
		t.Fatal("strict fell back to legacy-only uploader")
	}
	both := &runFilesGrantUploader{}
	r.cfg.Store = both
	r.uploadRunFilesWithGrant(ctx, msg, nil)
	if both.legacy != 0 || both.granted != 0 {
		t.Fatal("missing strict grant dispatched")
	}
	wrong, e := store.WithRunValueFormat(context.Background(), store.RunValueFormat{RunID: "r", Format: 1})
	if e != nil {
		t.Fatal(e)
	}
	r.uploadRunFilesWithGrant(wrong, msg, g)
	if both.legacy != 0 || both.granted != 0 {
		t.Fatal("conflicting format dispatched")
	}
	fresh := &runFilesNewOnlyUploader{}
	r.cfg.Store = fresh
	r.uploadRunFilesWithGrant(context.Background(), msg, nil)
	if fresh.inner.granted != 0 {
		t.Fatal("legacy invented strict upload")
	}
	fs, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r.cfg.Store = fs
	r.uploadRunFilesWithGrant(ctx, msg, g)
	if g.closes != 0 {
		t.Fatal("no-op store closed borrowed grant")
	}
}
