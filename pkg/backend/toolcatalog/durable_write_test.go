package toolcatalog

import (
	"slices"
	"testing"
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
