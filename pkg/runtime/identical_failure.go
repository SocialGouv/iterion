package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Identical repeat — a tool node's command that fails the same way again
// ---------------------------------------------------------------------------
//
// A tool node's failure is EXECUTION_FAILED, the code every surface reads as
// "a later attempt can outlast it": the recovery dispatcher re-runs the node
// once, the cloud runner hands the run back to the queue, and each redelivery
// resumes it on that node in a fresh sandbox, up to the stream's delivery
// bound. That is the right reading of a command that failed ONCE — a flaky
// registry, a file another process still held.
//
// It is the wrong reading of a command that failed the same way AGAIN: same
// exit status, same output. Nothing between two executions of an unchanged
// command says the third will differ, so the engine records the repeat as
// what it is — TOOL_FAILED_PERMANENT, "a fixed exit code on unchanged inputs"
// — and the shared classification (pkg/retrypolicy) keeps every automatic
// resume away from it. An operator's resume still re-executes the node.
//
// What decides "the same way" is a fingerprint of the failure, and the count
// rides the checkpoint (store.Checkpoint.NodeFailureStreaks), so a resume on
// another replica continues the streak the first engine started.
//
// Not every failure of a command is read this way (countedToolFailure): the
// command must have run to an exit status of its own, and its output must not
// name an outage of what it talks to (transient_outage.go) — the one family
// of failures that repeats word for word and still heals by waiting.

// EnvNodeMaxIdenticalFailures names how many executions in a row a tool node's
// command may fail identically before the failure is recorded as permanent.
const EnvNodeMaxIdenticalFailures = "ITERION_NODE_MAX_IDENTICAL_FAILURES"

// DefaultNodeMaxIdenticalFailures is the limit when the variable is unset. It
// is the ONE place the default is written: the tests that speak of the
// default derive from it, and the two pages that state its value
// (docs/environment-variables.md, docs/resume.md) are held to it by
// TestTheDocsStateTheDefaultLimitOnceEach.
//
// The limit counts EXECUTIONS of the node, across engines. The recovery
// dispatcher retries an EXECUTION_FAILED once in place
// (recovery.ExecutionFailedRecipe), and the count of that retry rides the
// checkpoint: the engine that launches a run executes a failing node twice,
// every later one — a redelivery, an auto-resume, an operator's resume —
// once. A limit of N ≥ 2 therefore takes the verdict after N executions and
// N − 2 redeliveries: at 2, the first execution and its in-process retry, no
// redelivery at all; at 1, the first failure, without the retry.
const DefaultNodeMaxIdenticalFailures = 3

// nodeMaxIdenticalFailures resolves the limit: the variable, else the default.
// `0` (or off/no/false/none) removes the verdict — a command that keeps
// failing identically stays EXECUTION_FAILED and no streak is kept. `1` takes
// the verdict at the FIRST failure, before the one in-process retry. A value
// that is no count keeps the default and says so once on stderr, never a
// silent replacement.
func nodeMaxIdenticalFailures() int {
	raw := strings.TrimSpace(os.Getenv(EnvNodeMaxIdenticalFailures))
	if raw == "" {
		return DefaultNodeMaxIdenticalFailures
	}
	switch strings.ToLower(raw) {
	case "off", "no", "false", "none":
		return 0
	}
	if n, ok := parseFailureCount(raw); ok {
		return n
	}
	identicalFailuresEnvWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "iterion: %s=%q is not a count (a non-negative whole number, 0 or off to disable) — keeping the default (%d)\n",
			EnvNodeMaxIdenticalFailures, raw, DefaultNodeMaxIdenticalFailures)
	})
	return DefaultNodeMaxIdenticalFailures
}

var identicalFailuresEnvWarnOnce sync.Once

// parseFailureCount reads a count: a non-negative whole number, written in
// digits or in a float's notation ("1e6", "inf"). A value past what an int
// holds is a very LARGE count — a limit no streak reaches — and never "not a
// count": falling back to the default there would turn the most permissive
// setting an operator can write into the strictest shipped one.
func parseFailureCount(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	switch {
	case err == nil:
		return n, n >= 0
	case errors.Is(err, strconv.ErrRange):
		return math.MaxInt, !strings.HasPrefix(raw, "-")
	}
	// ParseFloat reports an out-of-range value as ±Inf with ErrRange.
	f, err := strconv.ParseFloat(raw, 64)
	if (err != nil && !errors.Is(err, strconv.ErrRange)) || math.IsNaN(f) || f < 0 || f != math.Trunc(f) {
		return 0, false
	}
	if f >= math.MaxInt {
		return math.MaxInt, true
	}
	return int(f), true
}

// IdenticalToolFailure is the cause a tool node's terminal failure carries
// once its command has failed the same way Count executions in a row: the
// fact behind the TOOL_FAILED_PERMANENT code, for a reader that asks the
// chain (errors.As) how the verdict was reached.
type IdenticalToolFailure struct {
	NodeID string
	// Count is the streak's length, this failure included; Limit is the
	// count the verdict is taken at.
	Count, Limit int
	// Cause is the failure itself, as the executor returned it.
	Cause error
}

