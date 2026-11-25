package delegate

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// relBelow's retry is what lets the walk happen for a workspace spelled through
// a symlink; without it the helper always reports "not below" and the guard
// becomes a permanent no-op — invisible from the outside, which is why round 3
// could delete it with a green suite.
func TestRelBelowResolvesTheBase(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "realrepo")
	if err := os.MkdirAll(filepath.Join(real, ".iterion"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	walkBase, rel := relBelow(link, filepath.Join(real, ".iterion", "pi"))
	if rel != filepath.Join(".iterion", "pi") || walkBase != real {
		t.Errorf("relBelow = (%q, %q), want the resolved base and a walkable rel", walkBase, rel)
	}
	// A genuinely unrelated target is "not below" — a no-op, never an error.
	if _, r := relBelow(t.TempDir(), t.TempDir()); r != "" {
		t.Errorf("rel = %q for an unrelated target, want none", r)
	}
	// A first component merely STARTING with ".." is below, not an escape.
	if _, r := relBelow(base, filepath.Join(base, "..cache", "pi")); r == "" {
		t.Error(`"..cache" was treated as an escape`)
	}
}

// A component we cannot inspect is an uncertainty, and uncertainties refuse.
func TestRefuseSymlinkedPathFailsClosedOnUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the permission bits this asserts")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot drop permissions: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := refuseSymlinkedPath(base, filepath.Join(locked, "inner", "pi"), "pi backend", "the pi credential"); err == nil {
		t.Error("an uninspectable component was accepted")
	}
}

// The three state-root guards share one contract, so all three must name the
// caller. refuseSymlinkedPath kept pi's wording after the extraction, so an
// auto-memory run refused by a repo-planted symlink told the operator to go
// look at "the pi credential" — and the memoised guard repeated it on every
// node of the run.
func TestPrepareStateRoot_ErrorsNameTheirCaller(t *testing.T) {
	tmpRoot := filepath.Clean(t.TempDir())
	work := filepath.Join(tmpRoot, "pipe")
	// A symlinked ANCESTOR: the component walk, not the leaf check.
	if err := os.MkdirAll(filepath.Join(work, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "real"), filepath.Join(work, ".iterion")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root := filepath.Join(work, ".iterion", "auto-memory")

	err := PrepareStateRoot(Task{WorkDir: work}, root, StateInCheckout,
		"auto-memory", "the agent's MEMORY.md notes", nil)
	if err == nil {
		t.Fatal("a symlinked ancestor inside the checkout must be refused")
	}
	piWord := regexp.MustCompile(`\bpi\b`)
	message := strings.ReplaceAll(err.Error(), tmpRoot, "<tmp>")
	if piWord.MatchString(message) {
		t.Errorf("an auto-memory refusal points the operator at pi: %v", err)
	}
	if !strings.Contains(err.Error(), "MEMORY.md") {
		t.Errorf("the refusal does not say what would have been written: %v", err)
	}
	piErr := PrepareStateRoot(Task{WorkDir: work}, root, StateInCheckout,
		"pi backend", "the pi credential", nil)
	if piErr == nil {
		t.Fatal("a pi refusal must be produced")
	}
	piMessage := strings.ReplaceAll(piErr.Error(), tmpRoot, "<tmp>")
	if !piWord.MatchString(piMessage) {
		t.Fatalf("the word guard must match a pi refusal: %v", piErr)
	}
}

// A repo can ship `<WorkDir>/.iterion/.gitignore` as a tracked symlink. A
// dangling link must not let the guard write create an attacker-chosen host
// file, and a link to an existing file must not pass for the guard: the state
// dir would stay stageable, so the miss is logged. Every StateInCheckout
// caller of PrepareStateRoot (claw auto-memory included) reaches this through
// hideWorkspaceStateDir.
func TestHideWorkspaceStateDirRefusesASymlinkedGuard(t *testing.T) {
	t.Run("dangling: nothing is written through it", func(t *testing.T) {
		work := t.TempDir()
		target := filepath.Join(t.TempDir(), "absent")
		if err := os.MkdirAll(filepath.Join(work, ".iterion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, ".iterion", ".gitignore")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		hideWorkspaceStateDir(work, nil, "pi", "session files")
		if _, err := os.Stat(target); err == nil {
			t.Error("created a host file at the symlink's target")
		}
	})

	t.Run("pointing at an existing file: the miss is not silent", func(t *testing.T) {
		work := t.TempDir()
		other := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(other, []byte("unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(work, ".iterion"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, filepath.Join(work, ".iterion", ".gitignore")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		var buf bytes.Buffer
		hideWorkspaceStateDir(work, iterlog.New(iterlog.LevelWarn, &buf), "pi", "session files")
		if !strings.Contains(buf.String(), "not a regular file") {
			t.Errorf("log = %q — the workspace is unguarded and nothing says so", buf.String())
		}
		if got, _ := os.ReadFile(other); string(got) != "unrelated\n" {
			t.Errorf("wrote through the symlink: %q", got)
		}
	})
}
