package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type bindingDrain struct {
	drainFakeRun
	t      *testing.T
	header store.RunDataHeader
	seen   context.Context
}

func (d *bindingDrain) ExportRunFiles(ctx context.Context) error {
	d.seen = ctx
	if ctx.Err() != nil {
		d.t.Error("inherited cancellation")
	}
	if v, ok := store.TenantFromContext(ctx); !ok || v != "team-original" {
		d.t.Error("tenant lost")
	}
	if v, ok := store.OwnerFromContext(ctx); !ok || v != "owner-original" {
		d.t.Error("owner lost")
	}
	b, ok := store.RunDataBindingFrom(ctx)
	if !ok || b.Header() != d.header {
		d.t.Error("original birth lost")
	}
	if _, ok := ctx.Deadline(); !ok {
		d.t.Error("unbounded cleanup")
	}
	return nil
}
func TestCleanupDrainRetainsConcreteStoreIdentity(t *testing.T) {
	h := store.RunDataHeader{RunID: "r", Format: 2, Protocol: store.IncarnationRunData, BirthID: "original"}
	b, e := store.CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunDataBinding(store.WithIdentity(context.Background(), "team-original", "owner-original"), b)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	d := &bindingDrain{t: t, header: h}
	drainRunFilesOnCleanup(ctx, d, nil, nil)
	if d.seen == nil {
		t.Fatal("drain omitted")
	}
	if d.seen.Err() != context.Canceled {
		t.Fatal("bounded cleanup context leaked")
	}
}
