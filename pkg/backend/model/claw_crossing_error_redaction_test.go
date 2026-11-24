package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// resultEnvelopeFile writes one NDJSON result envelope a scripted runner can
// `cat` as its single stdout write.
func resultEnvelopeFile(t *testing.T, dir string, res delegate.IOResult) string {
	t.Helper()
	env, err := delegate.NewResultEnvelope(res)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "result.ndjson")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The runner's stderr and its structured error are the far side's account of
// a failed crossing, and this backend QUOTES them in the error it returns.
// That error travels further than the dispatch: the hooks that report a
// failed delegation read it where it is raised — the operator's run log, the
// delegate_error event, the error tracker — and the node's own scrub
// ([ClawExecutor.scrubNodeError]) only runs at Execute, after all of them.
// So the text crosses the run's redaction in the crossing, once, for every
// reader. Each case below is one of the error returns that quotes the far
// side.
func TestSandboxDispatch_TheErrorOfAFailedCrossingCarriesNoFarSideSecret(t *testing.T) {
	t.Parallel()
	const secret = "sk-ant-api03-CROSSING-0123456789abcdefghijklmnopqrstuv"
	const placeholder = "__ITERION_SECRET_api_key__"
	diagnostic := "iterion-claw-runner: anthropic: 401 unauthorized (x-api-key: " + secret + ")"

	for name, c := range map[string]struct {
		// script runs with $1 = the stderr diagnostic, $2 = the result
		// envelope file; want names the error text this path produces.
		script  string
		result  delegate.IOResult
		want    string
		exitErr bool
		jsonErr bool
	}{
		"a runner that dies with its reason on stderr": {
			script:  awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nexit 3",
			want:    "runner exited with error",
			exitErr: true,
		},
		"a runner whose structured error quotes the call it made": {
			script:  awaitTaskEnvelope + "cat \"$2\"\nexit 1",
			result:  delegate.IOResult{Error: "anthropic: 401 unauthorized (x-api-key: " + secret + ")"},
			want:    "claw backend: runner:",
			exitErr: true,
		},
		// The SAME structured error on a ZERO exit takes the other arm of
		// that return — `runner: %s` with no `(exit: %w)` to carry a chain —
		// and that arm is the only channel the far side's account has there:
		// the stderr buffer is not quoted, so nothing else in this table
		// covers it.
		"a runner whose structured error rides a zero exit": {
			script: awaitTaskEnvelope + "cat \"$2\"\nexit 0",
			result: delegate.IOResult{Error: "anthropic: 401 unauthorized (x-api-key: " + secret + ")"},
			want:   "claw backend: runner:",
		},
		"a runner that exits 0 without answering": {
			script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nexit 0",
			want:   "without sending result envelope",
		},
		"a runner whose stdout the launcher cannot decode": {
			script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nprintf 'not-an-envelope\\n'\nexit 0",
			want:   "claw backend: multiplexer",
		},
		// The decoder QUOTES the line it could not read, so the far side's
		// text rides the multiplexer's own error and not only the stderr
		// buffer beside it — and that error keeps its chain, so the quote
		// cannot be redacted by rebuilding the message.
		"a runner whose undecodable line is the secret": {
			script:  awaitTaskEnvelope + "printf 'not-an-envelope %s\\n' \"$1\"\nexit 0",
			want:    "claw backend: multiplexer",
			jsonErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var logs strings.Builder
			b := loggedTestClawBackend(&logs)
			_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: &scriptedSandbox{
					script: c.script,
					args:   []string{diagnostic, resultEnvelopeFile(t, dir, c.result)},
				},
				RedactSecrets: sinkRedaction(secret, placeholder),
			})
			if err == nil {
				t.Fatal("the crossing reported success — this witness no longer drives a failure")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the dispatch failed elsewhere than the path this case drives (want %q): %v", c.want, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the node's error carries the run's secret in clear: %v", err)
			}
			if !strings.Contains(err.Error(), placeholder) {
				t.Errorf("the far side's account is missing, or outside the run's redaction: %v", err)
			}
			// Load-bearing: the redaction replaces a quoted STRING, never the
			// wrapped error, so a classifier still reads the exit status
			// through the chain.
			var exitErr *exec.ExitError
			if c.exitErr && !errors.As(err, &exitErr) {
				t.Errorf("the typed *exec.ExitError must survive the redaction: %v", err)
			}
			// Same for the multiplexer's own error, which is the one the
			// redaction REBUILDS (it quotes the undecodable line): the
			// decoder's cause stays reachable, so what classifies a failed
			// crossing still reads the same chain.
			var syntaxErr *json.SyntaxError
			if c.jsonErr && !errors.As(err, &syntaxErr) {
				t.Errorf("the decoder's cause must survive the redaction: %v", err)
			}
			// Survive, and stay OUT of reach of a chain walk. The wrapper the
			// redaction builds answers Is/As without unwrapping precisely so
			// a reporter that records every link (pkg/errtrack, Sentry) cannot
			// print the text the pass rewrote — which, on the undecodable-line
			// case, is the secret itself.
			for i, link := range chainLinks(err) {
				if strings.Contains(link.Error(), secret) {
					t.Errorf("link %d of the node error's chain carries the run's secret in clear: %T: %v",
						i+1, link, link)
				}
			}
		})
	}
}

