package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The tool-node contract (#3421), end to end: a real engine, the production
// executor running real `bash -c` recipes in a scratch workspace, the
// production recovery recipes, and the classifier the runner acks or naks a
// delivery on. Nothing here is stubbed but the workflow.

// tcRun is one run of a tool-only bot and what it left behind.
type tcRun struct {
	err    error
	ws     string
	events []*store.Event
	run    *store.Run
}

// tcCanary is printed on a recipe's stdout by the fixtures below. A failure
// that quotes its node's output carries it; none may.
const tcCanary = "CANARY-7f3a91"

func tcBot(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parsertest.Parse("contract.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

// tcSession is one run's engine, store and workspace, kept so a paused run
// can be resumed on them.
type tcSession struct {
	engine *runtime.Engine
	store  store.RunStore
	ws     string
}

const tcRunID = "contract"

// tcOpen builds what a runner builds for src. env sets the two dials for the
// engine built here; a name mapped to "" is the dial left unset.
func tcOpen(t *testing.T, src string, env map[string]string) *tcSession {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(runtime.EnvOutputValidation, "")
	t.Setenv(runtime.EnvOutputRefs, "")
	for name, value := range env {
		t.Setenv(name, value)
	}
	wf := tcBot(t, src)
	ws := t.TempDir()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(ws))
	engine := runtime.New(wf, st, executor,
		runtime.WithWorkDir(ws),
		runtime.WithLogger(iterlog.New(iterlog.LevelError, io.Discard)),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
	)
	return &tcSession{engine: engine, store: st, ws: ws}
}

// collect reads what the run left behind after a Run or a Resume ended in err.
func (s *tcSession) collect(t *testing.T, err error) tcRun {
	t.Helper()
	out := tcRun{err: err, ws: s.ws}
	var loadErr error
	if out.events, loadErr = s.store.LoadEvents(context.Background(), tcRunID); loadErr != nil {
		t.Fatal(loadErr)
	}
	if out.run, loadErr = s.store.LoadRun(context.Background(), tcRunID); loadErr != nil {
		t.Fatal(loadErr)
	}
	return out
}

// tcExecute runs src the way a runner does, from its entry to its end.
func tcExecute(t *testing.T, src string, env map[string]string) tcRun {
	t.Helper()
	s := tcOpen(t, src, env)
	return s.collect(t, s.engine.Run(context.Background(), tcRunID, nil))
}

// starts counts the times node was started: one per attempt.
func (r tcRun) starts(node string) int {
	n := 0
	for _, ev := range r.events {
		if ev.Type == store.EventNodeStarted && ev.NodeID == node {
			n++
		}
	}
	return n
}

func (r tcRun) count(typ store.EventType, node string) int {
	n := 0
	for _, ev := range r.events {
		if ev.Type == typ && ev.NodeID == node {
			n++
		}
	}
	return n
}

// output is what node published, read from its node_finished event, without
// the `_`-prefixed metadata the engine stamps beside it (its duration).
func (r tcRun) output(t *testing.T, node string) map[string]any {
	t.Helper()
	for _, ev := range r.events {
		if ev.Type == store.EventNodeFinished && ev.NodeID == node {
			out, _ := ev.Data["output"].(map[string]any)
			for key := range out {
				if strings.HasPrefix(key, "_") {
					delete(out, key)
				}
			}
			return out
		}
	}
	t.Fatalf("node %q never finished", node)
	return nil
}

func (r tcRun) file(t *testing.T, name string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.ws, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), true
}

// failedPermanentlyAt asserts the whole shape of a contract refusal: the run
// failed at node with TOOL_FAILED_PERMANENT, node was attempted exactly once
// under the production recipes, and the runner acks the delivery instead of
// redelivering it.
func (r tcRun) failedPermanentlyAt(t *testing.T, node string) *runtime.RuntimeError {
	t.Helper()
	var rerr *runtime.RuntimeError
	if !errors.As(r.err, &rerr) {
		t.Fatalf("the run did not fail with a RuntimeError: %v", r.err)
	}
	if rerr.Code != runtime.ErrCodeToolFailedPermanent || rerr.NodeID != node {
		t.Errorf("failed with code %s at node %q, want %s at %q: %v", rerr.Code, rerr.NodeID, runtime.ErrCodeToolFailedPermanent, node, r.err)
	}
	if r.run == nil || r.run.FailureCode != store.FailureToolFailedPermanent {
		t.Errorf("the run doc does not carry the code: %+v", r.run)
	}
	if n := r.starts(node); n != 1 {
		t.Errorf("node %q was started %d times, want exactly 1: a contract refusal is not retried", node, n)
	}
	if outcome := classifyExecResult(r.err, tcRunID); outcome.action != actionAck || outcome.op != "ack-deterministic-failure" {
		t.Errorf("the runner would redeliver this run: %+v", outcome)
	}
	return rerr
}

// noByteOfStdout asserts the canary a recipe printed is in none of the places
// a failure is persisted or shown: the returned error, the run doc, and the
// failure events. The node's own tool_called event carries its output by
// design and is not one of them.
func (r tcRun) noByteOfStdout(t *testing.T) {
	t.Helper()
	if strings.Contains(r.err.Error(), tcCanary) {
		t.Errorf("the run error quotes the recipe's stdout: %v", r.err)
	}
	if r.run != nil && strings.Contains(r.run.Error, tcCanary) {
		t.Errorf("the run doc quotes the recipe's stdout: %s", r.run.Error)
	}
	seen := 0
	for _, ev := range r.events {
		if ev.Type != store.EventRunFailed && ev.Type != store.EventNodeRecovery {
			continue
		}
		seen++
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), tcCanary) {
			t.Errorf("%s quotes the recipe's stdout: %s", ev.Type, raw)
		}
	}
	if seen < 2 {
		t.Errorf("read %d failure events, want the recovery decision and the run failure", seen)
	}
}

