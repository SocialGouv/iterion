package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// TestFSStore_GlobalSpaceAnswersToPlatformAuthority drives the ungranted
// path of the shared gate on the filesystem adapter: the local store answers
// the same question the cloud one does, so the two cannot drift apart (the
// quota halves already had drifted — the FS aggregate charged a global write,
// the cloud one charged nothing).
//
// Each arm asserts the DISK, not the error alone: a gate that returned a
// refusal after writing the bytes would pass on the error and fail here.
func TestFSStore_GlobalSpaceAnswersToPlatformAuthority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	s := DefaultFSStore()
	ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}

	// A run's context: no grant.
	_, err := s.WriteDocument(context.Background(), ref, knowledge.DocumentInput{Path: "a.md", Content: []byte("# pollution")})
	if !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("ungranted global write: err=%v, want knowledge.ErrGlobalWriteForbidden", err)
	}
	root, rerr := s.Root(ref)
	if rerr != nil {
		t.Fatalf("root: %v", rerr)
	}
	if _, serr := os.Stat(filepath.Join(root, "a.md")); !os.IsNotExist(serr) {
		t.Fatalf("the refused write landed on disk anyway at %s (stat err=%v)", root, serr)
	}

	// The operator's context: the capability stands.
	granted := knowledge.WithPlatformWriteAuthority(context.Background())
	if _, err := s.WriteDocument(granted, ref, knowledge.DocumentInput{Path: "a.md", Content: []byte("# catalogue")}); err != nil {
		t.Fatalf("granted global write: %v", err)
	}
	if doc, err := s.ReadDocument(context.Background(), ref, "a.md"); err != nil || string(doc.Content) != "# catalogue" {
		t.Fatalf("readback: %q err=%v — the global space stays READABLE without a grant", string(doc.Content), err)
	}

	// Delete is a mutation too: the half the ticket's "pollute or WIPE"
	// wording names, and the one a per-handler guard forgets first.
	if err := s.DeleteDocument(context.Background(), ref, "a.md"); !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("ungranted global delete: err=%v, want knowledge.ErrGlobalWriteForbidden", err)
	}
	if _, err := s.ReadDocument(context.Background(), ref, "a.md"); err != nil {
		t.Fatalf("the refused delete removed the document anyway: %v", err)
	}
	if err := s.DeleteDocument(granted, ref, "a.md"); err != nil {
		t.Fatalf("granted global delete: %v", err)
	}

	// A team-scoped space is reached by its own org only, and stays ungated.
	orgRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "acme", Name: "conventions"}
	if _, err := s.WriteDocument(context.Background(), orgRef, knowledge.DocumentInput{Path: "a.md", Content: []byte("x")}); err != nil {
		t.Fatalf("team-scoped write must stay ungated: %v", err)
	}
	if err := s.DeleteDocument(context.Background(), orgRef, "a.md"); err != nil {
		t.Fatalf("team-scoped delete must stay ungated: %v", err)
	}
}

// TestFSStore_ImportHonoursTheGate: the archive importer is a writer like any
// other — it goes through WriteDocument — so the one gate covers it without a
// second guard of its own. This is the path `POST /api/memory/import` and
// `iterion memory import` take.
func TestFSStore_ImportHonoursTheGate(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	s := DefaultFSStore()
	src := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "acme", Name: "src"}
	if _, err := s.WriteDocument(context.Background(), src, knowledge.DocumentInput{Path: "a.md", Content: []byte("# a")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf bytes.Buffer
	if _, err := knowledge.ExportSpace(context.Background(), s, src, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	archive := buf.Bytes()

	dst := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}
	if _, err := knowledge.ImportSpace(context.Background(), s, dst, bytes.NewReader(archive), knowledge.ImportStrategy("overwrite")); !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("ungranted import into the global space: err=%v, want knowledge.ErrGlobalWriteForbidden", err)
	}
	granted := knowledge.WithPlatformWriteAuthority(context.Background())
	if _, err := knowledge.ImportSpace(granted, s, dst, bytes.NewReader(archive), knowledge.ImportStrategy("overwrite")); err != nil {
		t.Fatalf("granted import: %v", err)
	}
}
