package runner

import (
	"context"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"testing"
	"time"
)

type capturedBirthUploader struct {
	store.RunStore
	t      *testing.T
	want   store.RunDataHeader
	called bool
	grant  store.RunFilesGrant
	legacy bool
}

func (u *capturedBirthUploader) UploadRunFiles(ctx context.Context, id string) (int, error) {
	u.legacy = true
	return 0, nil
}
func (u *capturedBirthUploader) UploadRunFilesFrom(ctx context.Context, id string, grant store.RunFilesGrant) (int, error) {
	u.called = true
	if grant != u.grant {
		u.t.Error("upload replaced original grant")
	}
	b, ok := store.RunDataBindingFrom(ctx)
	if !ok || b.Header() != u.want {
		u.t.Errorf("upload lost original binding: got %+v/%v want %+v", b.Header(), ok, u.want)
	}
	if ctx.Err() != nil {
		u.t.Errorf("upload inherits cancellation: %v", ctx.Err())
	}
	d, ok := ctx.Deadline()
	if !ok || time.Until(d) <= 0 || time.Until(d) > uploadRunFilesBudget {
		u.t.Error("upload lacks independent bounded deadline")
	}
	tenant, _ := store.TenantFromContext(ctx)
	if tenant != "team" || id != u.want.RunID {
		u.t.Error("identity lost")
	}
	return 0, nil
}
func TestRunFilesUploadRetainsBirthAfterCancellation(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		h := store.RunDataHeader{RunID: "r", Format: f, Protocol: store.IncarnationRunData, BirthID: "original-birth"}
		b, err := store.CaptureRunDataBinding(h)
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := store.WithRunDataBinding(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := store.NewRetainedRunFilesGrant(dir, root, b, func(context.Context) error { return nil })
		if err != nil {
			_ = root.Close()
			t.Fatal(err)
		}
		defer grant.Close()
		up := &capturedBirthUploader{t: t, want: h, grant: grant}
		r := &Runner{cfg: Config{Store: up, Logger: iterlog.Nop()}}
		r.uploadRunFilesWithGrant(ctx, &queue.RunMessage{RunID: "r", TenantID: "team", OwnerID: "owner"}, grant)
		if up.legacy {
			t.Fatal("strict upload fell back to legacy path")
		}
		if !up.called {
			t.Fatal("upload omitted")
		}
	}
}

func TestRunFilesUploadMissingGrantRefusesPathFallback(t *testing.T) {
	h := store.RunDataHeader{RunID: "r", Format: store.ValueFormat(1), Protocol: store.IncarnationRunData, BirthID: "birth"}
	b, err := store.CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunDataBinding(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	up := &capturedBirthUploader{t: t, want: h}
	r := &Runner{cfg: Config{Store: up, Logger: iterlog.Nop()}}
	r.uploadRunFiles(ctx, &queue.RunMessage{RunID: "r"})
	if up.called || up.legacy {
		t.Fatal("missing grant uploaded through a pathname")
	}
	r.uploadRunFiles(context.Background(), &queue.RunMessage{RunID: "legacy"})
	if !up.legacy {
		t.Fatal("legacy upload omitted")
	}
}
