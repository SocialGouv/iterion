package toolcatalog

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// TestTheDurableWriteSetIsTheWritingSetPlusTheShellAndClawsOwnTool holds the
// derivation rather than the list: the set exists to answer a DIFFERENT
// question from WritingCanonicals', and the only honest way to keep the two
// agreeing about the words they share is to build one from the other.
//
// A literal list here would pass while the two drifted — which is the defect
// WritingCanonicals' own godoc records having been paid twice.
func TestTheDurableWriteSetIsTheWritingSetPlusTheShellAndClawsOwnTool(t *testing.T) {
	t.Parallel()
	got := DurableWriteCanonicals()
	for _, want := range WritingCanonicals() {
		if !slices.Contains(got, want) {
			t.Errorf("DurableWriteCanonicals() omits %q, a member of WritingCanonicals(): a tool that writes a file writes one in the memory tree too", want)
		}
	}
	// The two additions, each the reason this set is asked separately.
	for _, want := range []string{"bash", "memory_write"} {
		if !slices.Contains(got, want) {
			t.Errorf("DurableWriteCanonicals() omits %q — see the godoc: the shell writes any file the process reaches, and memory_write is claw's own durable-write builtin", want)
		}
	}
	// The shell's absence from the narrower set is a CONTRACT (`readonly:`
	// keeps it on claude_code, #1651). Asserted here because the whole reason
	// this set exists is that the two answers about `bash` differ: if the
	// narrower one ever gains the shell, this file's reason is gone and the
	// caller should read WritingCanonicals directly.
	if slices.Contains(WritingCanonicals(), "bash") {
		t.Error("WritingCanonicals() now carries the shell — the two readings have converged, so DurableWriteCanonicals' shell clause is dead and its callers should read the narrower set")
	}
	// Every member folds onto itself, or a caller that canonicalises its
	// input before testing membership can never match.
	for _, m := range got {
		if c := CanonicalToolName(m); c != m {
			t.Errorf("member %q folds onto %q: the set must hold canonical concepts, since callers canonicalise a declaration before testing it", m, c)
		}
	}
}

// TestTheDurableWriteSetCannotBeGrownByACaller is the "copied, so a consumer
// cannot grow the set for everyone else" half of the contract — the same
// promise WritingCanonicals makes, and it has to hold for the derived set or
// one caller's append changes every other caller's verdict.
func TestTheDurableWriteSetCannotBeGrownByACaller(t *testing.T) {
	t.Parallel()
	first := DurableWriteCanonicals()
	//nolint:staticcheck // SA4010: the append IS the mutation under test — a
	// caller growing its own copy must not reach the package's.
	_ = append(first, "definitely_not_a_tool")
	first[0] = "clobbered"
	for _, m := range DurableWriteCanonicals() {
		if m == "definitely_not_a_tool" || m == "clobbered" {
			t.Fatalf("a caller's append or write reached the package's set (saw %q): the function must return a copy", m)
		}
	}
}

// TestDurableWriteSpellingsOnAnswersInNamesABackendCanSend holds the three
// claims of that function, each of which a reader of its answer spends on the
// permission gate — and a gate matches a CALL, by the name the backend's model
// emits. A reader handed a concept KEY asked about a call no backend makes, and
// read `deny: ["Write*"]` as leaving a writer it removes (iterion#2832).
//
//   - where the registry declares a roster, the answer is drawn from it: every
//     name is on that roster, and every durable writer of that roster is there;
//   - where it declares none, the answer is drawn from the SPELLING table, so
//     each name folds onto a durable-write concept and none is a key the table
//     does not carry;
//   - the store's own builtin is in NEITHER: `memory_write` has no spelling row
//     and is on no roster, which is what makes it the `memory:` block's.
func TestDurableWriteSpellingsOnAnswersInNamesABackendCanSend(t *testing.T) {
	t.Parallel()

	writers := DurableWriteCanonicals()
	store := []string{}
	rows := CanonicalRows()
	for _, canonical := range writers {
		if _, projected := rows[canonical]; !projected {
			store = append(store, canonical)
		}
	}
	if len(store) == 0 {
		t.Fatal("every durable-write concept has a spelling row now — the store-builtin claim below is asserted over nothing")
	}

	declared, undeclared := 0, 0
	for _, backend := range backendcaps.Names() {
		got := DurableWriteSpellingsOn(backend)
		if len(got) == 0 {
			t.Errorf("%s: no durable writer at all — a node whose list bounds nothing there would be read as unable to write", backend)
			continue
		}
		for _, name := range got {
			if canonical := CanonicalToolName(name); !slices.Contains(writers, canonical) {
				t.Errorf("%s: %q folds onto %q, which is not a durable-write concept", backend, name, canonical)
			}
			if slices.Contains(store, CanonicalToolName(name)) {
				t.Errorf("%s: %q is the durable store's OWN builtin, which a `tools:` list cannot install (C135) and no roster carries", backend, name)
			}
		}
		roster := backendcaps.NativeTools(backend)
		if len(roster) == 0 {
			undeclared++
			// No roster: every name must be a ROW of the spelling table (which
			// is keyed by spelling), never a bare concept key the table does
			// not carry — the shape that asked the gate about a call nothing
			// sends.
			for _, name := range got {
				if _, carried := rows[name]; !carried {
					t.Errorf("%s: %q is no row of CanonicalRows — it is a concept key the spelling table does not carry, and the gate would be asked about a call nothing sends",
						backend, name)
				}
			}
			continue
		}
		declared++
		for _, name := range got {
			if !slices.Contains(roster, name) {
				t.Errorf("%s: %q is not on the roster the registry declares for it: %v", backend, name, roster)
			}
		}
		for _, native := range roster {
			if slices.Contains(writers, CanonicalToolName(native)) && !slices.Contains(got, native) {
				t.Errorf("%s: the roster carries the durable writer %q and the answer omits it", backend, native)
			}
		}
	}
	if declared == 0 || undeclared == 0 {
		t.Fatalf("the registry exercises one branch only (%d backends with a roster, %d without) — the other claim above is asserted over nothing", declared, undeclared)
	}
}
