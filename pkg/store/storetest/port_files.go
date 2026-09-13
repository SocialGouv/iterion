package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func RunPortFiles(t *testing.T, factory Factory) {
	s := factory(t)
	ctx := store.WithRuntimeSemantics(testCtx(), store.RuntimeSemanticsPortsV1)
	const id = "pc1_captured_files"
	if _, err := s.CreateRun(ctx, id, "files", nil); err != nil {
		t.Fatal(err)
	}
	publisher, files := store.AsPortFilesStore(s), store.AsRunFilesStore(s)
	if publisher == nil || files == nil {
		t.Fatal("missing native file publication seam")
	}
	body := bytes.Repeat([]byte("verified output\x00\xff"), 128*1024)
	digest := sha256.Sum256(body)
	ref := store.PortFileRef{RunID: id, Producer: "producer", Attempt: 1, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(body)), MediaType: "application/octet-stream"}
	var err error
	ref.Path, err = store.PortFilePath(ref.Producer, ref.Attempt, ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate := func() {
		t.Helper()
		reader, _, err := files.OpenRunFile(ctx, id, ref.Path)
		if err == nil {
			_ = reader.Close()
			t.Fatal("captured bytes became visible before atomic publication")
		}
	}
	if err := publisher.PutPortFile(ctx, ref, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	assertPrivate()
	for name, candidate := range map[string][]byte{
		"different same-size body": bytes.Repeat([]byte("x"), len(body)),
		"truncated":                body[:len(body)-1], "extra byte": append(append([]byte(nil), body...), 'x'),
	} {
		t.Run(name, func(t *testing.T) {
			if err := publisher.PutPortFile(ctx, ref, bytes.NewReader(candidate)); err == nil {
				t.Fatal("bad content overwrote a content-addressed file")
			}
			assertPrivate()
		})
	}
	if err := publisher.PutPortFile(ctx, ref, bytes.NewReader(body)); err != nil {
		t.Fatalf("identical retry failed: %v", err)
	}
	// An ordinary tool may leave a stale scratch file with the same text
	// path. Native public reads resolve the captured file, never that shadow.
	scratch, err := files.EnsureRunFilesDir(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(scratch, filepath.FromSlash(ref.Path))
	if err := os.MkdirAll(filepath.Dir(shadow), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shadow, []byte("stale scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if uploader := store.AsRunFilesUploader(s); uploader != nil {
		if n, err := uploader.UploadRunFiles(ctx, id); err != nil || n != 0 {
			t.Fatalf("scratch upload overwrote an immutable captured file: %d, %v", n, err)
		}
	}
	assertPrivate()
	listed, err := files.ListRunFiles(ctx, id)
	if err != nil || len(listed) != 0 {
		t.Fatalf("unpublished file appeared in public list: %+v, %v", listed, err)
	}
	loaded, err := s.LoadRun(ctx, id)
	if err != nil || loaded.PortExecution != nil {
		t.Fatalf("capturing bytes implicitly published a workflow result: %+v, %v", loaded, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := publisher.PutPortFile(canceled, ref, bytes.NewReader(body)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled capture: %v", err)
	}
	if err := s.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if reader, _, err := files.OpenRunFile(ctx, id, ref.Path); err == nil {
		reader.Close()
		t.Fatal("captured file survived native deletion")
	}
}
