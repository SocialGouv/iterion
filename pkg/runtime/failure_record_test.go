package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The scenario is the one shape that reaches the engine's failure text
// unredacted: a value a node READ (the live output map keeps real values on
// purpose — it feeds {{outputs.X}} and the resume checkpoint) rendered into a
// `fail:` node's message, which the ENGINE builds and writes on the run
// document. A node's own execution error does NOT arrive raw: the executor
// redacts every node error's text on the way out of Execute
// (ClawExecutor.scrubNodeError), which is why a witness built on a failing
// tool's stdout would be green today whatever the engine does — tautological,
// and silent the day that pass moves.
const failNodeSecretBot = `tool readcfg:
  command: "cat creds.txt"

fail stopped:
  code: STOPPED
  message: "cannot deploy: {{outputs.readcfg.result}}"

workflow w:
  worktree: none
  sandbox: none
  entry: readcfg
  readcfg -> stopped
`

// Same workflow, resumable: the run parks failed_resumable on the guard's
// predecessor, so the acceptance's "and on a resume" has a path.
const failNodeSecretResumableBot = `tool readcfg:
  command: "cat creds.txt"

fail stopped:
  code: STOPPED
  resumable: true
  message: "cannot deploy: {{outputs.readcfg.result}}"

workflow w:
  worktree: none
  sandbox: none
  entry: readcfg
  readcfg -> stopped
`

// secretRunHarness builds a run whose workspace holds the sentinel, with the
// REAL executor (so the scrubber under test is the production one, shared by
// claw and claude_code) and the REAL per-run log tee, and returns the pieces
// the assertions read.
type secretRunHarness struct {
	store    store.RunStore
	storeDir string
	ws       string
	engine   *Engine
}

func newSecretRunHarness(t *testing.T, bot, secret, runID string) secretRunHarness {
	t.Helper()
	wf := compileBotText(t, bot)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	exec := model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })
	storeDir := t.TempDir()
	s, serr := store.New(storeDir)
	if serr != nil {
		t.Fatal(serr)
	}
	logger, closer := store.TeeRunLog(iterlog.Nop(), iterlog.LevelInfo, storeDir, runID)
	if closer == nil {
		t.Fatalf("scenario broken: no run.log tee for %s", runID)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return secretRunHarness{
		store:    s,
		storeDir: storeDir,
		ws:       ws,
		engine:   New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none"), WithLogger(logger)),
	}
}

// runLogOf reads the per-run log the tee wrote.
func runLogOf(t *testing.T, storeDir, runID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storeDir, "runs", runID, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("scenario broken: the run.log tee wrote nothing")
	}
	return string(b)
}

// assertRecordedFailure is the whole acceptance of iterion#2037 on one run:
// the placeholder is THERE (a scrub that empties the message is not one), the
// value is not, in the run document AND in run.log. The positive half matters
// as much as the negative: an absence assertion alone passes when the failure
// text never reached the sink at all.
func assertRecordedFailure(t *testing.T, h secretRunHarness, runID, secret string) {
	t.Helper()
	r, err := h.store.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	ph := secretguard.PlaceholderForName("DB")
	if r.Error == "" {
		t.Fatal("scenario broken: the run recorded no failure text")
	}
	if strings.Contains(r.Error, secret) {
		t.Errorf("the run document's error carries the value: %q", r.Error)
	}
	if !strings.Contains(r.Error, ph) {
		t.Errorf("the run document's error = %q, want %s where the value was", r.Error, ph)
	}
	if !strings.Contains(r.Error, "cannot deploy") {
		t.Errorf("the run document's error lost the diagnosis an operator greps: %q", r.Error)
	}
	log := runLogOf(t, h.storeDir, runID)
	if strings.Contains(log, secret) {
		t.Errorf("run.log carries the value:\n%s", log)
	}
	failed := ""
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "Run failed:") {
			failed = line
		}
	}
	if failed == "" {
		t.Fatalf("scenario broken: run.log holds no failure line:\n%s", log)
	}
	if !strings.Contains(failed, "cannot deploy") || !strings.Contains(failed, ph) {
		t.Errorf("run.log's failure line = %q, want the diagnosis and %s (its absence of the value alone proves nothing)", failed, ph)
	}
}

