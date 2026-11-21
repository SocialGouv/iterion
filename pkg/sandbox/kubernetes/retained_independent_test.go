package kubernetes

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"os"
	"testing"
)

type cancelledAfterOpen struct {
	path   string
	cancel context.CancelFunc
	root   *os.Root
}

func (o *cancelledAfterOpen) OpenRoot(context.Context) (*os.Root, error) {
	r, e := os.OpenRoot(o.path)
	o.root = r
	o.cancel()
	return r, e
}
func TestRetainedDrainCancellationAfterAcquireClosesBoth(t *testing.T) {
	p := t.TempDir()
	before, e := os.ReadDir("/proc/self/fd")
	if e != nil {
		t.Skip(e)
	}
	for range 16 {
		ctx, cancel := context.WithCancel(context.Background())
		o := &cancelledAfterOpen{path: p, cancel: cancel}
		tree, e := openReadBackDrainTree(ctx, sandbox.ReadBackDir{HostDir: p, RootOpener: o})
		if tree != nil {
			tree.close()
			t.Fatal("cancelled acquisition returned tree")
		}
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		if _, e = o.root.Stat("."); e == nil {
			t.Fatal("opener duplicate leaked")
		}
	}
	after, e := os.ReadDir("/proc/self/fd")
	if e != nil {
		t.Fatal(e)
	}
	if len(after) != len(before) {
		t.Fatalf("FD delta %d", len(after)-len(before))
	}
}