const tcSchemas = `schema publish_out:
  pr_number: int
  pr_url: string

schema merge_out:
  merged: bool

`

// The incident of 2026-11-19: `publish` prints a line, then its JSON, under a
// declared schema. The run stops there, before `merge` builds a URL out of a
// field nobody produced.
func TestAToolNodeThatPrintsBesideItsJSONFailsAtThatNode(t *testing.T) {
	r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf x >> publish.attempts; echo "`+tcCanary+` [main abc1234] chore: bump deps"; printf '{"pr_number": 42, "pr_url": "https://example.invalid/pr/42"}'`+"`"+`
  output: publish_out

tool merge:
  command: `+"`"+`printf %s {{outputs.publish.pr_number}} > merge.ran; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow incident:
  entry: publish
  publish -> merge
  merge -> done
`, nil)
	rerr := r.failedPermanentlyAt(t, "publish")
	for _, want := range []string{"stdout is not a JSON object", "publish_out", runtime.EnvOutputValidation} {
		if !strings.Contains(rerr.Error(), want) {
			t.Errorf("the failure does not say %q: %v", want, rerr)
		}
	}
	r.noByteOfStdout(t)
	if attempts, _ := r.file(t, "publish.attempts"); attempts != "x" {
		t.Errorf("publish ran %q, want exactly once", attempts)
	}
	if _, ran := r.file(t, "merge.ran"); ran {
		t.Error("merge ran on an output its producer never published")
	}
	if n := r.starts("merge"); n != 0 {
		t.Errorf("merge was started %d times after publish failed", n)
	}
	if n := r.count(store.EventNodeFinished, "publish"); n != 0 {
		t.Errorf("publish finished %d times: its text wrapper was published", n)
	}
}

// Anything that is not an object is not the answer, valid JSON included: an
// array, a `null`, two objects one after the other.
func TestAToolNodeAnsweringWithJSONThatIsNotAnObjectFails(t *testing.T) {
	for name, printed := range map[string]string{
		"an array":    `[1, 2]`,
		"a null":      `null`,
		"two objects": `{"pr_number": 1, "pr_url": "u"}{"pr_number": 2, "pr_url": "u"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf '%s' '`+printed+`'`+"`"+`
  output: publish_out

workflow shapes:
  entry: publish
  publish -> done
`, nil)
			rerr := r.failedPermanentlyAt(t, "publish")
			if !strings.Contains(rerr.Error(), "stdout is not a JSON object") {
				t.Errorf("the failure does not name the cause: %v", rerr)
			}
		})
	}
}

// The lines a tool prints FOR THE ENGINE are not part of its answer: a node
// that declares `output:` and prints a directive beside its JSON answers
// with the JSON — on either side of it, and whichever stream carries the
// directive — and one that prints a directive alone answered nothing.
func TestAToolNodePrintingAnEngineDirectiveBesideItsJSONAnswersWithTheJSON(t *testing.T) {
	directives := map[string]string{
		"attachment":         "[iterion] attachment=report.md name=report",
		"preview_url":        "[iterion] preview_url=http://127.0.0.1:8080/ kind=preview",
		"preview_screenshot": "[iterion] preview_screenshot=shot.png url=http://127.0.0.1:8080/",
	}
	object := `printf '{"pr_number": 42, "pr_url": "u"}\n'`
	want := map[string]any{"pr_number": float64(42), "pr_url": "u"}
	for name, directive := range directives {
		say := `printf '%s\n' '` + directive + `'`
		for position, command := range map[string]string{
			"before the JSON":            `printf report > report.md; ` + say + `; ` + object,
			"after the JSON":             `printf report > report.md; ` + object + `; ` + say,
			"on stderr, beside the JSON": `printf report > report.md; ` + say + ` >&2; ` + object,
		} {
			t.Run(name+" "+position, func(t *testing.T) {
				r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+command+"`"+`
  output: publish_out

workflow directive:
  entry: publish
  publish -> done
`, nil)
				if r.err != nil {
					t.Fatalf("a directive beside the JSON failed the node: %v", r.err)
				}
				if got := r.output(t, "publish"); !reflect.DeepEqual(got, want) {
					t.Errorf("output = %v, want the declared fields", got)
				}
				if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 0 {
					t.Errorf("%d schema warning(s) on an answer that conforms", n)
				}
			})
		}
		t.Run(name+" alone", func(t *testing.T) {
			r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf report > report.md; `+say+"`"+`
  output: publish_out

workflow directive:
  entry: publish
  publish -> done
`, nil)
			if r.err != nil {
				t.Fatalf("a directive alone failed the node: %v", r.err)
			}
			if got := r.output(t, "publish"); !reflect.DeepEqual(got, map[string]any{"result": directive}) {
				t.Errorf("output = %v, want the text wrapper of what was printed", got)
			}
			if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 1 {
				t.Errorf("%d schema warnings, want the one advisory mismatch", n)
			}
		})
		t.Run(name+" beside text", func(t *testing.T) {
			r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+say+`; echo done`+"`"+`
  output: publish_out

workflow directive:
  entry: publish
  publish -> done
`, nil)
			r.failedPermanentlyAt(t, "publish")
		})
	}
}

