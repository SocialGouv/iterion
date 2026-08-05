package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/knowledge"
)

func TestExportImportRoundTrip(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := DefaultFSStore()
	ctx := context.Background()
	src := botRef("notes")

	if _, err := s.WriteDocument(ctx, src, knowledge.DocumentInput{Path: "a.md", Content: []byte("# A\nbody")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteDocument(ctx, src, knowledge.DocumentInput{Path: "sub/b.md", Content: []byte("---\ntitle: B\n---\nb")}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	m, err := knowledge.ExportSpace(ctx, s, src, &buf)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if m.DocCount != 2 || m.Format != knowledge.ExportFormat {
		t.Fatalf("manifest: %+v", m)
	}

	// Import into a different (fresh) space.
	dst := LegacyBotRef("/tmp/other-project", "notes")
	sum, err := knowledge.ImportSpace(ctx, s, dst, &buf, knowledge.ImportSkip)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sum.Imported != 2 {
		t.Fatalf("imported=%d want 2", sum.Imported)
	}
	doc, err := s.ReadDocument(ctx, dst, "sub/b.md")
	if err != nil || string(doc.Content) != "---\ntitle: B\n---\nb" {
		t.Fatalf("round-trip body: %q err=%v", doc.Content, err)
	}

	// Re-import with skip strategy → all skipped.
	buf.Reset()
	knowledge.ExportSpace(ctx, s, src, &buf)
	sum2, _ := knowledge.ImportSpace(ctx, s, dst, &buf, knowledge.ImportSkip)
	if sum2.Skipped != 2 || sum2.Imported != 0 {
		t.Fatalf("skip re-import: %+v", sum2)
	}
}

func TestExportRejectsSecret(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := DefaultFSStore()
	ctx := context.Background()
	ref := botRef("notes")
	if _, err := s.WriteDocument(ctx, ref, knowledge.DocumentInput{Path: "leak.md", Content: []byte("token: ghp_0123456789abcdefghijklmnopqrstuvwxyz")}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err := knowledge.ExportSpace(ctx, s, ref, &buf)
	var secErr *knowledge.ErrSecretInExport
	if !errors.As(err, &secErr) {
		t.Fatalf("expected ErrSecretInExport, got %v", err)
	}
}