func (f *IdenticalToolFailure) Error() string {
	return fmt.Sprintf("the same failure %d times in a row: %v", f.Count, f.Cause)
}

func (f *IdenticalToolFailure) Unwrap() error { return f.Cause }

// reason is the verdict in an operator's words, for the node's event and the
// run log.
func (f *IdenticalToolFailure) reason() string {
	return fmt.Sprintf("the command failed %d times in a row with the same exit status and output; recorded as %s: the node is not retried and the run is not redelivered",
		f.Count, ErrCodeToolFailedPermanent)
}

// remedy is what the record of the failure tells the operator to do about it
// (RuntimeError.Hint): the run document's error, run_failed and the runner's
// run_retry_skipped all read it.
//
// It does not presume where the cause is: the same words come back from a
// command that is wrong and from a service, a credential or a host that was
// not there for it.
func (f *IdenticalToolFailure) remedy(runID string) string {
	return fmt.Sprintf("the command of node %q failed %d times in a row with the same exit status and output, so nothing re-runs it automatically. "+
		"A resume re-runs this node once (`iterion resume --run-id %s`, with --force when the .bot changed): resume when what it failed on is settled — "+
		"the command or its inputs fixed, or, when the cause was outside the bot (a service, a credential, the environment it runs in), that back in place; "+
		"%s sets the count (0 keeps the automatic retries)",
		f.NodeID, f.Count, runID, EnvNodeMaxIdenticalFailures)
}

// repeatsAsPermanent reports that node is one whose failure, repeated
// identically, is read as permanent: a tool node whose recipe is a process
// with an exit status — a `command:` the shell runs, or a `script:` body —
// and whose recovery ladder reaches no model.
//
// A registry recipe (a bare tool name) and an `action:` have no exit status
// and may be a remote call. A Verified Action under `policy: recover` puts a
// model's sample between two executions (the self-repair and agent rungs,
// read through the predicates dispatch gates them on), so its next attempt
// can differ however alike the recipe's own output is — the bar
// pkg/retrypolicy sets for a deterministic code. Agent, judge and every
// other node kind never qualify.
func repeatsAsPermanent(node ir.Node) bool {
	t, ok := node.(*ir.ToolNode)
	if !ok {
		return false
	}
	switch ir.ToolRecipeKindOf(t) {
	case ir.ToolRecipeShell, ir.ToolRecipeScript:
	default:
		return false
	}
	return !ir.ToolRecoverySelfRepairRung(t) && !ir.ToolRecoveryAgentRung(t)
}

// countedToolFailure returns the failure and the command's exit status when
// execErr is one an identical repeat is read from; ok is false for every
// other failure of the node, which ends its streak.
//
// Counted: an EXECUTION_FAILED of a node repeatsAsPermanent names, whose
// command RAN to an exit status of its own, and whose text names no outage.
// Each condition has a failure it sets aside:
//
//   - no exit status in the chain — the command never ran, or what failed is
//     not a process's end: the script file could not be written, the sandbox
//     refused the recipe, a Verified Action's postcondition is unmet after a
//     recipe that exited 0. Nothing there is "the same exit status and
//     output". Under the sandbox the status is the exec client's: a client
//     that reports success while the completion line says the command exited
//     3 leaves no exit error in the chain either, and is not counted — the
//     status is known, but the failure keeps its redeliveries. It fails safe;
//   - sandbox.ErrExecStatusUnknown — the exec session ended without the
//     command's completion line, so the status in the chain is the exec
//     CLIENT's, and the command may never have started;
//   - a transient outage's wording (namesTransientOutage) — the failure that
//     repeats word for word and heals by waiting.
func countedToolFailure(node ir.Node, code ErrorCode, execErr error) (failure *model.ToolExecutionError, status int, ok bool) {
	if code != ErrCodeExecutionFailed || !repeatsAsPermanent(node) {
		return nil, 0, false
	}
	if !errors.As(execErr, &failure) || failure == nil || errors.Is(execErr, sandbox.ErrExecStatusUnknown) {
		return nil, 0, false
	}
	status, proven := failure.ExitStatus()
	if !proven || namesTransientOutage(failure.Error()) {
		return nil, 0, false
	}
	return failure, status, true
}

