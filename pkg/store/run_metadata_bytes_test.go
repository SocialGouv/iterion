package store

import (
	"bytes"
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type metadataCountingReader struct{ reads int }

func (r *metadataCountingReader) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestRunMetadataFilesystemBytesRefuseBeforeEffects(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.runJSONPath("r"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"format_version": 2`, `"format_version": 99`, 1))
	if err = os.WriteFile(s.runJSONPath("r"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		err := filepath.WalkDir(s.runDir("r"), func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !d.IsDir() {
				b, e := os.ReadFile(p)
				if e != nil {
					return e
				}
				out[p] = string(b)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, closer, err := TeeRunLogContext(ctx, iterlog.Nop(), iterlog.LevelInfo, s.root, "r"); !errors.Is(err, ErrUnsupportedValueFormat) || closer != nil {
		t.Fatal("typed log open", closer, err)
	}
	if _, closer := TeeRunLog(iterlog.Nop(), iterlog.LevelInfo, s.root, "r"); closer != nil {
		closer.Close()
		t.Fatal("legacy tee opened future run")
	}

	before := snapshot()
	input := &metadataCountingReader{}
	actions := []struct {
		name string
		call func() error
	}{
		{"attachment", func() error { return s.WriteAttachment(ctx, "r", AttachmentRecord{Name: "a"}, input) }},
		{"remove-attachment", func() error { return s.RemoveAttachment(ctx, "r", "a") }},
		{"delete-attachments", func() error { return s.DeleteRunAttachments(ctx, "r") }},
		{"diff", func() error { return s.PutRunDiffBlob(ctx, "r", "ref", []byte("bad")) }},
		{"session", func() error { return s.PutBackendSession(ctx, "r", "ref", []byte("bad")) }},
		{"delete-session", func() error { return s.DeleteBackendSession(ctx, "r", "ref") }},
		{"scratch", func() error { return s.PutScratchBank(ctx, "r", input, 0) }},
		{"delete-scratch", func() error { return s.DeleteScratchBank(ctx, "r") }},
		{"tool", func() error { _, err := s.WriteToolBlob(ctx, "r", "u", "output", []byte("bad")); return err }},
		{"pid", func() error { return s.WritePIDFile("r", 123) }},
		{"delete-pid", func() error { return s.RemovePIDFile("r") }},
		{"attempt", func() error { _, err := s.RecordAttempt(ctx, "r", &AttemptSnapshot{}); return err }},
	}
	for _, a := range actions {
		if err := a.call(); !errors.Is(err, ErrUnsupportedValueFormat) {
			t.Fatalf("%s: %v", a.name, err)
		}
	}
	if input.reads != 0 {
		t.Fatal("future header read body", input.reads)
	}
	after := snapshot()
	if len(after) != len(before) {
		t.Fatal("refusal created files", after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatal("refusal changed", k)
		}
	}
	staged, err := filepath.Glob(filepath.Join(s.root, ".run-body-stage-*"))
	if err != nil || len(staged) != 0 {
		t.Fatal(staged, err)
	}
}

type metadataBlockingReader struct {
	entered, release chan struct{}
	once             sync.Once
	source           io.Reader
	ctx              context.Context
}

func (r *metadataBlockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.entered)
		select {
		case <-r.release:
		case <-r.ctx.Done():
		}
	})
	return r.source.Read(p)
}

func TestRunMetadataFilesystemStreamKeepsOriginalFormat(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"attachment", "scratch"} {
		for _, mode := range []string{"changed", "future", "same-format"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, err := New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				other, err := New(s.root)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if _, err = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); err != nil {
					t.Fatal(err)
				}
				input := &metadataBlockingReader{entered: make(chan struct{}), release: make(chan struct{}), source: strings.NewReader("body"), ctx: ctx}
				release := sync.OnceFunc(func() { close(input.release) })
				defer release()
				done := make(chan error, 1)
				go func() {
					if kind == "attachment" {
						done <- s.WriteAttachment(ctx, "r", AttachmentRecord{Name: "a"}, input)
					} else {
						done <- s.PutScratchBank(ctx, "r", input, 4)
					}
				}()
				select {
				case <-input.entered:
				case err := <-done:
					t.Fatalf("no streaming barrier: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var want error
				switch mode {
				case "same-format":
					err = other.SetRunnerVersion(ctx, "r", "concurrent")
				case "changed":
					if err = other.DeleteRun(ctx, "r"); err != nil {
						t.Fatal(err)
					}
					if n, e := other.PruneDeletionMarkers(ctx, time.Now().Add(time.Second)); e != nil || n != 1 {
						t.Fatal(n, e)
					}
					_, err = other.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 1})
					want = ErrValueFormatChanged
				default:
					raw, e := os.ReadFile(s.runJSONPath("r"))
					if e != nil {
						t.Fatal(e)
					}
					raw = []byte(strings.Replace(string(raw), `"format_version": 2`, `"format_version": 99`, 1))
					err = os.WriteFile(s.runJSONPath("r"), raw, 0600)
					want = ErrUnsupportedValueFormat
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(s.runJSONPath("r"))
				if err != nil {
					t.Fatal(err)
				}
				release()
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if want != nil {
					if !errors.Is(err, want) {
						t.Fatal(err, want)
					}
					after, e := os.ReadFile(s.runJSONPath("r"))
					if e != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed parent", e)
					}
					if _, e = os.Stat(filepath.Join(s.runDir("r"), "attachments", "a")); !os.IsNotExist(e) {
						t.Fatal("published attachment", e)
					}
					if _, e = os.Stat(filepath.Join(s.runDir("r"), "scratch-bank.tgz")); !os.IsNotExist(e) {
						t.Fatal("published bank", e)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				staged, e := filepath.Glob(filepath.Join(s.root, ".run-body-stage-*"))
				if e != nil || len(staged) != 0 {
					t.Fatal(staged, e)
				}
			})
		}
	}
}