// The sink this redaction exists for: the operator's "Delegation failed"
// line. It is written by the store hooks, which do NOT redact it — so what
// keeps the secret out of the run log is the text the crossing handed up.
// The guard is nil here on purpose: a redaction added at the sink later must
// not be what makes this witness pass.
func TestClawCrossing_AFailedNodesOperatorLineCarriesNoFarSideSecret(t *testing.T) {
	t.Parallel()
	const secret = "ghp_CROSSING0123456789abcdefghijklmnopqrstu"
	const placeholder = "__ITERION_SECRET_deploy_token__"

	dir := t.TempDir()
	var backendLogs strings.Builder
	b := loggedTestClawBackend(&backendLogs)
	_, dispatchErr := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "deploy", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: &scriptedSandbox{
			script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nexit 2",
			args:   []string{"iterion-claw-runner: git push failed (token " + secret + ")", ""},
		},
		RedactSecrets: sinkRedaction(secret, placeholder),
	})
	if dispatchErr == nil {
		t.Fatal("the crossing reported success — this witness no longer drives a failure")
	}

	var runLog strings.Builder
	hooks := NewStoreEventHooks(context.Background(), &captureEmitter{}, "run-1",
		iterlog.New(iterlog.LevelInfo, &runLog), nil, nil)
	hooks.OnDelegateError("deploy", DelegateInfo{BackendName: delegate.BackendClaw, Error: dispatchErr})

	if !strings.Contains(runLog.String(), "Delegation failed") {
		t.Fatalf("scenario broken: no delegation-failed line in %q", runLog.String())
	}
	if strings.Contains(runLog.String(), secret) {
		t.Errorf("the run log carries the runner's secret in clear: %q", runLog.String())
	}
	if !strings.Contains(runLog.String(), placeholder) {
		t.Errorf("the runner's account is missing from the run log, or outside the run's redaction: %q", runLog.String())
	}
}

// redactedThrough is the pass for an ERROR, and the chain is what makes it
// more than a string replacement: the wrapper is built only when the text
// changed, and even then the error below stays reachable.
func TestRedactedThrough_KeepsWhatItDidNotTouchAndTheCauseItDid(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the cause")
	wrapped := fmt.Errorf("claw backend: runner: %w", sentinel)

	if got := redactedThrough(func(s string) string { return s }, wrapped); got != wrapped {
		t.Errorf("an error the pass did not change must be returned as itself: %#v", got)
	}
	// A nil redactor is the run's redaction kill switch.
	if got := redactedThrough(nil, wrapped); got != wrapped {
		t.Errorf("a nil redactor must return the error as itself: %#v", got)
	}

	red := redactedThrough(func(s string) string { return strings.ReplaceAll(s, "the cause", "__PLACEHOLDER__") }, wrapped)
	if strings.Contains(red.Error(), "the cause") {
		t.Errorf("the pass did not apply: %v", red)
	}
	if !errors.Is(red, sentinel) {
		t.Errorf("the cause must stay reachable through the redaction: %v", red)
	}
}

