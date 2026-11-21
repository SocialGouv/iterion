package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

func r53ScratchBound(t *testing.T, birth string, f store.ValueFormat) context.Context {
	t.Helper()
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: f, Protocol: store.IncarnationRunData, BirthID: birth})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunDataBinding(context.Background(), b)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}
func TestR53ScratchNamespaceBoundaries(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		base := filepath.Join(t.TempDir(), "scratch")
		s := &Store{runFilesScratch: base}
		a, e := s.runFilesScratchForBinding(r53ScratchBound(t, "a", f), "r", f)
		if e != nil {
			t.Fatal(e)
		}
		b, e := s.runFilesScratchForBinding(r53ScratchBound(t, "b", f), "r", f)
		if e != nil || a == b {
			t.Fatal(a, b, e)
		}
		if scratchRootContains(base, a) || scratchRootContains(base+"-v2", a) {
			t.Fatal("strict beneath historical cleanup")
		}
		if _, e = s.runFilesScratchForBinding(r53ScratchBound(t, "a", f), "other", f); !errors.Is(e, store.ErrRunDataIdentityChanged) {
			t.Fatal(e)
		}
		if _, e = os.Stat(base); !os.IsNotExist(e) {
			t.Fatal("selection unexpectedly creates root", e)
		}
		if e = os.MkdirAll(base, 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.Symlink(base, base+"-incarnations"); e != nil {
			t.Fatal(e)
		}
		if _, e = s.runFilesScratchForBinding(r53ScratchBound(t, "a", f), "r", f); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal("alias overlap not refused", e)
		}
		got, e := s.runFilesScratchForBinding(context.Background(), "r", f)
		want, e2 := s.runFilesScratchDirForFormat("r", f)
		if e != nil || e2 != nil || got != want {
			t.Fatal("legacy changed", got, want, e, e2)
		}
	}
}
