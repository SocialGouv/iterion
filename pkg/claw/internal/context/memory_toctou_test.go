package context

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// startFlipping swaps a symlink between two targets until stop is closed.
func startFlipping(t *testing.T, link, a, b string, stop <-chan struct{}, done chan<- struct{}) {
	t.Helper()
	go func() {
		defer close(done)
		target := a
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.Remove(link)
			os.Symlink(target, link)
			if target == a {
				target = b
			} else {
				target = a
			}
			runtime.Gosched()
		}
	}()
}

// A confined read must never hand back content from outside dir, however the
// links flip while it runs. With readWithin in place the guarantee is
// unconditional — a flip before the check, or between the open and its
// identity comparison, is dropped; without the guard, the flipper lands
// inside the check-to-read window within a few hundred rounds and the test
// goes red.
func TestReadWithinSurvivesALinkFlip(t *testing.T) {
	dir := t.TempDir()
	// The outside file lives in a second temp directory: it must be outside
	// the boundary by construction, not just by name.
	inside := filepath.Join(dir, "inside.md")
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFile(t, inside, "INSIDE-CONTENT")
	writeFile(t, outside, "OUTSIDE-CONTENT")
	link := filepath.Join(dir, "link.md")

	stop := make(chan struct{})
	done := make(chan struct{})
	startFlipping(t, link, inside, outside, stop, done)
	defer func() { close(stop); <-done }()

	for i := 0; i < 3000; i++ {
		data, ok := readWithin(link, dir)
		if ok && strings.Contains(string(data), "OUTSIDE-CONTENT") {
			t.Fatal("readWithin returned content from outside the boundary during a link flip")
		}
	}
}

// The same guarantee through the loader: a workspace CLAUDE.md that flips
// between a file inside the root and one outside must never inject the
// outside content, whichever round catches the swap.
func TestLoadMemoryWorkspaceSymlinkFlipNeverLeaks(t *testing.T) {
	l := newImportLayout(t)
	wdClaude := filepath.Join(l.workDir, "CLAUDE.md")
	inside := filepath.Join(l.workDir, "shared", "in.md")
	outside := filepath.Join(l.outside, "wd-claude.md")

	stop := make(chan struct{})
	done := make(chan struct{})
	startFlipping(t, wdClaude, inside, outside, stop, done)
	defer func() { close(stop); <-done }()

	opts := MemoryOptions{WalkUp: true, SkipUser: true, SkipOuter: true, ClaudeCodeLayout: true, Root: l.root}
	for i := 0; i < 300; i++ {
		got, _ := LoadMemory(l.workDir, opts)
		if strings.Contains(got, "WORKDIR-CLAUDE-THROUGH-A-LINK") {
			t.Fatal("outside content leaked through a workspace symlink flip")
		}
	}
}
