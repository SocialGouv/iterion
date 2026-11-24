package runstream

import (
	"fmt"
	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/store"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestFilesystemReadonlyNoLockMatrix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions oracle; Windows has separate PID-lock tests")
	}
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			src, st, root := newFileSourceFixture(t)
			defer src.Close()
			ctx, r := fsRetainedBirth(t, st, format, 0)
			if _, e := st.AppendEvent(ctx, r.ID, store.Event{Data: map[string]any{"marker": "readable"}}); e != nil {
				t.Fatal(e)
			}
			if e := st.AppendRunLog(ctx, r.ID, 0, []byte("readable")); e != nil {
				t.Fatal(e)
			}
			lockdir := filepath.Join(root, ".run-write-locks")
			testfs.RemoveAll(t, root, lockdir)
			if e := os.Chmod(root, 0500); e != nil {
				t.Fatal(e)
			}
			defer os.Chmod(root, 0700)
			snapshot := func() map[string]string {
				out := map[string]string{}
				if e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					rel, e := filepath.Rel(root, path)
					if e != nil {
						return e
					}
					info, e := d.Info()
					if e != nil {
						return e
					}
					value := info.Mode().String()
					if !d.IsDir() {
						raw, e := os.ReadFile(path)
						if e != nil {
							return e
						}
						value += string(raw)
					}
					out[rel] = value
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				return out
			}
			before := snapshot()
			es, e := src.SubscribeEvents(ctx, r.ID, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer es.Close()
			ls, e := src.SubscribeLogs(ctx, r.ID, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer ls.Close()
			if event := fsRetainedEvent(t, es); event.Data["marker"] != "readable" {
				t.Fatal(event)
			}
			if chunk := fsRetainedLog(t, ls); string(chunk.Data) != "readable" {
				t.Fatal(chunk)
			}
			es.Close()
			ls.Close()
			if after := snapshot(); !reflect.DeepEqual(before, after) {
				t.Fatal("reader mutated filesystem", before, after)
			}
		})
	}
}