// toolFailureFingerprint digests what an unchanged failure leaves again: the
// exit status and the text the error carries (the command's stdout and stderr
// included, as the executor quotes them), minus the one value the executor
// draws afresh per execution (model.ToolExecutionError.StableText).
//
// A digest, never the text: the checkpoint it is written to holds no command
// output. The run and the node salt it, so equal failures of two runs — or of
// two nodes — do not share a value. Each part is length-prefixed: no two
// lists of parts hash the same bytes.
//
// Its honest limit is the text itself. A command whose output differs between
// two executions — a timestamp, a request id, a temp path of its own, a line
// count — never matches, and keeps the EXECUTION_FAILED reading.
func toolFailureFingerprint(runID, nodeID string, status int, failure *model.ToolExecutionError) string {
	h := sha256.New()
	for _, part := range []string{runID, nodeID, strconv.Itoa(status), failure.StableText()} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// noteToolFailure counts a failure of nodeID in the node's streak and returns
// the verdict when the streak reaches the limit; nil otherwise.
//
// code is the failure's classification. Only a failure countedToolFailure
// accepts is counted, and only while the limit is on; any other failure of
// the node ends its streak — the count is of failures IN A ROW, and a network
// outage or a different refusal between two of them is not one. A run torn
// down between two executions (a drain, a cancel) never reaches here, so the
// streak it leaves in the checkpoint continues.
func (e *Engine) noteToolFailure(rs *runState, nodeID string, code ErrorCode, execErr error) *IdenticalToolFailure {
	limit := nodeMaxIdenticalFailures()
	failure, status, counted := countedToolFailure(e.workflow.Nodes[nodeID], code, execErr)
	if limit <= 0 || !counted {
		delete(rs.nodeFailureStreaks, nodeID)
		return nil
	}
	fingerprint := toolFailureFingerprint(rs.runID, nodeID, status, failure)
	streak := rs.nodeFailureStreaks[nodeID]
	if streak.Fingerprint != fingerprint {
		streak = store.NodeFailureStreak{Fingerprint: fingerprint}
	}
	streak.Count++
	if rs.nodeFailureStreaks == nil {
		rs.nodeFailureStreaks = make(map[string]store.NodeFailureStreak)
	}
	rs.nodeFailureStreaks[nodeID] = streak
	if streak.Count < limit {
		return nil
	}
	return &IdenticalToolFailure{NodeID: nodeID, Count: streak.Count, Limit: limit, Cause: execErr}
}

// failIdenticalToolFailure ends the run on an identical repeat: the node's
// event and the run log say what was decided and why, then the failure is
// written resumable with the permanent code, the fact as its cause and the
// remedy as its hint.
//
// attempt is the count the recovery dispatcher holds for the node under
// EXECUTION_FAILED, the classification the verdict replaces (0 when no
// dispatcher is wired). failedOutput is booked before the checkpoint a resume
// reads its budget carry from is built, as every branch that ends an attempt
// by writing one does.
func (e *Engine) failIdenticalToolFailure(rs *runState, nodeID string, repeat *IdenticalToolFailure, attempt int, failedOutput map[string]any) error {
	reason := repeat.reason()
	data := map[string]any{
		"strategy":                "identical_tool_failure",
		"code":                    string(ErrCodeToolFailedPermanent),
		"promoted_from":           string(ErrCodeExecutionFailed),
		"identical_failures":      repeat.Count,
		"identical_failure_limit": repeat.Limit,
		"reason":                  reason,
		"error":                   repeat.Cause.Error(),
	}
	if attempt > 0 {
		data["attempt"] = attempt
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeRecovery, nodeID, data); err != nil && e.logger != nil {
		// Best-effort, as the dispatcher's own event: the verdict stands
		// when the event store is down.
		e.logger.Warn("recovery: failed to emit recovery event for node %q (code=%s, identical failures=%d): %v",
			nodeID, ErrCodeToolFailedPermanent, repeat.Count, err)
	}
	if e.logger != nil {
		e.logger.Warn("recovery: node %q: %s — fix the cause, then resume the run (%s=0 keeps the automatic retries)",
			nodeID, reason, EnvNodeMaxIdenticalFailures)
	}
	e.recordFailedNodeSpend(rs, nodeID, failedOutput)
	return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
		Code:    ErrCodeToolFailedPermanent,
		NodeID:  nodeID,
		Cause:   repeat,
		Message: nodeExecutionFailedMessage(nodeID, repeat.Cause),
		Hint:    repeat.remedy(rs.runID),
	})
}

// nodeExecutionFailedMessage is the text a node's terminal execution failure
// is recorded under. The DLQ reason, the run document's error and operator
// greps key on it, so its writers share it.
func nodeExecutionFailedMessage(nodeID string, execErr error) string {
	return fmt.Sprintf("node %q execution failed: %v", nodeID, execErr)
}

// nodeRecovered drops what a node's failures left once it has succeeded — its
// retry counters and its failure streak — so a later failure starts fresh.
func (rs *runState) nodeRecovered(nodeID string) {
	delete(rs.nodeAttempts, nodeID)
	delete(rs.nodeFailureStreaks, nodeID)
}

// cloneNodeFailureStreaks copies the streaks for a checkpoint or a derived
// run state; nil when there is none, so checkpoints stay compact.
func cloneNodeFailureStreaks(src map[string]store.NodeFailureStreak) map[string]store.NodeFailureStreak {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]store.NodeFailureStreak, len(src))
	for nodeID, streak := range src {
		dst[nodeID] = streak
	}
	return dst
}

// restoreNodeFailureStreaks rebuilds the streaks from a checkpoint, never
// aliasing its map: the run state mutates its own.
func restoreNodeFailureStreaks(src map[string]store.NodeFailureStreak) map[string]store.NodeFailureStreak {
	dst := make(map[string]store.NodeFailureStreak, len(src))
	for nodeID, streak := range src {
		dst[nodeID] = streak
	}
	return dst
}
