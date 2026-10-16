package botinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A removal name resolves one directory under .botz/ — nothing more. Bundles
// installed by an earlier version (whose validator held names to path safety
// only) carry names outside the install-time charset — "my bot", " my bot",
// "café", ".hidden" (a manifest name is never charset-checked, and its
// whitespace edges are part of the name) — and must stay removable by this
// build, or an upgraded workspace is stranded with a bundle its own tool
// cannot uninstall.
func TestRemove_TakesAnyPathSafeNameAnEarlierVersionInstalled(t *testing.T) {
	for _, name := range []string{"my bot", " my bot", "café", ".hidden"} {
		t.Run(name, func(t *testing.T) {
			workdir := t.TempDir()
			target := filepath.Join(workdir, ".botz", name)
			writeBundle(t, target, name, 0)
			if _, err := Remove(context.Background(), Options{Name: name, Workdir: workdir}); err != nil {
				t.Fatalf("Remove(%q) = %v, want the pre-gate install removed", name, err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("Remove(%q) left the bundle in place (stat: %v)", name, err)
			}
		})
	}
}

// Resolution is by the exact installed name: a padded variant of the name
// names a different directory. It must fall on the not-installed refusal —
// resolving the TRIMMED name here would delete .botz/"my bot", a bundle the
// caller did not name.
func TestRemove_ANamePaddedBeyondTheInstalledNameNamesADifferentDirectory(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, ".botz", "my bot")
	writeBundle(t, target, "my bot", 0)
	if _, err := Remove(context.Background(), Options{Name: "my bot\n", Workdir: workdir}); err == nil {
		t.Fatal("Remove(\"my bot\\n\") = nil error, want the not-installed refusal")
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Fatalf("Remove(\"my bot\\n\") touched %q — the caller did not name that bundle (stat: %v)", target, serr)
	}
}

// The removal gate still holds the one bound the filesystem needs: the name
// resolves to a single directory under .botz/, never outside it. The live
// directory outside .botz makes the containment real — a gate that let
// "../escape" through would delete <workdir>/escape, and this witness would
// redden on the deleted directory and on the gate's own refusal never
// arriving (a "not installed" stat error is not a containment refusal).
func TestRemove_StillRefusesANameThatEscapesTheBotzDirectory(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "../escape"} {
		t.Run(name, func(t *testing.T) {
			workdir := t.TempDir()
			outside := filepath.Join(workdir, "escape")
			if err := os.MkdirAll(filepath.Join(workdir, ".botz"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := Remove(context.Background(), Options{Name: name, Workdir: workdir})
			if err == nil {
				t.Fatalf("Remove(%q) = nil error, want refused", name)
			}
			if _, serr := os.Stat(outside); serr != nil {
				t.Fatalf("Remove(%q) touched %s outside .botz (stat: %v)", name, outside, serr)
			}
			if strings.ContainsAny(name, `/\`) && !strings.Contains(err.Error(), "no path separators") {
				t.Fatalf("Remove(%q) error = %v, want the gate's own separator refusal", name, err)
			}
		})
	}
}

// The remove gate speaks in remove nouns: a failed uninstall reports a
// "remove name" problem, not an install one — the operator reads the message
// on an uninstall, on the CLI's stderr or in the server's 400 body.
func TestRemove_TheGateSpeaksInRemoveNouns(t *testing.T) {
	for _, name := range []string{"", "   ", "a/b"} {
		_, err := Remove(context.Background(), Options{Name: name, Workdir: t.TempDir()})
		if err == nil {
			t.Fatalf("Remove(%q) = nil error, want refused", name)
		}
		if !strings.Contains(err.Error(), "remove name") {
			t.Fatalf("Remove(%q) error = %v, want remove-flavored wording", name, err)
		}
	}
}

// A name no gate can vouch for reaches no raw output here: every message
// carrying the name quotes it, so a newline in a store entry's name (the
// store keeps the raw manifest name) cannot write a line of a caller's
// output.
func TestRemove_ANameCarryingANewlineStaysQuotedInTheError(t *testing.T) {
	name := "evil\n  ! catalog not refreshed: nothing, all good"
	_, err := Remove(context.Background(), Options{Name: name, Workdir: t.TempDir()})
	if err == nil {
		t.Fatal("Remove of a never-installed newline-carrying name = an error, want refused")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("Remove's error carries a raw newline — it would write a line of the caller's output: %q", err.Error())
	}
}
