package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// recordingMemoryStore is a knowledge.MemoryStore that answers an empty space
// and records every mutation attempt. It is deliberately PERMISSIVE: it never
// refuses a write of its own, so a test that sees no mutation saw the gate
// above it refuse — not a store that happened to say no.
type recordingMemoryStore struct {
	writes  []knowledge.DocumentInput
	deletes []string
}

func (s *recordingMemoryStore) Root(ref knowledge.SpaceRef) (string, error) {
	return "mem://" + ref.ID(), nil
}

func (s *recordingMemoryStore) BuildIndex(context.Context, knowledge.SpaceRef) ([]knowledge.IndexEntry, error) {
	return nil, nil
}

func (s *recordingMemoryStore) Autoload(context.Context, knowledge.SpaceRef, []string) ([]knowledge.AutoloadEntry, error) {
	return nil, nil
}

func (s *recordingMemoryStore) ListDocuments(context.Context, knowledge.SpaceRef, string) ([]knowledge.DocumentMeta, error) {
	return nil, nil
}

func (s *recordingMemoryStore) ReadDocument(context.Context, knowledge.SpaceRef, string) (knowledge.Document, error) {
	return knowledge.Document{}, knowledge.ErrDocNotFound
}

func (s *recordingMemoryStore) WriteDocument(_ context.Context, _ knowledge.SpaceRef, in knowledge.DocumentInput) (knowledge.DocumentMeta, error) {
	s.writes = append(s.writes, in)
	return knowledge.DocumentMeta{Path: in.Path, Size: int64(len(in.Content))}, nil
}

func (s *recordingMemoryStore) DeleteDocument(_ context.Context, _ knowledge.SpaceRef, path string) error {
	s.deletes = append(s.deletes, path)
	return nil
}

func (s *recordingMemoryStore) UsageBytes(context.Context, knowledge.SpaceRef) (int64, int64, error) {
	return 0, 0, nil
}

func toolNames(opts *GenerationOptions) map[string]bool {
	names := map[string]bool{}
	for _, t := range opts.Tools {
		names[t.Name] = true
	}
	return names
}

func systemText(opts *GenerationOptions) string {
	var b strings.Builder
	for _, blk := range opts.SystemBlocks {
		b.WriteString(blk.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestInstallWorkspaceMemory_GlobalWriteNeedsPlatformAuthority is the
// ticket's own reproduction (#2400): the instance-wide `global` space had a
// super-admin write gate on the REST surface ONLY, so a run of ANY team wrote
// it — installWorkspaceMemory appended memory_write on spec.Write alone.
//
// The two arms isolate the AUTHORITY from the capability: the same ref, the
// same spec, the same store — only the context's grant differs. Without that
// second arm a gate that simply removed the tool would pass, and the
// capability would be gone instead of gated (D0: keep it, gate it).
func TestInstallWorkspaceMemory_GlobalWriteNeedsPlatformAuthority(t *testing.T) {
	globalRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "shared"}
	spec := &delegate.MemorySpec{Scope: "shared", Visibility: "global", Read: true, Write: true}

	t.Run("a run carries no authority: no memory_write", func(t *testing.T) {
		store := &recordingMemoryStore{}
		opts := &GenerationOptions{}
		if err := installWorkspaceMemory(context.Background(), opts, store, globalRef, spec); err != nil {
			t.Fatalf("install: %v", err)
		}
		names := toolNames(opts)
		if names[MemoryWriteToolName] {
			t.Errorf("a run of any team was handed %q on the instance-wide global space: tools=%v", MemoryWriteToolName, names)
		}
		// The read half is NOT withdrawn: global is read-only to orgs by design.
		if !names[MemoryReadToolName] || !names[MemoryListToolName] {
			t.Errorf("the read tools must survive the write gate: tools=%v", names)
		}
		// Explicit, not silent: the refusal reaches the transcript.
		if txt := systemText(opts); !strings.Contains(txt, "workspace_memory_write_refused") {
			t.Errorf("the withheld tool left no trace in the system blocks: %q", txt)
		}
	})

	t.Run("a platform-authorized context keeps the capability", func(t *testing.T) {
		store := &recordingMemoryStore{}
		opts := &GenerationOptions{}
		ctx := knowledge.WithPlatformWriteAuthority(context.Background())
		if err := installWorkspaceMemory(ctx, opts, store, globalRef, spec); err != nil {
			t.Fatalf("install: %v", err)
		}
		if names := toolNames(opts); !names[MemoryWriteToolName] {
			t.Errorf("the gate removed the capability instead of gating it: tools=%v", names)
		}
		if txt := systemText(opts); strings.Contains(txt, "workspace_memory_write_refused") {
			t.Errorf("an authorized install must not announce a refusal: %q", txt)
		}
	})

	t.Run("a team-scoped space is untouched", func(t *testing.T) {
		store := &recordingMemoryStore{}
		opts := &GenerationOptions{}
		orgRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "t1", Name: "conventions"}
		if err := installWorkspaceMemory(context.Background(), opts, store, orgRef, spec); err != nil {
			t.Fatalf("install: %v", err)
		}
		if names := toolNames(opts); !names[MemoryWriteToolName] {
			t.Errorf("a team-scoped write must stay ungated: tools=%v", names)
		}
	})
}

// TestMemoryWriteTool_GlobalRefIsRefusedAtTheStore proves the gate does not
// depend on the install site alone. If a future caller wires memoryWriteTool
// directly (or the install gate is removed), the STORE still refuses: the
// authority is checked where every writer passes, so the hole cannot reopen
// one site at a time.
func TestMemoryWriteTool_GlobalRefIsRefusedAtTheStore(t *testing.T) {
	store, _, _ := memTest(t, "ignored")
	globalRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "shared"}
	tool := memoryWriteTool(store, globalRef, "bot:x")
	_, err := tool.Execute(context.Background(), []byte(`{"path":"a.md","content":"# pollution"}`))
	if !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("memory_write on a global ref from an ungranted context: err=%v, want knowledge.ErrGlobalWriteForbidden", err)
	}
}
