package e2e

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion report` is the durable, human-readable record of a run — the
// artifact an operator keeps once `.iterion/runs/<id>/` is gone. Its
// contract: read the run's own events + artifacts and render them
// chronologically, with the outcome, at the documented store path.
// Only the `verify:` header special case was tested.
//
// Mutation check: stop reading events and the timeline loses its nodes
// (and their order); stop reading artifacts and the artifact table
// empties; write somewhere else and the documented path assertion fails;
// mis-render the outcome and the status assertion fails.

// runReportFixture executes report_mini.bot to completion and returns
// the store dir it landed in.
func runReportFixture(t *testing.T, runID string) string {
	t.Helper()
	storeDir := t.TempDir()

	exec := newScenarioExecutor()
	exec.on("survey", func(map[string]any) (map[string]any, error) {
		return map[string]any{"value": "notes", "summary": "surveyed-the-tree", "_tokens": 120, "_cost_usd": 0.002}, nil
	})
	exec.on("build", func(map[string]any) (map[string]any, error) {
		return map[string]any{"value": "built", "summary": "build-succeeded"}, nil
	})
	exec.on("verify", func(map[string]any) (map[string]any, error) {
		return map[string]any{"value": "green", "summary": "all-green", "ok": true, "_tokens": 30, "_cost_usd": 0.001}, nil
	})

	if err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "report_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      exec,
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON}); err != nil {
		t.Fatalf("run report_mini: %v", err)
	}
	return storeDir
}

func TestReportRendersChronologicalRunReport(t *testing.T) {
	runID := "e2e-report-basic"
	storeDir := runReportFixture(t, runID)

	// --- default destination: <store>/runs/<id>/report.md -------------
	var out bytes.Buffer
	if err := cli.RunReport(cli.ReportOptions{RunID: runID, StoreDir: storeDir}, humanPrinter(&out)); err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	reportPath := filepath.Join(storeDir, "runs", runID, "report.md")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("report not written to the documented path %s: %v", reportPath, err)
	}
	md := string(data)

	// Identity + outcome.
	for _, want := range []string{runID, "report_mini", string(store.RunStatusFinished), "Run Finished"} {
		if !strings.Contains(md, want) {
			t.Errorf("report does not mention %q\n---\n%s", want, md)
		}
	}

	// Every executed node appears in the timeline, in execution order.
	// Scoped to the Timeline section: the Artifacts table above it is
	// sorted by node id and would confound a whole-document scan.
	tl := md[strings.Index(md, "## Timeline"):]
	iSurvey := strings.Index(tl, "survey")
	iBuild := strings.Index(tl, "build")
	iVerify := strings.Index(tl, "verify")
	if iSurvey < 0 || iBuild < 0 || iVerify < 0 {
		t.Fatalf("timeline is missing one of the executed nodes (survey=%d build=%d verify=%d)\n---\n%s", iSurvey, iBuild, iVerify, md)
	}
	if !(iSurvey < iBuild && iBuild < iVerify) {
		t.Errorf("nodes are not in chronological order (survey=%d build=%d verify=%d)", iSurvey, iBuild, iVerify)
	}

	// The published artifacts' own summaries are carried over, not just
	// their node names — the report is read when the store is gone.
	if !strings.Contains(md, "surveyed-the-tree") {
		t.Errorf("report does not carry the survey artifact's summary\n---\n%s", md)
	}
	if !strings.Contains(md, "all-green") {
		t.Errorf("report does not carry the verdict artifact's summary\n---\n%s", md)
	}
	// `build` publishes nothing, so it must appear in the timeline but
	// NOT in the artifact table — the table reflects the store, not the graph.
	if strings.Contains(md[:strings.Index(md, "## Timeline")], "build-succeeded") {
		t.Error("artifact table lists a node that published no artifact")
	}

	// Accounting is reported (150 tokens across the two LLM nodes).
	if !strings.Contains(md, "150") {
		t.Errorf("report does not total the run's tokens\n---\n%s", md)
	}

	if !strings.Contains(out.String(), reportPath) {
		t.Errorf("report command output = %q, want it to name where the report landed", out.String())
	}
}

// TestReportHonoursOutputPathAndJSON: `--output` redirects the markdown,
// and `--json` yields the same report as a machine-readable object.
func TestReportHonoursOutputPathAndJSON(t *testing.T) {
	runID := "e2e-report-output"
	storeDir := runReportFixture(t, runID)

	dest := filepath.Join(t.TempDir(), "custom-report.md")
	if err := cli.RunReport(cli.ReportOptions{RunID: runID, StoreDir: storeDir, Output: dest}, humanPrinter(&bytes.Buffer{})); err != nil {
		t.Fatalf("RunReport --output: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("report not written to --output %s: %v", dest, err)
	}
	if !strings.Contains(string(data), runID) {
		t.Errorf("--output report does not mention the run id\n---\n%s", data)
	}
	// --output must NOT also write the in-store copy.
	if _, err := os.Stat(filepath.Join(storeDir, "runs", runID, "report.md")); err == nil {
		t.Error("--output also wrote the in-store report; the flag is a redirect, not a duplicate")
	}

	var jsonOut bytes.Buffer
	if err := cli.RunReport(cli.ReportOptions{RunID: runID, StoreDir: storeDir}, jsonPrinter(&jsonOut)); err != nil {
		t.Fatalf("RunReport --json: %v", err)
	}
	for _, want := range []string{runID, "report_mini", "surveyed-the-tree"} {
		if !strings.Contains(jsonOut.String(), want) {
			t.Errorf("JSON report does not carry %q\n---\n%s", want, jsonOut.String())
		}
	}
}

// TestReportRejectsUnknownRun: reporting on a run that does not exist is
// a clear error, never an empty-but-plausible report.
func TestReportRejectsUnknownRun(t *testing.T) {
	storeDir := t.TempDir()
	if _, err := store.New(storeDir); err != nil {
		t.Fatalf("create store: %v", err)
	}
	err := cli.RunReport(cli.ReportOptions{RunID: "no-such-run", StoreDir: storeDir}, humanPrinter(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("RunReport succeeded for an unknown run, want an error")
	}
	if !strings.Contains(err.Error(), "load run") {
		t.Errorf("error = %v, want it to say the run could not be loaded", err)
	}
}
