package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// parentOf is a parent workflow whose one subbot runs source. Like the
// document, it runs in no worktree and no sandbox: a run of it, even one a
// regression let through, never works on the checkout the tests run in.
func parentOf(name, source string) string {
	return "dsl: 2\n\nschema empty:\n  ok: bool\n\nsubbot child:\n  source: \"" + source + "\"\n  output: empty\n\ntool start:\n  command: \"true\"\n\nworkflow " + name + ":\n  entry: start\n  worktree: none\n  sandbox: none\n  start -> child\n  child -> done\n"
}

// The YAML author twin, walked as an author walks it: the document
// validated and drawn, its .bot written by `fmt --to bot` — which `--check`
// then finds current — and that .bot run to its end under the scenario
// executor, its bounded loop taken once. The document itself is refused by
// every door reachable here that would run it — run, resume, a schedule, a
// subbot's source — by name and before any effect: no run is recorded for
// it, the parked run it was asked to resume is left as it was (and the
// .bot then resumes it), no schedule is written. Every run works in the
// test's own directory, never in the checkout the tests run in.
func TestTheAuthorTwinFromItsDocumentToARun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", "author", "review.bot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	doc := filepath.Join(dir, "review.bot.yaml")
	bot := filepath.Join(dir, "review.bot")
	if err := os.WriteFile(doc, src, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}

	if err := cli.RunValidate(doc, p); err != nil {
		t.Fatalf("validate the document: %v", err)
	}
	if err := cli.RunDiagram(cli.DiagramOptions{File: doc, View: "full"}, p); err != nil {
		t.Fatalf("draw the document: %v", err)
	}
	if _, err := os.Stat(bot); err == nil {
		t.Fatal("validate or diagram wrote the .bot")
	}
	if _, err := cli.RunFmt(cli.FmtOptions{Paths: []string{doc}, To: "bot", Printer: p}); err != nil {
		t.Fatalf("fmt --to bot: %v", err)
	}
	if _, err := cli.RunFmt(cli.FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Printer: p}); err != nil {
		t.Fatalf("fmt --to bot --check, the .bot just written: %v", err)
	}

	storeDir := t.TempDir()
	const runID = "run-author-twin"
	exec := newScenarioExecutor()
	reviews := 0
	exec.on("reviewer", func(map[string]any) (map[string]any, error) {
		reviews++
		return map[string]any{"ready": reviews > 1}, nil
	})
	if err := cli.RunRun(ctx, cli.RunOptions{
		File: bot, StoreDir: storeDir, RunID: runID,
		Executor: exec, NoInteractive: true, MergeInto: "none",
	}, p); err != nil {
		t.Fatalf("run the .bot written: %v", err)
	}
	if run := loadRun(t, storeDir, runID); run.Status != store.RunStatusFinished {
		t.Fatalf("the .bot's run: %s (%s), want finished", run.Status, run.Error)
	}
	if got := strings.Join(exec.calls, " "); got != "reviewer fixer reviewer" {
		t.Fatalf("nodes run: %s, want the loop taken once", got)
	}

	// A run of the .bot the resume door can take: the reviewer's backend
	// fails, the run is left resumable.
	const parkedID = "run-author-twin-parked"
	down := newScenarioExecutor()
	down.on("reviewer", func(map[string]any) (map[string]any, error) {
		return nil, errors.New("backend down")
	})
	if err := cli.RunRun(ctx, cli.RunOptions{
		File: bot, StoreDir: storeDir, RunID: parkedID,
		Executor: down, NoInteractive: true, MergeInto: "none",
	}, p); err == nil {
		t.Fatal("the run whose backend is down succeeded")
	}
	if run := loadRun(t, storeDir, parkedID); run.Status != store.RunStatusFailedResumable {
		t.Fatalf("the parked run: %s, want failed_resumable", run.Status)
	}

	docStore := t.TempDir()
	manifest := filepath.Join(dir, "schedules.yaml")
	for name, door := range map[string]func() error{
		"run": func() error {
			return cli.RunRun(ctx, cli.RunOptions{
				File: doc, StoreDir: docStore, RunID: "run-document",
				Executor: newScenarioExecutor(), NoInteractive: true, MergeInto: "none",
			}, p)
		},
		"resume": func() error {
			return cli.RunResumeWithFile(ctx, doc, cli.ResumeOptions{
				RunID: parkedID, StoreDir: storeDir, Executor: newScenarioExecutor(),
			}, p)
		},
		"schedule": func() error {
			return cli.RunScheduleAdd(p, cli.ScheduleAddOptions{
				ScheduleCommonOptions: cli.ScheduleCommonOptions{ManifestPath: manifest},
				Name:                  "document", Cron: "0 0 * * *", Bot: doc, Workdir: dir, StoreDir: docStore,
			})
		},
	} {
		if err := door(); !errors.Is(err, bundle.ErrAuthorDocument) {
			t.Errorf("%s: %v, want the refusal of an author document, by name", name, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(docStore, "runs")); err == nil && len(entries) > 0 {
		t.Errorf("a refused door recorded %d run(s) for the document", len(entries))
	}
	if run := loadRun(t, storeDir, parkedID); run.Status != store.RunStatusFailedResumable {
		t.Errorf("the refused resume touched the parked run: %s", run.Status)
	}
	if _, err := os.Stat(manifest); err == nil {
		t.Error("the refused schedule wrote its manifest")
	}
	// The .bot resumes what the document could not.
	up := newScenarioExecutor()
	up.on("reviewer", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ready": true}, nil
	})
	if err := cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: parkedID, StoreDir: storeDir, Executor: up}, p); err != nil {
		t.Fatalf("resume the parked run with the .bot: %v", err)
	}
	if run := loadRun(t, storeDir, parkedID); run.Status != store.RunStatusFinished {
		t.Fatalf("the parked run, resumed with the .bot: %s (%s), want finished", run.Status, run.Error)
	}

	parentDoc := filepath.Join(dir, "parent_doc.bot")
	parent := filepath.Join(dir, "parent.bot")
	for path, body := range map[string]string{parentDoc: parentOf("parent_doc", "review.bot.yaml"), parent: parentOf("parent", "review.bot")} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := cli.RunValidate(parentDoc, &cli.Printer{W: &out, Format: cli.OutputJSON}); err == nil || !strings.Contains(out.String(), "C305") {
		t.Fatalf("a subbot naming the document: %v, want C305 among\n%s", err, out.String())
	}
	if err := cli.RunRun(ctx, cli.RunOptions{
		File: parentDoc, StoreDir: docStore, RunID: "run-parent-document",
		Executor: newScenarioExecutor(), NoInteractive: true, MergeInto: "none",
	}, p); err == nil || !strings.Contains(err.Error(), "C305") {
		t.Fatalf("running a parent whose subbot names the document: %v, want C305", err)
	}
	if err := cli.RunValidate(parent, p); err != nil {
		t.Fatalf("a subbot naming the .bot written: %v", err)
	}
}
