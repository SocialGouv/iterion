package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationFilesystemGrantLifecycle(t *testing.T) {
	for _, format := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := directoryFixture(t, format)
			g, e := s.OpenRunFilesGrant(ctx, "r")
			if e != nil {
				t.Fatal(e)
			}
			defer g.Close()
			p := g.HostPath()
			if e = os.WriteFile(filepath.Join(p, "out"), []byte("original"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = s.UpdateRunStatus(ctx, "r", RunStatusFinished, ""); e != nil {
				t.Fatal(e)
			}
			stopped, cancel := context.WithCancel(ctx)
			cancel()
			bound := context.WithoutCancel(stopped)
			root, e := g.OpenRoot(bound)
			if e != nil {
				t.Fatal("post-terminal original denied", e)
			}
			root.Close()
			if e = os.Rename(p, p+"-held"); e != nil {
				t.Fatal(e)
			}
			if e = os.Mkdir(p, 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(p, "out"), []byte("foreign"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = g.ValidateHostPath(bound); !errors.Is(e, ErrRunDataIdentityChanged) {
				t.Fatal(e)
			}
			root, e = g.OpenRoot(bound)
			if e != nil {
				t.Fatal(e)
			}
			body, e := root.ReadFile("out")
			root.Close()
			if e != nil || string(body) != "original" {
				t.Fatal(string(body), e)
			}
			if e = s.DeleteRun(ctx, "r"); e != nil {
				t.Fatal(e)
			}
			if root, e = g.OpenRoot(bound); e == nil {
				root.Close()
				t.Fatal("deleted birth grant reacquired")
			}
		})
	}
}
