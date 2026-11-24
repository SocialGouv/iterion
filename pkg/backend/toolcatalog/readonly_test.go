package toolcatalog

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// TestReadonlyWithholdsDurableWritePerBackend pins the flag's per-backend
// leverage against the delegates' own enforcement, concept by concept —
// each row is the lever the delegate documents at its enforcement point,
// and a backend that learns a lever reddens its row rather than every
// reader of the question.
func TestReadonlyWithholdsDurableWritePerBackend(t *testing.T) {
	t.Parallel()

	// The whole durable-write vocabulary is asked, so a concept added to
	// DurableWriteCanonicals is answered here the day it lands.
	writers := DurableWriteCanonicals()
	if len(writers) == 0 {
		t.Fatal("DurableWriteCanonicals is empty — every row below would pass over a no-op")
	}

	t.Run("codex and pi withhold every durable writer", func(t *testing.T) {
		t.Parallel()
		// codex: a read-only OS sandbox; pi: a tool list pinned to four
		// readers. Neither leaves the shell.
		for _, backend := range []string{backendcaps.Codex, backendcaps.Pi} {
			for _, canonical := range writers {
				if !ReadonlyWithholdsDurableWrite(backend, canonical) {
					t.Errorf("%s: readonly leaves %q — but this backend's flag is a sandbox or a readers-only list, which no durable write survives", backend, canonical)
				}
			}
		}
	})

	t.Run("claude_code withholds the file writers and keeps the shell", func(t *testing.T) {
		t.Parallel()
		for _, canonical := range writers {
			got := ReadonlyWithholdsDurableWrite(backendcaps.ClaudeCode, canonical)
			want := canonical != "bash" && canonical != "memory_write"
			if got != want {
				t.Errorf("claude_code readonly, %q: withholds=%v — the flag removes the file-writing natives and keeps the shell (it is not a sandbox), and memory_write is on no claude_code roster at all", canonical, got)
			}
		}
		// The shell clause is the one a reader gets wrong: pinned against the
		// delegate's own contract (claudeReadonlyResidualWritePaths).
		if ReadonlyWithholdsDurableWrite(backendcaps.ClaudeCode, "bash") {
			t.Error("claude_code readonly was read as withholding the shell — the flag keeps it, which is exactly why a readonly claude_code node still holds a durable write path")
		}
	})

	t.Run("the backends that ignore the flag withhold nothing", func(t *testing.T) {
		t.Parallel()
		for _, backend := range []string{backendcaps.Claw, backendcaps.Kimi, backendcaps.Grok, backendcaps.OpenCode} {
			for _, canonical := range writers {
				if ReadonlyWithholdsDurableWrite(backend, canonical) {
					t.Errorf("%s: readonly withholds %q — no lever of this backend reads the flag, so the answer must stay false until one does", backend, canonical)
				}
			}
		}
	})

	t.Run("an unregistered backend withholds nothing", func(t *testing.T) {
		t.Parallel()
		if ReadonlyWithholdsDurableWrite("no-such-backend", "write") {
			t.Error("an unregistered backend was read as withholding — the flag's meaning is declared per backend, never guessed")
		}
	})
}

// TestDeclaredListBoundsPerBackend pins the tools-list parameter of the
// durable-write question: the list decides the surface only where it both
// REACHES the backend and means a bound there.
func TestDeclaredListBoundsPerBackend(t *testing.T) {
	t.Parallel()
	declared := []string{"read_file"}

	// The CLI-agent seam never receives the list: a declared bound is
	// silently dropped there, so nothing the node declares bounds the run.
	for _, backend := range []string{backendcaps.Pi, backendcaps.Kimi, backendcaps.Grok, backendcaps.OpenCode} {
		if DeclaredListBounds(backend, declared) {
			t.Errorf("%s never receives the list (ReceivesToolList), but a declared list was read as bounding its surface", backend)
		}
	}

	// On claw the list IS the surface, declared or not.
	if !DeclaredListBounds(backendcaps.Claw, declared) {
		t.Error("claw: a declared list does not decide the surface? ToolDefs are resolved from it")
	}
	if !DeclaredListBounds(backendcaps.Claw, nil) {
		t.Error("claw: an UNDECLARED list is no tool at all there (the list is the whole surface) — reading it as no restriction is iterion#2832's headline defect")
	}

	// The receiving CLI backends are bounded by a DECLARED list only.
	for _, backend := range []string{backendcaps.ClaudeCode, backendcaps.Codex} {
		if !DeclaredListBounds(backend, declared) {
			t.Errorf("%s receives the list: a declared one must bound the surface", backend)
		}
		if DeclaredListBounds(backend, nil) {
			t.Errorf("%s: an undeclared list is no restriction there — reading it as one is the hole #2832 closed", backend)
		}
		if !DeclaredListBounds(backend, []string{}) {
			t.Errorf("%s: `tools: []` IS a declaration (ToolsDeclared) and must bound the surface", backend)
		}
	}
}
