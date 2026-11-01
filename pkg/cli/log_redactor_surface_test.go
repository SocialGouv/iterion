package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The CLI's per-run logger is the run.log tee, and that handle is the one
// buildRunExecutor hands runview.BuildExecutor: a line `iterion run` logs
// on it afterwards — `executor close: %v`, the MCP health-check's quoted
// stderr, the sandbox pre-flight's detail — reaches run.log scrubbed
// (iterion#2822).
func TestTheCLIsOwnLineOnTheRunLogCrossesTheRunsMatcher(t *testing.T) {
	const value = "hunter2-9f8e7d6c5b4a"
	const runID = "run-cli-redactor"
	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	res := ir.Compile(parsertest.Parse("main.bot", `vars:
  planted: string = ""

secrets:
  db: "{{vars.planted}}"

workflow main:
  entry: done
`).File)
	if res.Workflow == nil {
		t.Fatalf("compile: %v", res.Diagnostics)
	}

	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)
	// The production shape: the tee replaces the handle, and the run's
	// matcher is installed on whatever teeRunLog returned.
	logger, closer := teeRunLog(logger, iterlog.LevelInfo, storeDir, runID)
	if closer == nil {
		t.Fatal("no run.log tee: this test has nothing to assert")
	}
	defer closer.Close()

	if _, err := buildRunExecutor(
		RunOptions{Vars: map[string]string{"planted": value}}, tiersMatchTheEngine, res.Workflow, st,
		lineage{runID: runID}, storeDir, logger, "", nil, "main", nil,
	); err != nil {
		t.Fatalf("buildRunExecutor: %v", err)
	}

	logger.Warn("MCP health-check failed for server %q: start: env TOKEN=%s rejected", "github", value)

	b, err := os.ReadFile(filepath.Join(storeDir, "runs", runID, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "MCP health-check failed") {
		t.Fatalf("the surface's own line never reached run.log: %q", got)
	}
	if strings.Contains(got, value) {
		t.Errorf("the CLI's own line carries the declared value raw in run.log: %q", got)
	}
	if !strings.Contains(got, secretguard.PlaceholderForName("db")) {
		t.Errorf("run.log shows no placeholder where the value was: %q", got)
	}
}
