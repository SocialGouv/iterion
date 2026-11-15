package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMongoRunFilesGrantRetainsSourceAndRefusesReplacementCleanup(t *testing.T) {
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		t.Skip("incarnation admission remains disabled")
	}

	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			s.blob = &r53ScratchBlob{valueFormatTestBlob: &valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}, scopes: map[string]*inMemoryBlob{}}
			r, bound, _ := r53ScratchCreate(t, s, ctx, format)
			g, e := s.OpenRunFilesGrant(bound, r.ID)
			if e != nil {
				t.Fatal(e)
			}
			defer g.Close()
			p := g.HostPath()
			if e = os.WriteFile(filepath.Join(p, "report"), []byte("original"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.Rename(p, p+"-held"); e != nil {
				t.Fatal(e)
			}
			if e = os.Mkdir(p, 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(p, "report"), []byte("foreign"), 0600); e != nil {
				t.Fatal(e)
			}
			n, e := s.UploadRunFilesFrom(bound, r.ID, g)
			if n != 1 || !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatal(n, e)
			}
			f, _, e := s.OpenRunFile(bound, r.ID, "report")
			if e != nil {
				t.Fatal(e)
			}
			body, e := io.ReadAll(f)
			f.Close()
			if e != nil || string(body) != "original" {
				t.Fatal(string(body), e)
			}
			body, e = os.ReadFile(filepath.Join(p, "report"))
			if e != nil || string(body) != "foreign" {
				t.Fatal("replacement cleanup", string(body), e)
			}
		})
	}
}
func TestMongoRunFilesGrantAllowsOriginalPostTerminalUpload(t *testing.T) {
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		t.Skip("incarnation admission remains disabled")
	}

	s, ctx := valueFormatMongo(t, true)
	s.blob = &r53ScratchBlob{valueFormatTestBlob: &valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}, scopes: map[string]*inMemoryBlob{}}
	r, bound, _ := r53ScratchCreate(t, s, ctx, 2)
	g, e := s.OpenRunFilesGrant(bound, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	if e = os.WriteFile(filepath.Join(g.HostPath(), "report"), []byte("bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.UploadRunFilesFrom(bound, "other", g); !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	if e = s.UpdateRunStatus(bound, r.ID, store.RunStatusFinished, ""); e != nil {
		t.Fatal(e)
	}
	stopped, cancel := context.WithCancel(bound)
	cancel()
	if _, e = s.UploadRunFilesFrom(stopped, r.ID, g); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	n, e := s.UploadRunFilesFrom(context.WithoutCancel(stopped), r.ID, g)
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = os.Stat(g.HostPath()); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("own scratch remains", e)
	}
}
