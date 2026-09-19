package treenoise

import (
	"reflect"
	"strings"
	"testing"
)

// The consumers read this list through THREE shapes that must never drift
// apart: git pathspecs, the shell rendering a bot's prompt carries, and the
// env value a tool script splits. The entries themselves must stay shell-
// safe and space-free, or the env split and the shell rendering both lie.

func TestPathspecsCoverEveryEntry(t *testing.T) {
	want := []string{
		":(exclude,top).claude",
		":(exclude,top)devbox.lock",
	}
	if got := Pathspecs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Pathspecs() = %q, want %q", got, want)
	}
}

func TestShellPathspecsRendersEveryEntryQuoted(t *testing.T) {
	want := "':(exclude,top).claude' ':(exclude,top)devbox.lock'"
	if got := ShellPathspecs(); got != want {
		t.Fatalf("ShellPathspecs() = %q, want %q", got, want)
	}
}

// The load-bearing env tests (plan review F8): the entries are shell-safe
// and space-free BY CONSTRUCTION, and the env value splits back into
// exactly the pathspecs — a lossless round trip, not an equality that is
// true because it was defined as one.
func TestEntriesAreShellSafeAndSpaceFree(t *testing.T) {
	for _, e := range Entries {
		if e.Path == "" {
			t.Errorf("entry with empty path: %+v", e)
		}
		for _, bad := range []string{" ", "'", `"`, "\t", "\n", "$", "`", ";", "&", "|", "(", ")"} {
			if strings.Contains(e.Path, bad) {
				t.Errorf("entry path %q contains %q — the env contract (space-separated, shell-rendered) breaks", e.Path, bad)
			}
		}
	}
}

func TestEnvValueSplitsBackIntoPathspecs(t *testing.T) {
	got := strings.Fields(EnvValue())
	if !reflect.DeepEqual(got, Pathspecs()) {
		t.Fatalf("Fields(EnvValue()) = %q, want Pathspecs() %q", got, Pathspecs())
	}
}

// F6 (plan review): git's `:(exclude,top).claude` hides a top-level FILE
// named .claude just as well as the directory — the predicate must agree
// with the pathspec on every path, or `workdirIsClean` (the predicate's
// consumer) calls a worktree dirty whose `add` stages nothing, and finalize
// dies on an empty commit.
func TestIsNoiseAgreesWithThePathspecSemantics(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".claude", true},               // a top-level FILE named .claude
		{".claude/", true},              // the directory, trailing slash
		{".claude/skills/x.md", true},   // the mirror's contents
		{".claude/settings.json", true}, // settings rewritten by plugin hooks
		{".claudeish", false},           // a sibling that merely starts alike
		{".claudeish/inner", false},     // …and its contents
		{"devbox.lock", true},           // the lock, top-level exact
		{"sub/dir/devbox.lock", false},  // NOT the lock: a different file the bot owns
		{"README.md", false},            // real work
		{"docs/adr/0001-x.md", false},   // real work, nested
	}
	for _, tc := range cases {
		if got := IsNoise(tc.path); got != tc.want {
			t.Errorf("IsNoise(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The list stays honest: every entry says why it is noise, so the next
// reader can tell setup from work.
func TestEveryEntryCarriesItsReason(t *testing.T) {
	for _, e := range Entries {
		if e.Why == "" {
			t.Errorf("entry %q carries no reason", e.Path)
		}
		if !strings.HasPrefix(e.Path, ".") && e.Path != "devbox.lock" {
			t.Errorf("entry %q is neither a dot-directory nor the lock — adding a member means re-reading every consumer's semantics", e.Path)
		}
	}
}
