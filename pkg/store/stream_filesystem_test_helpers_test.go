package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func filesystemSelectionProbeStore(t *testing.T) *FilesystemRunStore {
	t.Helper()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func filesystemSelectionProbeWrite(t *testing.T, p, body string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func filesystemSelectionProbeHeader(t *testing.T, s *FilesystemRunStore, proto, format int) {
	t.Helper()
	filesystemSelectionProbeWrite(t, s.runJSONPath("r"), fmt.Sprintf(`{"id":"r","birth_id":"original","data_protocol":%d,"format_version":%d}`, proto, format))
}
func filesystemSelectionProbeSelect(t *testing.T, s *FilesystemRunStore, ctx context.Context) FilesystemStreamSelection {
	t.Helper()
	n, ok, e := s.OpenFilesystemStream(ctx, "r", FilesystemStreamOpenOptions{})
	if e != nil || !ok || n == nil {
		t.Fatalf("selection=%v observed=%v err=%v", n, ok, e)
	}
	t.Cleanup(func() { n.Close() })
	return n
}
func filesystemSelectionProbeRead(t *testing.T, n FilesystemStreamSelection, ctx context.Context) string {
	t.Helper()
	f, e := n.OpenLog(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	b, e := io.ReadAll(f)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
