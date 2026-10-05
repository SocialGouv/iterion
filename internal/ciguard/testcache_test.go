package ciguard

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var goTestCommand = regexp.MustCompile(`\bgo test\b`)

// TestEveryGoTestInTheWorkflowSkipsTheTestCache holds ADR-105's first
// decision: CI never replays a test result from a cache. Go replays a cached
// result when the test binary and the files the test itself opened are
// unchanged — a file a child process reads is not tracked, and iterion's e2e
// tests build ./cmd/iterion in a child `go build`, so with a restored
// GOCACHE a change confined to cmd/iterion would replay `ok (cached)`.
// -count=1 turns the test cache off for the run, whatever GOCACHE the job
// restored.
func TestEveryGoTestInTheWorkflowSkipsTheTestCache(t *testing.T) {
	src, err := os.ReadFile(workflowPaths[0])
	if err != nil {
		t.Fatalf("read %s: %v", workflowPaths[0], err)
	}
	var wf workflowSteps
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	found := 0
	for job, j := range wf.Jobs {
		for _, st := range j.Steps {
			for _, cmd := range shellCommands(st.Run) {
				if !goTestCommand.MatchString(cmd) {
					continue
				}
				found++
				if !strings.Contains(cmd, "-count=1") {
					t.Errorf("job %q, step %q runs `go test` without -count=1 — a result cached by another run would be replayed as a pass:\n  %s", job, st.Name, cmd)
				}
			}
		}
	}
	if found < 5 {
		t.Fatalf("found %d `go test` commands in %s — the workflow no longer has the shape this guard reads", found, workflowPaths[0])
	}
}

// shellCommands splits a `run:` script into logical commands: comment lines
// dropped, a line ending in a backslash joined with the next.
func shellCommands(script string) []string {
	var cmds []string
	var cur strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		if s := strings.TrimSpace(cur.String()); s != "" {
			cmds = append(cmds, s)
		}
		cur.Reset()
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		cmds = append(cmds, s)
	}
	return cmds
}
