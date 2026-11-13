//go:build unix

package context

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A rules directory belongs to the repository, and a FIFO named x.md is a
// perfectly good thing for a repository to contain: opening it for reading
// blocks until a writer shows up, forever. Only regular files are rules.
func TestRulesIgnoreNonRegularFiles(t *testing.T) {
	l := newMemoryLayout(t)
	rules := filepath.Join(l.workDir, ".claude", "rules")
	writeFile(t, filepath.Join(rules, "real.md"), "REAL-RULE")
	pipe := filepath.Join(rules, "pipe.md")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	if got := relPaths(discoverRules(rules, "")); strings.Join(got, "|") != "real.md" {
		t.Errorf("rules = %q, want only the regular file", got)
	}

	done := make(chan string, 1)
	go func() {
		got, _ := LoadMemory(l.workDir, MemoryOptions{ClaudeCodeLayout: true, SkipUser: true})
		done <- got
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "REAL-RULE") {
			t.Errorf("the regular rule was not loaded:\n%s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadMemory blocked reading a FIFO named like a rule")
	}
}