// Inside a fan-out branch the refusal is the same failure: the branch's node
// is not retried, the run carries the permanent code, the convergence node
// never runs, and the runner acks.
func TestAToolNodeInAFanOutBranchIsHeldToTheSameContract(t *testing.T) {
	r := tcExecute(t, tcSchemas+`router fan:
  mode: fan_out_all

tool publish:
  command: `+"`"+`echo "`+tcCanary+` [main abc1234] chore: bump deps"; printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  output: publish_out

compute side:
  output: merge_out
  expr:
    merged: "true"

tool join:
  await: wait_all
  command: `+"`"+`touch join.ran; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow branches:
  entry: fan
  fan -> publish
  fan -> side
  publish -> join
  side -> join
  join -> done
`, nil)
	if r.err == nil {
		t.Fatal("a branch printed beside its JSON under a declared output, and the run finished")
	}
	if code := runtime.ClassifyError(r.err); code != runtime.ErrCodeToolFailedPermanent {
		t.Errorf("the run failed with %s, want %s: %v", code, runtime.ErrCodeToolFailedPermanent, r.err)
	}
	if r.run == nil || r.run.FailureCode != store.FailureToolFailedPermanent {
		t.Errorf("the run doc does not carry the code: %+v", r.run)
	}
	if !strings.Contains(r.err.Error(), "stdout is not a JSON object") || strings.Contains(r.err.Error(), tcCanary) {
		t.Errorf("the failure does not name the cause, or quotes the output: %v", r.err)
	}
	if n := r.starts("publish"); n != 1 {
		t.Errorf("publish was started %d times in its branch, want 1", n)
	}
	if _, ran := r.file(t, "join.ran"); ran {
		t.Error("the convergence node ran after a branch failed its contract")
	}
	if outcome := classifyExecResult(r.err, tcRunID); outcome.action != actionAck {
		t.Errorf("the runner would redeliver this run: %+v", outcome)
	}
}

