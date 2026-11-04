package runtime

// ---------------------------------------------------------------------------
// The engine's log lines — Layer 0 on run.log
// ---------------------------------------------------------------------------

// loggedLineKey is the key the log redactor hands the executor's
// SecretScrubber. A log line is a STRING while the only secret matcher a run
// owns speaks output maps (Layer 0, docs/secrets.md): the line rides through
// that one seam, like the recorded failure text does
// (recordedFailureText) — a second matcher of its own would drift from it,
// which is why pkg/errtrack is extended and never duplicated.
const loggedLineKey = "line"

// unscrubbableLogLine replaces a line the run's scrubber did not give back.
// Writing the raw line would publish exactly what the pass exists to
// withhold, and an empty line reads to an operator as a sink that dropped a
// record. The engine cannot report this through the logger — the report would
// re-enter the very redactor that failed — so the marker IS the report, and
// the failure-record path names the offending executor type on the run's
// first failure.
const unscrubbableLogLine = "(log line withheld: the run's secret scrubber did not return it — ScrubOutput must return every key it is given)"

// installLogRedactor forks the engine's logger so every record it KEEPS
// crosses the run's secret matcher before reaching any sink — run.log, the
// runner pod's stderr, the cloud RunLogStore tee, error tracking.
//
// Events are scrubbed at emitBranch and the recorded failure text at
// recordedFailureText, but a NON-event line had no pass at all: the
// workspace-snapshot diagnostics quote a git command's combined output, a
// capture error names what it could not read, and the engine logs on more
// than two hundred lines — a guard remembered at each is the shape that
// drifts (iterion#2822).
//
// The engine is where this belongs because the engine is where the two halves
// meet: the per-run logger comes from the launch surface (the CLI, the
// dispatcher, the runner loop, the studio service), the run's secret matcher
// comes from the executor, and runtime.New is the ONE point every surface
// goes through to join them for the engine's OWN lines — e.logger and every
// fork of it, including the loggers the engine hands its unexported helpers.
// It covers no logger the engine never holds: an EXPORTED helper
// (MergeBundlePresets, RecoverFinalize, CommitUncommittedAndFinalize) logs
// git and devbox output on the logger its CALLER passes, so coverage there
// follows the handle — which is why those callers pass the RUN's logger,
// the one runview.BuildExecutor set the matcher on (docs/secrets.md).
//
// WithRedactor FORKS: the logger the caller handed in is never mutated, so a
// process-wide logger shared by concurrent runs (the studio service runs many
// in one process) keeps its own reading, and a run's matcher cannot replace
// another run's. Installed before the first line a constructor logs, and
// before any goroutine exists, so no line is written while it lands.
//
// An executor that does not implement SecretScrubber at all (a stub, an
// injected one) wires nothing. The production executors always implement it,
// so what the cost follows is their GUARD: a guard whose sink flags are
// cleared — ITERION_SECRETS_REDACT=off, the kill switch — hands the line
// straight back, and the scan, which is what a line actually pays, does not
// run; a NIL guard (a run with no known value, no file hint and the
// heuristic off) wires the pass as an identity — the executor is a
// SecretScrubber whatever its guard, and RedactMap's nil check hands the
// line straight back — so the no-value run pays one call per line, not the
// scan. The scan is skipped for a dropped record too: WithRedactor applies
// after the level test, so a quiet logger pays nothing.
func (e *Engine) installLogRedactor() {
	if e.logger == nil {
		return
	}
	scrubber, ok := e.executor.(SecretScrubber)
	if !ok {
		return
	}
	e.logger = e.logger.WithRedactor(func(line string) string {
		return scrubbedLogLine(scrubber, line)
	})
}

// scrubbedLogLine is the pass itself: one key through the run's matcher,
// fail-closed on a scrubber that does not return it.
func scrubbedLogLine(scrubber SecretScrubber, line string) string {
	if line == "" {
		return line
	}
	text, ok := scrubber.ScrubOutput(map[string]any{loggedLineKey: line})[loggedLineKey].(string)
	if !ok {
		return unscrubbableLogLine
	}
	return text
}