// The deferred report reads `stderrBuf` after whichever Wait returned, and an
// abandoned drain is the one Wait that returns while the pipe is still held:
// [exec.Cmd.Wait] closes the parent's pipes and JOINS its copying goroutines
// before it hands back [exec.ErrWaitDelay], so the buffer has no live writer
// when the defer reads it. Under `-race` this is also the witness that the
// report is not a concurrent read of a buffer still being written.
func TestSandboxDispatch_ReportsTheRunnersStderrWhenTheDrainIsAbandoned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	const warning = "iterion-claw-runner: warn: the settings-hooks document declares an event claw does not export"

	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	run := newPipeHoldingSandbox(t, dir,
		awaitTaskEnvelope+"printf '%s\\n' \""+warning+"\" >&2\ncat \"$3\"\nexit 0", resultFile)

	type outcome struct {
		res delegate.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- outcome{res, err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s", sandboxDrainBound)
	}
	if got.err != nil {
		t.Fatalf("the node was failed by the abandoned drain: %v", got.err)
	}
	if got.res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", got.res.Output)
	}
	if !strings.Contains(logs.String(), "left its pipes held") {
		t.Fatalf("scenario broken: the drain was not abandoned, so this witness proves nothing: %q", logs.String())
	}
	if !strings.Contains(logs.String(), warning) {
		t.Errorf("the runner's stderr is lost when the drain is abandoned: %q", logs.String())
	}
}

// chainLinks is every error reachable from err through every shape an error
// REPORTER follows: the two the standard library defines — the single
// `Unwrap() error` and the `Unwrap() []error` of a multi-%w join or an
// [errors.Join] — plus the `Cause() error` of the pkg/errors convention, which
// the vendored Sentry SDK walks alongside them
// (vendor/github.com/getsentry/sentry-go/exception.go, convertErrorDFS).
//
// Each shape is tested on its own instead of in a type switch, so a link
// implementing two of them is followed through both: a walk that reaches MORE
// links than a reporter does can only over-report a leak, never miss one.
// Depth-bounded like the SDK's walk, so a self-referential chain cannot hang
// the suite.
func chainLinks(err error) []error {
	const maxDepth = 32
	var out []error
	var walk func(error, int)
	walk = func(e error, depth int) {
		if e == nil || depth > maxDepth {
			return
		}
		out = append(out, e)
		if u, ok := e.(interface{ Unwrap() error }); ok {
			walk(u.Unwrap(), depth+1)
		}
		if u, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range u.Unwrap() {
				walk(c, depth+1)
			}
		}
		if u, ok := e.(interface{ Cause() error }); ok {
			walk(u.Cause(), depth+1)
		}
	}
	walk(err, 0)
	return out
}

// causeWrapper is the pkg/errors shape: a wrapper whose message hides its
// cause and whose cause is reachable only through Cause().
type causeWrapper struct {
	msg   string
	cause error
}

func (c causeWrapper) Error() string { return c.msg }
func (c causeWrapper) Cause() error  { return c.cause }

// The chain walk above is what pins "no LINK of a crossing's error carries the
// far side's text in clear", so a shape it does not follow is a leak the suite
// cannot see — and the reporter that reads these chains follows three shapes,
// not two. Driven on each shape in turn, with the secret one link down.
func TestChainLinksReachesEveryShapeAReporterFollows(t *testing.T) {
	t.Parallel()
	const secret = "sk-ant-api03-CHAINWALK-0123456789abcdef"
	leaf := errors.New("far side said: " + secret)
	for name, err := range map[string]error{
		"Unwrap() error":   fmt.Errorf("claw backend: runner: %w", leaf),
		"Unwrap() []error": errors.Join(errors.New("host side"), leaf),
		// The one shape whose own message hides its cause, so the walk is
		// the ONLY way to see the leak — exactly the wrapper this backend's
		// own redaction builds, and the one chainLinks used to miss.
		"Cause() error": causeWrapper{msg: "claw backend: runner: redacted", cause: leaf},
	} {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, link := range chainLinks(err) {
				if link == leaf {
					found = true
				}
			}
			if !found {
				t.Errorf("the chain walk does not follow %s: a link holding the far side's text in clear is invisible to it", name)
			}
		})
	}
	// The Cause shape, stated as what it costs: its message carries nothing
	// of its cause, so a suite that only reads the top-level text sees a
	// clean error over a leaking chain.
	hidden := causeWrapper{msg: "claw backend: runner: redacted", cause: leaf}
	if strings.Contains(hidden.Error(), secret) {
		t.Fatal("scenario broken: the Cause wrapper quotes its cause, so this shape proves nothing about the walk")
	}
}