// The second net, alone: the producer answers with a JSON object that lacks a
// field its schema declares — an advisory mismatch, as before — and the
// consumer that reads that field is refused before its command runs.
func TestAToolBodyReadingAFieldItsProducerOwesIsRefusedBeforeItRuns(t *testing.T) {
	consumers := map[string]string{
		"command": `tool merge:
  command: ` + "`" + `touch merge.ran; printf %s {{outputs.publish.pr_number}} >/dev/null; printf '{"merged": true}'` + "`" + `
  output: merge_out
`,
		"sh script": `tool merge:
  script: |
    touch merge.ran
    printf '%s' {{outputs.publish.pr_number}} >/dev/null
    printf '{"merged": true}'
  language: sh
  output: merge_out
`,
		"py script": `tool merge:
  script: |
    open("merge.ran", "w").close()
    print('{"merged": true}', {{outputs.publish.pr_number}})
  language: py
  output: merge_out
`,
		"postcondition": `tool merge:
  command: ` + "`" + `touch merge.ran; printf '{"merged": true}'` + "`" + `
  postcondition: ` + "`" + `touch merge.ran; test {{outputs.publish.pr_number}} = 42` + "`" + `
  output: merge_out
`,
	}
	for surface, consumer := range consumers {
		t.Run(surface, func(t *testing.T) {
			r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf '{"pr_url": "`+tcCanary+`"}'`+"`"+`
  output: publish_out

`+consumer+`
workflow owed:
  entry: publish
  publish -> merge
  merge -> done
`, nil)
			rerr := r.failedPermanentlyAt(t, "merge")
			for _, want := range []string{"{{outputs.publish.pr_number}}", `node "publish"`, "publish_out", "not run", runtime.EnvOutputRefs} {
				if !strings.Contains(rerr.Error(), want) {
					t.Errorf("the failure does not say %q: %v", want, rerr)
				}
			}
			r.noByteOfStdout(t)
			if _, ran := r.file(t, "merge.ran"); ran {
				t.Error("the body ran on a field its producer did not supply")
			}
			if n := r.count(store.EventToolCalled, "merge"); n != 0 {
				t.Errorf("merge emitted %d tool_called events: something was executed", n)
			}
			// The producer's own verdict is unchanged: reported, and kept.
			if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 1 {
				t.Errorf("publish raised %d schema warnings, want the one advisory mismatch", n)
			}
			if got := r.output(t, "publish"); !reflect.DeepEqual(got, map[string]any{"pr_url": tcCanary}) {
				t.Errorf("publish's output moved: %v", got)
			}
		})
	}
}

// The producers that may leave a declared field out: a gate an operator
// answered in part, and a compute expression that resolves to an absence.
// Their reader is rendered as before; neither raises a schema warning of its
// own, so nothing told the author a field could be missing.
func TestAFieldAHumanOrAComputeLeftOutIsNotOwed(t *testing.T) {
	t.Run("a human gate answered in part", func(t *testing.T) {
		s := tcOpen(t, tcSchemas+`schema answer:
  go: bool
  note: string

prompt ask:
  Continue? Add a note if you want.

human gate:
  interaction: human
  instructions: ask
  output: answer

tool merge:
  command: `+"`"+`printf %s {{outputs.gate.note}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow gated:
  entry: gate
  gate -> merge
  merge -> done
`, nil)
		if err := s.engine.Run(context.Background(), tcRunID, nil); !errors.Is(err, runtime.ErrRunPaused) {
			t.Fatalf("the gate did not pause: %v", err)
		}
		r := s.collect(t, s.engine.Resume(context.Background(), tcRunID, map[string]any{"go": true}))
		if r.err != nil {
			t.Fatalf("a gate answered without its note failed the node that reads it: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.gate.note}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})

	t.Run("a compute expression that leaves a field out", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`schema counted:
  n: int
  note: string

compute count:
  output: counted
  expr:
    n: "1"

tool merge:
  command: `+"`"+`printf %s {{outputs.count.note}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow computed:
  entry: count
  count -> merge
  merge -> done
`, nil)
		if r.err != nil {
			t.Fatalf("a compute absence failed the node that reads it: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.count.note}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})
}

// A Verified Action is a tool node, and its `output:` its promise whichever
// rung answered: an idempotent skip answers with its postcondition's stdout,
// here nothing, and the node that reads a declared field of it is refused.
func TestAVerifiedActionOwesItsDeclaredFieldsOnItsSkipToo(t *testing.T) {
	r := tcExecute(t, tcSchemas+`tool prepare:
  command: `+"`"+`touch published.flag >/dev/null`+"`"+`

tool publish:
  command: `+"`"+`touch published.flag; printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  postcondition: `+"`"+`test -f published.flag`+"`"+`
  output: publish_out

tool merge:
  command: `+"`"+`printf %s {{outputs.publish.pr_number}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow skipped:
  entry: prepare
  prepare -> publish
  publish -> merge
  merge -> done
`, nil)
	r.failedPermanentlyAt(t, "merge")
	if got := r.output(t, "publish"); !reflect.DeepEqual(got, map[string]any{"result": ""}) {
		t.Errorf("the skip published %v, want the empty wrapper of its postcondition's stdout", got)
	}
	if _, ran := r.file(t, "merge.saw"); ran {
		t.Error("merge ran on a field the skip did not supply")
	}
}

// What the two rules leave alone, each on the run it used to produce.
func TestTheToolContractLeavesEveryOtherShapeAsItWas(t *testing.T) {
	t.Run("a declared output and empty stdout: the empty wrapper, reported", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`true >/dev/null`+"`"+`
  output: publish_out

workflow quiet:
  entry: publish
  publish -> done
`, nil)
		if r.err != nil {
			t.Fatalf("an empty stdout failed the node: %v", r.err)
		}
		if got := r.output(t, "publish"); !reflect.DeepEqual(got, map[string]any{"result": ""}) {
			t.Errorf("output = %v, want the empty text wrapper", got)
		}
		if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 1 {
			t.Errorf("%d schema warnings, want 1", n)
		}
	})

	t.Run("no declared output and text: the text wrapper", func(t *testing.T) {
		r := tcExecute(t, `tool say:
  command: `+"`"+`echo hello there`+"`"+`

workflow chatty:
  entry: say
  say -> done
`, nil)
		if r.err != nil {
			t.Fatalf("text without a schema failed the node: %v", r.err)
		}
		if got := r.output(t, "say"); !reflect.DeepEqual(got, map[string]any{"result": "hello there"}) {
			t.Errorf("output = %v, want the text wrapper", got)
		}
	})

	t.Run("a `result: string` schema answered with a JSON object", func(t *testing.T) {
		r := tcExecute(t, `schema said:
  result: string

tool say:
  command: `+"`"+`printf '{"result": "hello"}'`+"`"+`
  output: said

workflow typed:
  entry: say
  say -> done
`, nil)
		if r.err != nil {
			t.Fatalf("a conforming object failed the node: %v", r.err)
		}
		if got := r.output(t, "say"); !reflect.DeepEqual(got, map[string]any{"result": "hello"}) {
			t.Errorf("output = %v", got)
		}
		if n := r.count(store.EventNodeOutputSchemaMismatch, "say"); n != 0 {
			t.Errorf("%d schema warnings on a conforming output", n)
		}
	})

	t.Run("a JSON object missing a declared field: reported, kept", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf '{"pr_number": 42}'`+"`"+`
  output: publish_out

workflow partial:
  entry: publish
  publish -> done
`, nil)
		if r.err != nil {
			t.Fatalf("a partial object failed the node: %v", r.err)
		}
		if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 1 {
			t.Errorf("%d schema warnings, want 1", n)
		}
	})

	t.Run("a node with a postcondition publishes the merge of both stdouts", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`echo deploying; touch deployed.flag`+"`"+`
  postcondition: `+"`"+`test -f deployed.flag && printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  output: publish_out

workflow verified:
  entry: publish
  publish -> done
`, nil)
		if r.err != nil {
			t.Fatalf("a verified action whose recipe prints text failed: %v", r.err)
		}
		got := r.output(t, "publish")
		if got["result"] != "deploying" || got["pr_url"] != "u" {
			t.Errorf("output = %v, want the recipe's text beside the postcondition's fields", got)
		}
	})

	t.Run("a producer that did not run on this path: the reference as written", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`schema start_out:
  go: bool

tool start:
  command: `+"`"+`printf '{"go": false}'`+"`"+`
  output: start_out

tool publish:
  command: `+"`"+`printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  output: publish_out

tool merge:
  command: `+"`"+`printf %s {{outputs.publish.pr_number}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow skipped:
  entry: start
  start -> publish when go
  start -> merge when not go
  publish -> merge
  merge -> done
`, nil)
		if r.err != nil {
			t.Fatalf("a producer that never ran failed its reader: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.publish.pr_number}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})

	t.Run("a field the producer's schema does not declare: the reference as written", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  output: publish_out

tool merge:
  command: `+"`"+`printf %s {{outputs.publish._note}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow undeclared:
  entry: publish
  publish -> merge
  merge -> done
`, nil)
		if r.err != nil {
			t.Fatalf("an undeclared field failed its reader: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.publish._note}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})

	t.Run("a producer without a schema: the reference as written", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`tool publish:
  command: `+"`"+`printf '{"pr_url": "u"}'`+"`"+`

tool merge:
  command: `+"`"+`printf %s {{outputs.publish.pr_number}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow untyped:
  entry: publish
  publish -> merge
  merge -> done
`, nil)
		if r.err != nil {
			t.Fatalf("an untyped producer failed its reader: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.publish.pr_number}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})

	t.Run("an input no edge mapped: the reference as written", func(t *testing.T) {
		r := tcExecute(t, tcSchemas+`schema merge_in:
  fallback_used: string

tool publish:
  command: `+"`"+`printf '{"pr_number": 42, "pr_url": "u"}'`+"`"+`
  output: publish_out

tool merge:
  input: merge_in
  command: `+"`"+`printf %s {{input.fallback_used}} > merge.saw; printf '{"merged": true}'`+"`"+`
  output: merge_out

workflow unmapped:
  entry: publish
  publish -> merge
  merge -> done
`, nil)
		if r.err != nil {
			t.Fatalf("an unmapped input failed its reader: %v", r.err)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{input.fallback_used}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})
}

// Each dial, written out, restores the run that position used to produce —
// the wrapper for the first, the rendered hole for the second.
func TestEachToolContractDialRestoresTheLenientReading(t *testing.T) {
	bot := tcSchemas + `tool publish:
  command: ` + "`" + `echo "[main abc1234] chore: bump deps"; printf '{"pr_number": 42, "pr_url": "u"}'` + "`" + `
  output: publish_out

tool merge:
  command: ` + "`" + `printf %s {{outputs.publish.pr_number}} > merge.saw; printf '{"merged": true}'` + "`" + `
  output: merge_out

workflow dials:
  entry: publish
  publish -> merge
  merge -> done
`
	wrapper := map[string]any{"result": "[main abc1234] chore: bump deps\n{\"pr_number\": 42, \"pr_url\": \"u\"}"}

	t.Run("the validation dial off: the wrapper is published, and the reader is the one refused", func(t *testing.T) {
		for _, off := range []string{"0", "false", "off", "no", "disabled"} {
			r := tcExecute(t, bot, map[string]string{runtime.EnvOutputValidation: off})
			r.failedPermanentlyAt(t, "merge")
			if got := r.output(t, "publish"); !reflect.DeepEqual(got, wrapper) {
				t.Errorf("%s=%s: publish's output = %v, want the text wrapper", runtime.EnvOutputValidation, off, got)
			}
			if n := r.count(store.EventNodeOutputSchemaMismatch, "publish"); n != 1 {
				t.Errorf("%s=%s: %d schema warnings, want 1", runtime.EnvOutputValidation, off, n)
			}
		}
	})

	t.Run("both dials off: the run of before, the hole rendered as written", func(t *testing.T) {
		r := tcExecute(t, bot, map[string]string{runtime.EnvOutputValidation: "0", runtime.EnvOutputRefs: "0"})
		if r.err != nil {
			t.Fatalf("both dials off, and the run still failed: %v", r.err)
		}
		if got := r.output(t, "publish"); !reflect.DeepEqual(got, wrapper) {
			t.Errorf("publish's output = %v, want the text wrapper", got)
		}
		if saw, _ := r.file(t, "merge.saw"); saw != "{{outputs.publish.pr_number}}" {
			t.Errorf("merge saw %q, want the reference as written", saw)
		}
	})

	t.Run("the reference dial off alone: the producer is still refused", func(t *testing.T) {
		r := tcExecute(t, bot, map[string]string{runtime.EnvOutputRefs: "0"})
		r.failedPermanentlyAt(t, "publish")
	})

	t.Run("the validation dial on: refused for what it printed, not for a field", func(t *testing.T) {
		r := tcExecute(t, bot, map[string]string{runtime.EnvOutputValidation: "1"})
		rerr := r.failedPermanentlyAt(t, "publish")
		if !strings.Contains(rerr.Error(), "stdout is not a JSON object") {
			t.Errorf("strict validation does not name the cause: %v", rerr)
		}
	})
}
