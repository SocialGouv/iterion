package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type r53ScratchBlob struct {
	*valueFormatTestBlob
	mu     sync.Mutex
	scopes map[string]*inMemoryBlob
}

func (b *r53ScratchBlob) RunObjectsForScope(scope store.IncarnationDataScope) (blob.RunObjects, error) {
	key, e := scope.BlobPrefix()
	if e != nil {
		return nil, e
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.scopes[key]
	if v == nil {
		v = newInMemoryBlob()
		b.scopes[key] = v
	}
	return v, nil
}
func (b *r53ScratchBlob) ArtifactsForScope(scope store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	v, e := b.RunObjectsForScope(scope)
	if e != nil {
		return nil, e
	}
	return scopeArtifactFixture{v.(*inMemoryBlob)}, nil
}
func r53ScratchCreate(t *testing.T, s *Store, ctx context.Context, f store.ValueFormat) (*store.Run, context.Context, string) {
	t.Helper()
	r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "same", WorkflowName: "wf", Format: f, DataProtocol: store.IncarnationRunData})
	if e != nil {
		t.Fatal(e)
	}
	h := store.RunDataHeader{RunID: r.ID, Format: f, Protocol: r.DataProtocol, BirthID: r.BirthID}
	b, e := store.CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunDataBinding(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	scope, e := store.ResolveIncarnationDataScope(ctx, h)
	if e != nil {
		t.Fatal(e)
	}
	prefix, e := scope.BlobPrefix()
	if e != nil {
		t.Fatal(e)
	}
	return r, ctx, prefix
}
func TestR53MongoScratchGrantedPathMustRemainBirthDisjoint(t *testing.T) {
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		t.Skip("incarnation admission remains disabled")
	}
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			b := &r53ScratchBlob{valueFormatTestBlob: &valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}, scopes: map[string]*inMemoryBlob{}}
			s.blob = b
			old, oldctx, oldprefix := r53ScratchCreate(t, s, ctx, f)
			oldpath, e := s.EnsureRunFilesDir(oldctx, old.ID)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.DeleteRun(oldctx, old.ID); e != nil {
				t.Fatal(e)
			}
			if _, e := os.Stat(oldpath); !os.IsNotExist(e) {
				t.Fatalf("delete kept its own birth scratch: %v", e)
			}
			if n, e := s.PruneDeletionMarkers(oldctx, time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			next, nextctx, nextprefix := r53ScratchCreate(t, s, ctx, f)
			nextpath, e := s.EnsureRunFilesDir(nextctx, next.ID)
			if e != nil {
				t.Fatal(e)
			}
			if old.BirthID == next.BirthID || oldprefix == nextprefix {
				t.Fatal("fixture did not create distinct births")
			}
			t.Logf("distinct blob scopes: %s != %s; local paths equal=%v", oldprefix, nextprefix, oldpath == nextpath)
			if e = os.WriteFile(filepath.Join(nextpath, "report.txt"), []byte("successor bytes"), 0600); e != nil {
				t.Fatal(e)
			}
			// The tool retained the valid path grant. It does not call the old API again.
			if e = os.MkdirAll(oldpath, 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(oldpath, "report.txt"), []byte("late predecessor bytes"), 0600); e != nil {
				t.Fatal(e)
			}
			if n, e := s.UploadRunFiles(nextctx, next.ID); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			if _, e := os.Stat(nextpath); !os.IsNotExist(e) {
				t.Fatalf("successful upload kept successor scratch: %v", e)
			}
			if oldpath != nextpath {
				late, readErr := os.ReadFile(filepath.Join(oldpath, "report.txt"))
				if readErr != nil || string(late) != "late predecessor bytes" {
					t.Fatalf("successor cleanup touched retired birth: %q %v", late, readErr)
				}
			}
			rc, _, e := s.OpenRunFile(nextctx, next.ID, "report.txt")
			if e != nil {
				t.Fatal(e)
			}
			got, e := io.ReadAll(rc)
			rc.Close()
			if e != nil {
				t.Fatal(e)
			}
			if string(got) != "successor bytes" {
				t.Fatalf("predecessor path grant contaminated successor blob despite disjoint scopes: %q", got)
			}
		})
	}
}