// A terminal `fail:` node: the record is written through the terminal wrapper.
func TestADeliberateFailuresRecordedMessageCarriesThePlaceholder(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	const runID = "rec-fail-terminal"
	h := newSecretRunHarness(t, failNodeSecretBot, secret, runID)
	if err := h.engine.Run(t.Context(), runID, nil); err == nil {
		t.Fatal("scenario broken: the graph routes to a fail node")
	}
	assertRecordedFailure(t, h, runID, secret)

	r, err := h.store.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("status = %q, want the terminal failure this bot declares", r.Status)
	}
}

// A `resumable: true` fail node: the record is written through the resumable
// wrapper, and the acceptance's resume half re-executes the guard's
// predecessor, reaches the same refusal and rewrites the record.
func TestAResumedDeliberateFailureRecordsThePlaceholderToo(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	const runID = "rec-fail-resumable"
	h := newSecretRunHarness(t, failNodeSecretResumableBot, secret, runID)
	if err := h.engine.Run(t.Context(), runID, nil); err == nil {
		t.Fatal("scenario broken: the graph routes to a fail node")
	}
	assertRecordedFailure(t, h, runID, secret)

	r, err := h.store.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("scenario broken: status %q is not resumable", r.Status)
	}
	if r.Checkpoint == nil {
		t.Fatal("scenario broken: a resumable failure wrote no checkpoint")
	}
	// The checkpoint is load-bearing for the resume: the scrub stops at the
	// record, so the node that read the file keeps its real output.
	got, ok := r.Checkpoint.Outputs["readcfg"]["result"].(string)
	if !ok {
		t.Fatalf("scenario broken: no readcfg output on the checkpoint: %#v", r.Checkpoint.Outputs)
	}
	if !strings.Contains(got, secret) {
		t.Errorf("the checkpoint lost the real value (a resume would replay a placeholder): %q", got)
	}

	if err := h.engine.Resume(t.Context(), runID, nil); err == nil {
		t.Fatal("scenario broken: the resumed run reaches the same refusal")
	}
	assertRecordedFailure(t, h, runID, secret)
}

// keyDroppingScrubber is an executor whose scrubber does not return the key it
// was given — the one shape recordedFailureText cannot trust.
type keyDroppingScrubber struct{ *stubExecutor }

func (keyDroppingScrubber) ScrubOutput(map[string]any) map[string]any { return map[string]any{} }

// Fail closed, not open: a scrubber that drops the text gets a marker on the
// record, never the raw message.
func TestAScrubberThatDropsTheTextRecordsTheMarker(t *testing.T) {
	e := New(minimalReviewWorkflow(), tmpStore(t), keyDroppingScrubber{newStubExecutor()})
	const raw = "cannot deploy: token=hunter2-9f8e7d6c5b4a"
	if got := e.recordedFailureText(raw); got != unscrubbableFailureText {
		t.Errorf("recordedFailureText = %q, want the withheld marker", got)
	}
	// An executor with no scrubber at all is a run with no secrets: the
	// message passes through whole.
	plain := New(minimalReviewWorkflow(), tmpStore(t), newStubExecutor())
	if got := plain.recordedFailureText(raw); got != raw {
		t.Errorf("without a scrubber recordedFailureText = %q, want it unchanged", got)
	}
}

// The chokepoint is only one while every path goes through it. The four
// wrappers in failure_record.go are the engine's only writers of a failure
// text on the run document; a tenth call site added straight on e.store would
// publish the value again, silently, which is the defect iterion#2037 names.
func TestEngineSweep_EveryRecordedFailureTextIsScrubbed(t *testing.T) {
	direct := regexp.MustCompile(`e\.store\.(FailRunResumable|FailRunTerminal|UpdateRunStatusCoded|UpdateRunStatusIfCoded)\b`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		hits := direct.FindAllString(string(body), -1)
		if name == "failure_record.go" {
			seen = len(hits)
			continue
		}
		for _, hit := range hits {
			t.Errorf("%s calls %s directly — a failure text reaches the run document through recordFailureResumable/Terminal/Status/StatusIf, which scrub it (iterion#2037); add the case there instead", name, hit)
		}
	}
	if seen != 4 {
		t.Errorf("failure_record.go holds %d store failure writes, want the 4 wrappers — a wrapper that stopped calling the store scrubs nothing", seen)
	}
}
