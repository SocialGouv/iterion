package store

import (
	"context"
	"errors"
	"fmt"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunFilesPathsKeepOriginalFormat(t *testing.T) {
	for _, from := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(from), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx := t.Context()
			if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: from}); e != nil {
				t.Fatal(e)
			}
			oldPath, e := s.EnsureRunFilesDir(ctx, "r")
			if e != nil {
				t.Fatal(e)
			}
			if e = s.DeleteRun(ctx, "r"); e != nil {
				t.Fatal(e)
			}
			if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Second)); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 3 - from}); e != nil {
				t.Fatal(e)
			}
			newPath, e := s.EnsureRunFilesDir(ctx, "r")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(newPath, "a.txt"), []byte("new"), 0600); e != nil {
				t.Fatal(e)
			}
			if oldPath == newPath {
				t.Fatal("returned path reused across formats")
			}
			if e = os.MkdirAll(oldPath, 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(oldPath, "a.txt"), []byte("old"), 0600); e != nil {
				t.Fatal(e)
			}
			rc, _, e := s.OpenRunFile(ctx, "r", "a.txt")
			if e != nil {
				t.Fatal(e)
			}
			defer rc.Close()
			p, e := io.ReadAll(rc)
			if e != nil || string(p) != "new" {
				t.Fatal(string(p), e)
			}
			old, e := WithRunValueFormat(ctx, RunValueFormat{RunID: "r", Format: from})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.EnsureRunFilesDir(old, "r"); !errors.Is(e, ErrValueFormatChanged) {
				t.Fatal(e)
			}
			if _, e = s.ListRunFiles(old, "r"); !errors.Is(e, ErrValueFormatChanged) {
				t.Fatal(e)
			}
			t.Logf("old granted path=%s; successor=%s; successor bytes=%q", oldPath, newPath, p)
		})
	}
}
func TestOpenedLogDescriptorKeepsOldInode(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx := t.Context()
	if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 1}); e != nil {
		t.Fatal(e)
	}
	logger, closer := TeeRunLog(iterlog.Nop(), iterlog.LevelInfo, s.root, "r")
	if closer == nil {
		t.Fatal("no logger")
	}
	defer closer.Close()
	if e = s.DeleteRun(ctx, "r"); e != nil {
		t.Fatal(e)
	}
	if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Second)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); e != nil {
		t.Fatal(e)
	}
	if e = s.AppendRunLog(context.Background(), "r", 0, []byte("new")); e != nil {
		t.Fatal(e)
	}
	logger.Info("late old descriptor")
	p, e := s.ReadRunLogRange(ctx, "r", 0, 0)
	if e != nil || string(p) != "new" {
		t.Fatal(string(p), e)
	}
	t.Log("old descriptor stays on unlinked inode; public new run log unchanged")
}
