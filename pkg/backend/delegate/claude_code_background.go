package delegate

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// Background work in a claude_code session.
//
// The CLI runs subagents asynchronously by default and lets the main agent end
// its turn while they work. A finished task is announced
// (system/task_notification) and, when the main agent is idle, the CLI starts a
// new turn by itself to deliver it; a task finishing mid-turn is injected into
// that turn. Under --json-schema, every turn that ends without a StructuredOutput
// call gets `[structured-output-enforce]`, which extracts a report even while
// the agent is only waiting. A session that returns at its first result
// therefore keeps a premature report and kills the unfinished work with the
// process.
//
// backgroundTracker follows that work on the SDK reader goroutine;
// bgLifecycle decides, each time a turn's stream closes and each time the
// CLI's state changes, whether the session is done, must keep reading (the
// CLI starts the next turn itself), or needs a message from iterion first. It
// engages only when the CLI itself reported background work: a session that
// backgrounds nothing still ends at its first result.
//
// What the CLI delivered is never inferred from the stream's order — its
// queue folds, coalesces or defers notifications by rules that change with
// its version and remote flags. The session ends at rest on the CLI's own
// idle (session state events) once it held a moment — the proof the work it
// waits for came back and was delivered; the report kept then is the one the agent
// wrote in its last turn that made a request. A message of iterion's is
// answered by the turn the CLI's replay of it shows took it. The report is
// never refused when it is written — the CLI counts every StructuredOutput
// call of a turn, refused ones included, against its own retry cap
// (MAX_STRUCTURED_OUTPUT_RETRIES, 5), and a refusal loop ends the turn with
// error_max_structured_output_retries.

const (
	defaultBackgroundWait          = 30 * time.Minute
	defaultBackgroundAutoTurnGrace = 90 * time.Second
	// backgroundAnswerWaitFactor derives the default answer wait from the
	// auto-turn grace: an operator who shortens the grace to nudge sooner
	// shortens the answer window with it, in proportion, and
	// ITERION_CLAUDE_CODE_BACKGROUND_ANSWER_WAIT sets it outright.
	backgroundAnswerWaitFactor       = 5
	defaultBackgroundFinalizeTimeout = 10 * time.Minute
	// defaultBackgroundIdleSettle is how long the CLI's idle must hold before
	// the session ends on it: the CLI reports idle a moment before a turn it
	// is about to run for work that landed while it flushed, then running.
	defaultBackgroundIdleSettle = time.Second
	// defaultBackgroundResultWait is how long a held result that left the
	// CLI's set is still waited for: the CLI itself waits that long for a
	// finished task's notification to reach its queue (2.1.280: twice two
	// minutes plus one), post-completion work — a worktree to finalise — in
	// flight.
	defaultBackgroundResultWait = 5 * time.Minute

	// minBackgroundLifecycleCLI is the oldest CLI whose task protocol was
	// probed; the runner image pins it (docker/llm-clis/package.json).
	minBackgroundLifecycleCLI = "2.1.280"

	structuredOutputToolName = "StructuredOutput"
	monitorToolName          = "Monitor"
)

type backgroundLifecycleConfig struct {
	enabled       bool
	wait          time.Duration // absolute, from the first unsettled result; 0 = unbounded
	autoTurnGrace time.Duration
	// answerWait is how long the CLI may take to ANSWER a message of
	// iterion's before the delivery is given up on — a different wait from
	// autoTurnGrace, which is how long the CLI gets to start a turn of its
	// own before iterion nudges it. One grace covers a replay and the turn's
	// first assistant message, and a provider backoff in between spends it.
	answerWait      time.Duration
	finalizeTimeout time.Duration
	idleSettle      time.Duration
	resultWait      time.Duration // how long a held result on its way is waited for; 0 = the grace alone
}

func resolveBackgroundLifecycleConfig() backgroundLifecycleConfig {
	cfg := backgroundLifecycleConfig{
		enabled:         true,
		wait:            envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_WAIT", defaultBackgroundWait),
		autoTurnGrace:   envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_AUTOTURN_GRACE", defaultBackgroundAutoTurnGrace),
		finalizeTimeout: envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_FINALIZE_TIMEOUT", defaultBackgroundFinalizeTimeout),
		idleSettle:      envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_IDLE_SETTLE", defaultBackgroundIdleSettle),
		resultWait:      envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_RESULT_WAIT", defaultBackgroundResultWait),
	}
	cfg.answerWait = envDurationOr("ITERION_CLAUDE_CODE_BACKGROUND_ANSWER_WAIT", cfg.autoTurnGrace*backgroundAnswerWaitFactor)
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE"))) {
	case "off", "0", "false", "no", "disabled":
		cfg.enabled = false
	}
	return cfg
}

// printBgWaitCeilingEnv bounds the CLI's own wind-down wait for background
// work. The CLI reads 0 as "wait indefinitely", so it is pinned (both layers,
// every spawn) rather than left to a settings `env`: defaultPrintBgWaitCeiling
// is the CLI's own default, and ITERION_CLAUDE_CODE_BACKGROUND_WAIT lowers it.
const printBgWaitCeilingEnv = "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"

// defaultPrintBgWaitCeiling is the CLI's own default for that ceiling.
const defaultPrintBgWaitCeiling = 10 * time.Minute

// bgLifecycleEnv is the environment the background lifecycle reads the CLI's
// own signals through: its session state (idle is otherwise reported while
// agents still run), the tasks still running, and no idle exit of the CLI's
// own — the session ends when iterion decides.
var bgLifecycleEnv = map[string]string{
	"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1",
	"CLAUDE_CODE_BG_TASKS_REPORT_RUNNING":   "1",
	"CLAUDE_CODE_EXIT_AFTER_STOP_DELAY":     "",
}

// holdsSession reports whether a background task of this type keeps the
// session open until it comes back: a subagent or a workflow — work the CLI's
// own print-mode wind-down waits for too, up to its own ceiling
// (CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS, 10 minutes by default). Every other
// kind — a background shell, a monitor, a teammate, an MCP task — is not bound
// to end (a dev server never does), so iterion does not wait for it, where the
// CLI's wind-down also holds armed monitors (behind a feature flag) and active
// teammates. Those die with the session, as before this lifecycle existed, and
// are reported like any other work that never reported back.
func holdsSession(taskType string) bool {
	switch taskType {
	case "local_agent", "local_workflow":
		return true
	}
	return false
}

// bgTask names one background task the way the CLI announced it.
type bgTask struct {
	ID          string
	Type        string
	Description string
}

func (t bgTask) label() string {
	switch {
	case t.Description != "" && t.Type != "":
		return fmt.Sprintf("%s (%s, %s)", t.Description, t.Type, t.ID)
	case t.Description != "":
		return fmt.Sprintf("%s (%s)", t.Description, t.ID)
	default:
		return t.ID
	}
}

// maxTaskDescription bounds a task description kept for labels.
const maxTaskDescription = 160

// cleanDescription makes a task description safe to replay. It is text from
// the agent's side — the CLI takes the command itself when no description was
// given, AFTER secret materialisation — and it ends up in iterion's own
// messages to the agent, in logs and in the run store: secrets go back to
// their placeholders first, then it is one line, bounded.
func cleanDescription(s string, redact func(string) string) string {
	if redact != nil {
		s = redact(s)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxTaskDescription {
		s = string([]rune(s)[:maxTaskDescription]) + "…"
	}
	return s
}

func (t bgTask) inLive(live []bgTask) bool {
	for _, x := range live {
		if x.ID == t.ID {
			return true
		}
	}
	return false
}

func bgTaskLabels(tasks []bgTask) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.label())
	}
	return out
}

// undeliveredTask is a finished task of the main agent's whose result no
// signal of the CLI's has proven delivered yet, with the notification's
// position on the stream.
type undeliveredTask struct {
	task bgTask
	seq  int64
}

// soAttempt is one StructuredOutput call of the main agent.
type soAttempt struct {
	toolUseID string
	seq       int64
	// turn: the CLI turn the call was made in (backgroundTracker.turn).
	turn int
}

// backgroundTracker is fed by observe, which runs on the SDK reader goroutine
// (claudesdk.WithMessageObserver), ahead of runSession's select loop; the loop
// reads it under the mutex.
//
// It never infers from the stream's order what the CLI delivered: the CLI
// folds a queued notification into a request, gives it a turn of its own, or
// coalesces it with others, by rules that change with its version and remote
// flags. Two signals of the CLI's own carry that fact instead: the session
// state (idle — no turn running, no background work it waits for, none of its
// finished results still on its way to the queue; the queue itself it does not
// check, and re-kicks a turn right after for a command already in it) and the
// replay of each message iterion writes at the moment a turn takes it.
type backgroundTracker struct {
	mu  sync.Mutex
	seq int64

	visible bool // the CLI emitted at least one background-task message
	engaged bool // the CLI reported at least one background task

	haveSnapshot bool
	snapshot     map[string]bgTask // latest background_tasks_changed: the authority on what runs
	started      map[string]bgTask // task_started bookkeeping, used only while no snapshot was seen
	known        map[string]bgTask // every non-ambient background task seen, for labels
	// owned: the background shells a subagent launched — the only tasks the
	// CLI flags owned_by_subagent. They report to their subagent, never to the
	// main agent: they are not lost with the session from its point of view.
	// (A shell never holds the session; a subagent's own agents, which the CLI
	// does not flag, are held like the main agent's — its wind-down waits for
	// them too.)
	owned map[string]bool
	// mainTasks: tasks the main agent launched (task_started on one of its
	// own tool calls) — the ones whose end is notified to it.
	mainTasks map[string]bool
	// awaited: held main-agent tasks the snapshot dropped before their
	// notification came. Until it comes, their result is still on its way;
	// awaitedAt: since when.
	awaited   map[string]bgTask
	awaitedAt map[string]time.Time
	// pendingAt: held main-agent tasks that reported their end with no idle
	// since — the CLI queues a result only once its worktree is finalised
	// (minutes, for a large one), and reports idle only once every finished
	// result reached its queue. Since when.
	pendingAt map[string]time.Time
	// redact maps secret values back to their placeholders (Task.RedactSecrets).
	redact func(string) string

	mainToolUses map[string]struct{}
	// spawnPending: main-agent Agent/Task/run_in_background calls not yet
	// matched by a task_started or a tool result — work being launched.
	spawnPending map[string]struct{}

	// turn counts the CLI's turns (its init messages). requestTurn is the last
	// one that made a request — a main-agent message came in it; a turn the
	// CLI starts only to hold its query for a queued follower makes none — and
	// requestTurnSeq the position of that turn's init.
	turn           int
	turnInitSeq    int64
	requestTurn    int
	requestTurnSeq int64
	turnActive     bool
	// idle: the CLI's latest session state (session_state_changed) is idle. A
	// level, not an edge: the CLI repeats it, and reports it a moment before a
	// turn it is about to run for work that landed while it flushed — a
	// running, or a turn, after it cancels it.
	idle bool
	// idleSeq: where the CLI last reported idle; lastEndSeq: where the last
	// background task of the main agent's reported its end. An idle vouches
	// only for what came before it: a finished subagent the CLI resumes
	// outside its turn loop re-registers and ends with no running in
	// between, under an idle reported before it, and a task ending after it
	// gets a turn of its own, the CLI's running reported a moment later.
	idleSeq    int64
	lastEndSeq int64
	// monitorCalls: the Monitor tool calls of every agent, the main one's and
	// its subagents', each with the tool call of the subagent that made it
	// (empty for the main agent's); taskToolUse: the tool call each
	// task_started named, and toolUseTask the reverse; callParent: the tool
	// call of the subagent that made each tool call (empty for the main
	// agent's). A task a Monitor call launched reports each event it sees to
	// that agent, which runs a turn for it — as long as it runs.
	monitorCalls map[string]string
	taskToolUse  map[string]string
	toolUseTask  map[string]string
	callParent   map[string]string
	// sources: the turn sources running at the last message; wasSource:
	// every task that ran as one — it stays one while it runs: a finished
	// subagent's watch through the resumes of that subagent it causes.
	// sourceRan: one ran since the lifecycle last read it (takeSourceRan) — a
	// monitor that expired mid-turn included; sourceEnded: the ones that ended
	// since it last read them (takeSourceEnded). stopRequested: the tasks an
	// agent stopped — named by a stop call (TaskStop, KillShell, KillBash;
	// stopCalls keeps each call's names) whose result says it stopped
	// something, or by that result (a teammate is stopped by its agent ID or
	// name, the result names its task). sourceSince: since when turn sources have
	// run past the node's first result (results counts them), zero when none
	// does; sourceRun: how long they ran before. clock stamps both (time.Now
	// when nil).
	sources       map[string]bool
	wasSource     map[string]bool
	sourceRan     bool
	sourceEnded   map[string]bool
	stopRequested map[string]bool
	stopCalls     map[string][]string
	// stopSeq: where the stop of each task was recorded (its result);
	// startSeq: where each task started. An agent's stop reaches the watches
	// that ran under it then, never one armed after it — the CLI resumes an
	// agent a model stopped, under the same id.
	stopSeq  map[string]int64
	startSeq map[string]int64
	// parkedStop: the stopped agents that were out of the CLI's set — parked —
	// at the stop call (stopCallSeq; leftSetSeq: where each task last left
	// the set). 2.1.280 stops a parked agent's descendants with it; a running
	// agent's stop ends its own monitors and shells, never its agents'.
	parkedStop  map[string]bool
	stopCallSeq map[string]int64
	leftSetSeq  map[string]int64
	// parkedAt: where each task last left the set. The CLI re-registers the
	// agent a message (SendMessage) resumes under the message's call
	// (task_started names it), whatever name or form of its ID the message
	// addressed. sendCalls: every agent's SendMessage calls that have not
	// resumed one nor returned; msgResumed: where the CLI last resumed each
	// agent for a message — a resume someone asked for, not one its watch
	// caused; resumedByCall: for an agent a subagent resumed, that subagent's
	// own tool call — the CLI hands the resumed agent to its resumer
	// (ownerAgentId), whose running loop takes its reports.
	parkedAt      map[string]int64
	msgResumed    map[string]int64
	sendCalls     map[string]int64
	resumedByCall map[string]string
	results       int
	sourceSince   time.Time
	sourceRun     time.Duration
	clock         func() time.Time
	// The last message iterion wrote (a nudge, the wrap-up): its uuid, where
	// the CLI replayed it — a turn took it — and whether the main agent spoke
	// after that.
	msgUUID     string
	msgTakenSeq int64
	msgAnswered bool
	// undelivered: the main agent's finished tasks whose delivery nothing
	// proved. Only an idle proves it — and the session ends at rest on one:
	// before that, a notification may reach the CLI's queue well after its
	// event (a worktree to finalise first).
	undelivered []undeliveredTask

	lastSO *soAttempt

	// settles counts the waves that came back — held work gone, no result on
	// its way — in stream order, so a wave that came back and a next one
	// launched right after are told apart however far the select loop lags.
	settles    int
	wasSettled bool
	// starts counts the waves of held work that started; otherEnds the ends
	// of the main agent's background tasks the session does not hold (a
	// shell): each ends once.
	starts    int
	otherEnds int

	cliVersion string
}

func newBackgroundTracker() *backgroundTracker {
	return &backgroundTracker{
		snapshot:      map[string]bgTask{},
		started:       map[string]bgTask{},
		known:         map[string]bgTask{},
		owned:         map[string]bool{},
		mainTasks:     map[string]bool{},
		awaited:       map[string]bgTask{},
		awaitedAt:     map[string]time.Time{},
		pendingAt:     map[string]time.Time{},
		mainToolUses:  map[string]struct{}{},
		spawnPending:  map[string]struct{}{},
		monitorCalls:  map[string]string{},
		taskToolUse:   map[string]string{},
		toolUseTask:   map[string]string{},
		callParent:    map[string]string{},
		stopRequested: map[string]bool{},
		wasSource:     map[string]bool{},
		sourceEnded:   map[string]bool{},
		stopCalls:     map[string][]string{},
		stopSeq:       map[string]int64{},
		startSeq:      map[string]int64{},
		parkedStop:    map[string]bool{},
		stopCallSeq:   map[string]int64{},
		leftSetSeq:    map[string]int64{},
		parkedAt:      map[string]int64{},
		msgResumed:    map[string]int64{},
		sendCalls:     map[string]int64{},
		resumedByCall: map[string]string{},
		wasSettled:    true,
	}
}

func isMainAgentMessage(parentToolUseID *string) bool {
	return parentToolUseID == nil || *parentToolUseID == ""
}

// isTerminalTaskStatus: task_notification is emitted when a task ends; the
// non-terminal statuses are listed so an unknown new one fails toward "ended"
// only for the notification clock — what runs is the snapshot's to say.
func isTerminalTaskStatus(status string) bool {
	switch strings.ToLower(status) {
	case "running", "pending", "started", "in_progress":
		return false
	}
	return true
}

func (t *backgroundTracker) observe(msg claudesdk.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	t.observeLocked(msg)
	t.noteWaveLocked()
	t.noteSourceLocked()
}

// noteSourceLocked follows the turn sources whatever changed the CLI's set:
// one that ran, one that ended, and the time one runs past the node's first
// result — its first turn is its own, no source prompted it.
func (t *backgroundTracker) noteSourceLocked() {
	var live map[string]bool
	for _, task := range t.liveLocked() {
		if t.isTurnSourceLocked(task) {
			if live == nil {
				live = map[string]bool{}
			}
			live[task.ID] = true
			t.wasSource[task.ID] = true
		}
	}
	for id := range t.sources {
		if !live[id] {
			t.sourceEnded[id] = true
		}
	}
	t.sources = live
	t.sourceRan = t.sourceRan || len(live) > 0
	timed := len(live) > 0 && t.results > 0
	switch {
	case timed && t.sourceSince.IsZero():
		t.sourceSince = t.now()
	case !timed && !t.sourceSince.IsZero():
		t.sourceRun += t.now().Sub(t.sourceSince)
		t.sourceSince = time.Time{}
	}
}

func (t *backgroundTracker) now() time.Time {
	if t.clock != nil {
		return t.clock()
	}
	return time.Now()
}

// sourceTime is how long turn sources ran past the node's first result, by
// now.
func (t *backgroundTracker) sourceTime(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.sourceRun
	if !t.sourceSince.IsZero() && now.After(t.sourceSince) {
		d += now.Sub(t.sourceSince)
	}
	return d
}

// takeSourceRan reports whether a turn source ran since the last call.
func (t *backgroundTracker) takeSourceRan() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ran := t.sourceRan
	t.sourceRan = false
	return ran
}

// takeSourceEnded reports whether a turn source no agent stopped ended since
// the last call. It reads the stops at the close: a stop's result, which
// names the task, comes after the CLI's set dropped it.
func (t *backgroundTracker) takeSourceEnded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ended := false
	for id := range t.sourceEnded {
		ended = ended || !t.stoppedLocked(id)
	}
	clear(t.sourceEnded)
	return ended
}

// stoppedLocked reports whether an agent stopped the task: itself, or, for a
// subagent's watch, a stop recorded after the watch started of that subagent
// (its stop ends its own monitors, parked or running) or of an agent above it
// that was parked at the stop call (the CLI stops a parked agent's descendants
// with it; a running agent's stop leaves its agents running).
func (t *backgroundTracker) stoppedLocked(id string) bool {
	if t.stopRequested[id] {
		return true
	}
	parent, call := t.monitorCalls[t.taskToolUse[id]]
	if !call {
		return false
	}
	for hops := 0; parent != "" && hops < 64; hops++ {
		owner, ok := t.toolUseTask[parent]
		if !ok {
			return false
		}
		if at, stopped := t.stopSeq[owner]; stopped && at > t.startSeq[id] && (hops == 0 || t.parkedStop[owner]) {
			return true
		}
		parent = t.callParent[parent]
	}
	return false
}

// noteWaveLocked counts a wave that just came back: no held work runs and
// none of its results is still on its way.
func (t *backgroundTracker) noteWaveLocked() {
	back := len(t.heldLocked()) == 0 && len(t.awaited) == 0
	if back && !t.wasSettled {
		t.settles++
	}
	if !back && t.wasSettled {
		t.starts++
	}
	t.wasSettled = back
}

func (t *backgroundTracker) observeLocked(msg claudesdk.Message) {
	switch m := msg.(type) {
	case *claudesdk.SystemMessage:
		if m.Subtype == "init" {
			t.turnActive = true
			t.idle = false
			t.turn++
			t.turnInitSeq = t.seq
			if m.ClaudeCodeVersion != "" {
				t.cliVersion = m.ClaudeCodeVersion
			}
			return
		}
		if state, ok := m.SessionState(); ok {
			t.idle = state == "idle"
			if t.idle {
				t.idleSeq = t.seq
				clear(t.pendingAt)
			}
			return
		}
		if ev, ok := m.TaskEvent(); ok {
			t.observeTaskLocked(m.Subtype, ev)
		}
	case *claudesdk.AssistantMessage:
		t.noteMonitorCallsLocked(m)
		if !isMainAgentMessage(m.ParentToolUseID) {
			return
		}
		t.turnActive = true
		t.idle = false
		if t.requestTurn != t.turn {
			t.requestTurn, t.requestTurnSeq = t.turn, t.turnInitSeq
		}
		if t.msgTakenSeq > 0 {
			t.msgAnswered = true
		}
		if m.Message == nil {
			return
		}
		for _, blk := range m.Message.Content {
			tu, ok := blk.(*claudesdk.ToolUseBlock)
			if !ok {
				continue
			}
			t.mainToolUses[tu.ID] = struct{}{}
			if spawnsBackgroundWork(tu) {
				t.spawnPending[tu.ID] = struct{}{}
			}
			if tu.Name == structuredOutputToolName {
				t.lastSO = &soAttempt{toolUseID: tu.ID, seq: t.seq, turn: t.turn}
			}
		}
	case *claudesdk.UserMessage:
		if m.Message != nil && !m.IsReplay {
			t.noteStopResultsLocked(m.Message.Content)
			for _, blk := range m.Message.Content {
				if tr, ok := blk.(*claudesdk.ToolResultBlock); ok {
					if called, pending := t.sendCalls[tr.ToolUseID]; pending && !tr.IsError {
						// Nothing re-registered under the call: the CLI queued the
						// message for its agent — running, or being woken — which
						// takes it at its next tool round: the run live at the call,
						// or the one the wake starts. Unless the agent left the set
						// since the call: that run is over, and a message still
						// pending comes back as a resume of its own.
						if task := queuedRecipient(tr.Content); task != "" && t.parkedAt[task] < called {
							t.msgResumed[task] = t.seq
						}
					}
					delete(t.sendCalls, tr.ToolUseID)
				}
			}
		}
		if m.Message == nil || !isMainAgentMessage(m.ParentToolUseID) {
			return
		}
		if m.IsReplay {
			if m.UUID != "" && m.UUID == t.msgUUID && t.msgTakenSeq == 0 {
				t.msgTakenSeq = t.seq
			}
			return
		}
		for _, blk := range m.Message.Content {
			if tr, ok := blk.(*claudesdk.ToolResultBlock); ok {
				delete(t.spawnPending, tr.ToolUseID)
			}
		}
	case *claudesdk.ResultMessage:
		t.turnActive = false
		t.results++
	}
}

func (t *backgroundTracker) observeTaskLocked(subtype string, ev claudesdk.TaskEvent) {
	t.visible = true
	switch subtype {
	case "background_tasks_changed":
		t.haveSnapshot = true
		snap := make(map[string]bgTask, len(ev.Tasks))
		for _, bt := range ev.Tasks {
			if bt.TaskID == "" || bt.Ambient {
				continue
			}
			task := bgTask{ID: bt.TaskID, Type: bt.TaskType, Description: cleanDescription(bt.Description, t.redact)}
			snap[bt.TaskID] = task
			t.known[bt.TaskID] = task
		}
		for id := range snap {
			delete(t.leftSetSeq, id)
		}
		for id, task := range t.snapshot {
			if _, still := snap[id]; still {
				continue
			}
			t.leftSetSeq[id] = t.seq
			t.parkedAt[id] = t.seq
			if t.mainTasks[id] && holdsSession(task.Type) {
				t.awaited[id] = task
				t.awaitedAt[id] = time.Now()
			}
		}
		t.snapshot = snap
		if len(snap) > 0 {
			t.engaged = true
		}
	case "task_started":
		if ev.TaskID != "" {
			_, resumed := t.startSeq[ev.TaskID]
			t.startSeq[ev.TaskID] = t.seq
			if _, sent := t.sendCalls[ev.ToolUseID]; sent {
				// Once: the agent's later resumes re-register under the same
				// call.
				delete(t.sendCalls, ev.ToolUseID)
				t.noteMessageResumeLocked(ev.TaskID, t.callParent[ev.ToolUseID])
			} else if resumed && ev.Prompt != "" && !strings.HasPrefix(strings.TrimSpace(ev.Prompt), "<task-notification>") {
				// A resume that is no wake for queued task-notifications (a
				// watch's events): an agent that ended with a message queued
				// for it is resumed with the message as its prompt, under its
				// own call, with the main loop's context — handed to no one.
				t.noteMessageResumeLocked(ev.TaskID, "")
			}
		}
		if ev.ToolUseID != "" {
			delete(t.spawnPending, ev.ToolUseID)
			if ev.TaskID != "" {
				t.taskToolUse[ev.TaskID] = ev.ToolUseID
				t.toolUseTask[ev.ToolUseID] = ev.TaskID
			}
		}
		if ev.TaskID != "" && ev.OwnedBySubagent {
			t.owned[ev.TaskID] = true
		}
		if ev.TaskID == "" || ev.Ambient || ev.SkipTranscript || ev.OwnedBySubagent {
			return
		}
		// The main agent's tasks, whatever the flag says: a workflow's
		// task_started carries no is_backgrounded, and a foreground agent can
		// be backgrounded later — the snapshot, not this event, says what runs.
		if _, main := t.mainToolUses[ev.ToolUseID]; main {
			t.mainTasks[ev.TaskID] = true
		}
		if ev.IsBackgrounded == nil || !*ev.IsBackgrounded {
			return
		}
		task := bgTask{ID: ev.TaskID, Type: ev.TaskType, Description: cleanDescription(ev.Description, t.redact)}
		t.started[ev.TaskID] = task
		t.known[ev.TaskID] = task
		t.engaged = true
	case "task_notification":
		if !isTerminalTaskStatus(ev.Status) {
			return
		}
		delete(t.started, ev.TaskID)
		delete(t.awaited, ev.TaskID)
		delete(t.awaitedAt, ev.TaskID)
		task, bg := t.known[ev.TaskID]
		_, main := t.mainToolUses[ev.ToolUseID]
		if bg && main && !t.owned[ev.TaskID] && !ev.Ambient && !ev.SkipTranscript {
			t.undelivered = append(t.undelivered, undeliveredTask{task: task, seq: t.seq})
			t.lastEndSeq = t.seq
			if holdsSession(task.Type) {
				t.pendingAt[ev.TaskID] = time.Now()
			} else {
				t.otherEnds++
			}
		}
	}
}

// noteMonitorCallsLocked records the Monitor calls of any agent's message,
// and the tasks it stops itself.
func (t *backgroundTracker) noteMonitorCallsLocked(m *claudesdk.AssistantMessage) {
	if m.Message == nil {
		return
	}
	callParent := ""
	if m.ParentToolUseID != nil {
		callParent = *m.ParentToolUseID
	}
	for _, blk := range m.Message.Content {
		tu, ok := blk.(*claudesdk.ToolUseBlock)
		if !ok {
			continue
		}
		t.callParent[tu.ID] = callParent
		switch tu.Name {
		case "SendMessage":
			t.sendCalls[tu.ID] = t.seq
		case monitorToolName:
			parent := ""
			if m.ParentToolUseID != nil {
				parent = *m.ParentToolUseID
			}
			t.monitorCalls[tu.ID] = parent
		case "TaskStop", "KillShell", "KillBash":
			// The names it gave count once its result says it stopped something:
			// the CLI refuses a stop of an ended task, of another agent's, of an
			// unknown or ambiguous id.
			var ids []string
			for _, key := range []string{"task_id", "shell_id"} {
				if id, _ := tu.Input[key].(string); id != "" {
					ids = append(ids, id)
				}
			}
			t.stopCalls[tu.ID] = ids
			t.stopCallSeq[tu.ID] = t.seq
		}
	}
}

// noteStopResultsLocked records the stops a stop call's result confirms: the
// names the call gave and the task_id its JSON output names — never on an
// error result (the CLI refuses a stop of an ended task, of another agent's,
// of an unknown or ambiguous id).
func (t *backgroundTracker) noteStopResultsLocked(content []claudesdk.ContentBlock) {
	for _, blk := range content {
		tr, ok := blk.(*claudesdk.ToolResultBlock)
		if !ok || tr.IsError {
			continue
		}
		ids, call := t.stopCalls[tr.ToolUseID]
		if !call {
			continue
		}
		called := t.stopCallSeq[tr.ToolUseID]
		noteStop := func(id string) {
			t.stopRequested[id] = true
			t.stopSeq[id] = t.seq
			left, out := t.leftSetSeq[id]
			t.parkedStop[id] = out && left < called
		}
		for _, id := range ids {
			noteStop(id)
		}
		text, _ := tr.Content.(string)
		var out struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(text), &out) == nil && out.TaskID != "" {
			noteStop(out.TaskID)
		}
	}
}

// turnSourceLocked reports whether a running task makes the CLI run the main
// agent's turns for as long as it runs: a Monitor call's watch — the main
// agent's, or a finished subagent's (isTurnSourceLocked) — an MCP or
// WebSocket monitor, a teammate. Each of its events or messages starts a
// turn; it never ends on its own account.
func (t *backgroundTracker) turnSourceLocked(live []bgTask) bool {
	for _, task := range live {
		if t.isTurnSourceLocked(task) {
			return true
		}
	}
	return false
}

func (t *backgroundTracker) isTurnSourceLocked(task bgTask) bool {
	if parent, call := t.monitorCalls[t.taskToolUse[task.ID]]; call {
		// A subagent's watch reports to that subagent: it prompts the main
		// agent's turns once the subagent finished — the CLI keeps it alive
		// for the watch and resumes it on each event, its result then
		// reported again (wasSource keeps it one through those resumes, not
		// through one a message asked for: that subagent's own work) — to
		// the main agent unless its owner runs: a nested agent reports to
		// the agent that launched it, or that took it over with a message,
		// while that one runs, and to the main agent otherwise (--print
		// never delivers to a parked owner) — the owner's own owner never
		// enters into it.
		for hops := 0; parent != "" && hops < 2; hops++ {
			owner, ok := t.toolUseTask[parent]
			if !ok {
				return true
			}
			if t.inSetLocked(owner) && (hops > 0 || !t.wasSource[task.ID] || t.messageResumedLocked(owner)) {
				return false
			}
			parent = t.ownerCallLocked(owner, parent)
		}
		return true
	}
	switch task.Type {
	case "monitor_ws", "monitor_mcp", "in_process_teammate":
		return true
	}
	return false
}

// messageResumedLocked reports whether the CLI resumed the agent for a
// message — the main agent's or a subagent's — since it last left the set.
func (t *backgroundTracker) messageResumedLocked(id string) bool {
	at, ok := t.msgResumed[id]
	return ok && at > t.parkedAt[id]
}

// noteSendExecuted records where a SendMessage call ran: its PostToolUse hook,
// which the CLI fires once the call returned — ahead of the result it emits
// after every hook answered, a slow one included. A call runs after the calls
// before it in its message: its tool_use is streamed long before, and the
// recipient may park and be woken in between.
func (t *backgroundTracker) noteSendExecuted(toolUseID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, pending := t.sendCalls[toolUseID]; pending {
		t.seq++
		t.sendCalls[toolUseID] = t.seq
	}
}

// queuedRecipient is the agent a SendMessage result says the message was
// queued for — {"success":true,"message":"Message queued for delivery to …",
// "pin":{"id":…}} for a running agent, "… is already waking; message queued
// …" for one being woken — or "".
func queuedRecipient(content any) string {
	text := ""
	switch c := content.(type) {
	case string:
		text = c
	case []any:
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					text += s
				}
			}
		}
	}
	var out struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Pin     struct {
			ID string `json:"id"`
		} `json:"pin"`
	}
	if json.Unmarshal([]byte(text), &out) != nil || !out.Success || !strings.Contains(out.Message, "queued") {
		return ""
	}
	return out.Pin.ID
}

// ownerCallLocked is the tool call of the agent that takes the reports of the
// agent task, launched by call: a subagent that resumed it with a message
// (the CLI hands it the agent), or its launcher.
func (t *backgroundTracker) ownerCallLocked(task, call string) string {
	if by, ok := t.resumedByCall[task]; ok {
		return by
	}
	return t.callParent[call]
}

// noteMessageResumeLocked records the resume of task for a message sent from
// the tool call by of an agent ("" for the main agent's). The CLI hands the
// resumed agent to a subagent that resumed it, never to the main agent, nor
// to a subagent it sits above (the CLI's owner chain from the resumer
// reaches it).
func (t *backgroundTracker) noteMessageResumeLocked(task, by string) {
	t.msgResumed[task] = t.seq
	if by == "" {
		return
	}
	for call, hops := by, 0; call != "" && hops < 64; hops++ {
		owner, ok := t.toolUseTask[call]
		if !ok {
			break
		}
		if owner == task {
			return
		}
		call = t.ownerCallLocked(owner, call)
	}
	t.resumedByCall[task] = by
}

// inSetLocked reports whether the CLI's set holds the task.
func (t *backgroundTracker) inSetLocked(id string) bool {
	src := t.started
	if t.haveSnapshot {
		src = t.snapshot
	}
	_, ok := src[id]
	return ok
}

// sent records a message iterion wrote with this uuid: answered once the CLI
// replayed it (a turn took it) and the main agent spoke after that.
func (t *backgroundTracker) sent(uuid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	t.msgUUID, t.msgTakenSeq, t.msgAnswered = uuid, 0, false
}

// settledDelivered records what an idle that held proves: every result that
// finished before it reached the agent.
func (t *backgroundTracker) settledDelivered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.undelivered[:0]
	for _, u := range t.undelivered {
		if u.seq > t.idleSeq {
			kept = append(kept, u)
		}
	}
	t.undelivered = kept
}

// quiescedDelivered records what a grace with no turn proves — the CLI starts
// one for anything queued: every finished result reached the agent but one
// still on its way to the CLI's queue.
func (t *backgroundTracker) quiescedDelivered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.undelivered[:0]
	for _, u := range t.undelivered {
		_, awaited := t.awaited[u.task.ID]
		_, pending := t.pendingAt[u.task.ID]
		if awaited || pending {
			kept = append(kept, u)
		}
	}
	t.undelivered = kept
}

func (t *backgroundTracker) isAwaited(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.awaited[id]
	return ok
}

// onItsWayWithin reports whether a held result still on its way to the CLI's
// queue ended (or left the CLI's set) less than bound ago.
func (t *backgroundTracker) onItsWayWithin(now time.Time, bound time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range []map[string]time.Time{t.awaitedAt, t.pendingAt} {
		for _, at := range m {
			if now.Sub(at) < bound {
				return true
			}
		}
	}
	return false
}

func (t *backgroundTracker) isPending(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pendingAt[id]
	return ok
}

func (t *backgroundTracker) settleCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.settles
}

// restEpoch moves whenever a wave of held work started or came back, or a
// background task of the main agent's the session does not hold ended: the
// turns that follow are for work that ends. Turns for work that never ends (a
// monitor's events) leave it where it was.
func (t *backgroundTracker) restEpoch() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.settles + t.starts + t.otherEnds
}

// liveLocked lists every background task still running (ambient tasks
// aside), sorted by id so every reader renders the same list.
func (t *backgroundTracker) liveLocked() []bgTask {
	src := t.started
	if t.haveSnapshot {
		src = t.snapshot
	}
	out := make([]bgTask, 0, len(src))
	for _, bt := range src {
		out = append(out, bt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// heldLocked lists the running tasks the session waits for: subagents and
// workflows (holdsSession).
func (t *backgroundTracker) heldLocked() []bgTask {
	var out []bgTask
	for _, task := range t.liveLocked() {
		if holdsSession(task.Type) {
			out = append(out, task)
		}
	}
	return out
}

func (t *backgroundTracker) answeringLocked() bool {
	return t.msgUUID != "" && !t.msgAnswered
}

// owedLocked: a held result may still be owed to the main agent — one on its
// way (awaited), or one that finished after the last turn that made a
// request started.
func (t *backgroundTracker) owedLocked() bool {
	if len(t.awaited) > 0 {
		return true
	}
	for _, u := range t.undelivered {
		if holdsSession(u.task.Type) && u.seq > t.requestTurnSeq {
			return true
		}
	}
	return false
}

func (t *backgroundTracker) heldUndeliveredLocked() bool {
	for _, u := range t.undelivered {
		if holdsSession(u.task.Type) {
			return true
		}
	}
	return false
}

// settledLocked: the CLI reports idle — no work it waits for runs, and every
// finished result of it reached its queue — and did so after the last of the
// main agent's background tasks reported its end; nothing held runs or is on its way, and no message
// of iterion's awaits its answer. An idle that holds a moment
// (backgroundLifecycleConfig idleSettle) — no turn re-kicked for a command
// still queued — is the proof everything was delivered: the select loop waits
// that long before it acts on it.
func (t *backgroundTracker) settledLocked(held []bgTask) bool {
	return t.idle && t.lastEndSeq < t.idleSeq && len(held) == 0 && len(t.awaited) == 0 && !t.answeringLocked()
}

// cliMovedOn reports that the reader goroutine already saw the CLI start a
// turn the select loop has not processed yet. The loop checks it before it
// writes anything on its own initiative (a wrap-up, a nudge) so a message
// never lands in the middle of a turn it believed idle.
func (t *backgroundTracker) cliMovedOn() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turnActive
}

// bgView is a consistent read of the tracker for the select loop.
type bgView struct {
	seq         int64
	visible     bool
	engaged     bool
	live        []bgTask // every background task still running
	held        []bgTask // those the session waits for (heldLocked)
	undelivered []bgTask
	// lost is the work a process that ends now takes with it: what still
	// runs, what finished without its result provably reaching the agent —
	// the transcript shows it launched and may never show it end. A
	// subagent's own shell counts once no held task runs: it would have woken
	// its finished subagent, whose continuation re-notifies the main agent.
	// Sorted by id.
	lost    []bgTask
	idle    bool // the CLI reports idle (a level; see backgroundTracker.idle)
	settled bool // settledLocked
	owed    bool // owedLocked
	// heldUndelivered: a subagent or workflow of the main agent's finished
	// and no idle that held has proven its result delivered since.
	heldUndelivered bool
	sawSO           bool
	waitable        bool // work running or being launched (orchestration-stall guard)
	answering       bool // iterion wrote a message the agent has not answered yet
	// turnSource: a running task makes the CLI run turns for as long as it
	// runs (turnSourceLocked).
	turnSource bool
	msgTaken   bool // a turn took iterion's last message (the CLI replayed it)
	// reportInLastTurn: the main agent's last StructuredOutput call came in
	// the last turn that made a request — after every result delivered before.
	reportInLastTurn bool
	cliVersion       string
}

// lostLocked computes bgView.lost.
func (t *backgroundTracker) lostLocked(live []bgTask) []bgTask {
	byID := map[string]bgTask{}
	heldLive := len(t.heldLocked()) > 0
	for _, task := range live {
		if !t.owned[task.ID] || !heldLive {
			byID[task.ID] = task
		}
	}
	for _, u := range t.undelivered {
		byID[u.task.ID] = u.task
	}
	for id, task := range t.awaited {
		byID[id] = task
	}
	out := make([]bgTask, 0, len(byID))
	for _, task := range byID {
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func undeliveredTasks(u []undeliveredTask) []bgTask {
	out := make([]bgTask, 0, len(u))
	for _, x := range u {
		out = append(out, x.task)
	}
	return out
}

func (t *backgroundTracker) view() bgView {
	t.mu.Lock()
	defer t.mu.Unlock()
	live := t.liveLocked()
	held := t.heldLocked()
	return bgView{
		seq:              t.seq,
		visible:          t.visible,
		engaged:          t.engaged,
		live:             live,
		held:             held,
		undelivered:      undeliveredTasks(t.undelivered),
		lost:             t.lostLocked(live),
		idle:             t.idle,
		settled:          t.settledLocked(held),
		owed:             t.owedLocked(),
		heldUndelivered:  t.heldUndeliveredLocked(),
		sawSO:            t.lastSO != nil,
		waitable:         len(live) > 0 || len(t.spawnPending) > 0,
		answering:        t.answeringLocked(),
		turnSource:       t.turnSourceLocked(live),
		msgTaken:         t.msgTakenSeq > 0,
		reportInLastTurn: t.lastSO != nil && t.lastSO.turn == t.requestTurn,
		cliVersion:       t.cliVersion,
	}
}

// newMessageUUID tags a message iterion writes, for the CLI's replay of it.
func newMessageUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const backgroundAutoTurnNudge = "[iterion] Continue: take into account the results of the background work " +
	"you launched that reached you above, then complete the task."

func backgroundRestWrapUpMessage(reason string) string {
	return fmt.Sprintf("[iterion] %s. Report now with what you have: take into account the results of your background "+
		"work that reached you, and state explicitly which parts depended on work whose result you have not seen — it "+
		"will not reach you any more.", reason)
}

func backgroundWrapUpMessage(running []bgTask, reason string) string {
	quoted := make([]string, 0, len(running))
	for _, l := range bgTaskLabels(running) {
		quoted = append(quoted, strconv.Quote(l))
	}
	return fmt.Sprintf("[iterion] %s: %d background task(s) launched in this session are still running and will be "+
		"stopped when this node ends: %s. Stop waiting and report now with what you have; state explicitly which "+
		"parts those tasks were to cover and that they were not verified.", reason, len(running), strings.Join(quoted, "; "))
}

// cliVersionAtLeast compares dotted numeric versions; an unparseable version
// compares as unknown (true) — the lifecycle then relies on what the CLI
// actually emits.
func cliVersionAtLeast(have, min string) bool {
	if have == "" {
		return true
	}
	hp := strings.Split(strings.TrimSpace(have), ".")
	mp := strings.Split(min, ".")
	for i := range len(mp) {
		if i >= len(hp) {
			return false
		}
		h, err1 := strconv.Atoi(strings.TrimFunc(hp[i], func(r rune) bool { return r < '0' || r > '9' }))
		m, err2 := strconv.Atoi(mp[i])
		if err1 != nil || err2 != nil {
			return true
		}
		if h != m {
			return h > m
		}
	}
	return true
}

// resultAggregate merges the ResultMessages of one session. Per-query fields
// (usage, turns, wall duration) sum; the CLI's process-cumulative ones (cost,
// API duration, per-model usage) keep the largest / last non-empty value, so a
// later result that omits or regresses them cannot shrink what was billed.
type resultAggregate struct {
	n          int
	last       *claudesdk.ResultMessage
	usage      claudesdk.Usage
	haveUsage  bool
	numTurns   int
	durMs      int
	durAPIMs   int
	maxCost    *float64
	modelUsage map[string]claudesdk.ModelUsage
	// structured: the StructuredOutput of the result that carried the main
	// agent's last StructuredOutput call (soToolUse).
	structured any
	soToolUse  string
}

func (a *resultAggregate) add(rm *claudesdk.ResultMessage, lastSO *soAttempt) {
	if rm == nil {
		return
	}
	a.n++
	a.last = rm
	if rm.Usage != nil {
		a.haveUsage = true
		a.usage.InputTokens += rm.Usage.InputTokens
		a.usage.OutputTokens += rm.Usage.OutputTokens
		a.usage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		a.usage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
	}
	a.numTurns += rm.NumTurns
	a.durMs += rm.DurationMs
	a.durAPIMs = max(a.durAPIMs, rm.DurationAPIMs)
	if rm.TotalCostUSD != nil && (a.maxCost == nil || *rm.TotalCostUSD > *a.maxCost) {
		c := *rm.TotalCostUSD
		a.maxCost = &c
	}
	for name, mu := range rm.ModelUsage {
		if a.modelUsage == nil {
			a.modelUsage = map[string]claudesdk.ModelUsage{}
		}
		if mu != (claudesdk.ModelUsage{}) {
			a.modelUsage[name] = mu
		}
	}
	if rm.StructuredOutput != nil {
		a.structured = rm.StructuredOutput
		a.soToolUse = ""
		if lastSO != nil {
			a.soToolUse = lastSO.toolUseID
		}
	}
}

// result returns the merged ResultMessage. A single result is returned as is
// — a session that ran one query is byte-identical to the pre-lifecycle path.
// withStructured=false drops a structured output that is not the fresh final
// report (the caller then runs its usual no-structured-output recovery).
func (a *resultAggregate) result(withStructured bool) *claudesdk.ResultMessage {
	if a.n == 0 || a.last == nil {
		return nil
	}
	if a.n == 1 && withStructured {
		return a.last
	}
	merged := *a.last
	if a.haveUsage {
		u := a.usage
		merged.Usage = &u
	}
	merged.NumTurns = a.numTurns
	merged.DurationMs = a.durMs
	merged.DurationAPIMs = a.durAPIMs
	if a.maxCost != nil {
		c := *a.maxCost
		merged.TotalCostUSD = &c
	}
	if len(a.modelUsage) > 0 {
		mu := make(map[string]claudesdk.ModelUsage, len(a.modelUsage))
		maps.Copy(mu, a.modelUsage)
		merged.ModelUsage = mu
	}
	merged.StructuredOutput = nil
	if withStructured {
		merged.StructuredOutput = a.structured
	} else if a.last.StructuredOutput != nil {
		// A query that ends on a report carries it as its result text too:
		// dropped here, it must not come back through the text path.
		merged.Result = nil
	}
	return &merged
}

// bgCloseAction is what runSession does when a turn's stream closes.
type bgCloseAction int

const (
	bgReturn           bgCloseAction = iota // the session is done
	bgReenter                               // keep reading: the CLI starts the next turn itself
	bgReenterAfterSend                      // send a message (the parent is idle), then keep reading
)

// bgTimerKind names what the lifecycle's own timer is waiting for.
type bgTimerKind int

const (
	bgTimerNone     bgTimerKind = iota
	bgTimerWait                 // parent idle, held work running: the wave's wait budget
	bgTimerAutoTurn             // parent idle, the CLI not: a turn it owes, or work iterion does not hold
	bgTimerFinalize             // the wrap-up turn's own bound
	bgTimerSettle               // the CLI reports idle: end once that holds
)

// bgLifecycle is runSession's background-work state machine. It runs on the
// select-loop goroutine.
type bgLifecycle struct {
	cfg      backgroundLifecycleConfig
	tracker  *backgroundTracker
	schema   bool
	maxTurns int
	warn     func(format string, args ...any)
	emit     func(BackgroundWork)

	agg       resultAggregate
	reentries int
	started   time.Time // first result of the current wave that left held work running
	deadline  time.Time // started + wait; zero when unbounded
	// episodeSettles: the tracker's settle count when the wave started.
	episodeSettles int

	wrapUpSent       bool
	wrapUpTakenAt    time.Time // when the CLI's replay showed a turn took the wrap-up
	autoTurnNudged   bool
	autoTurnDeadline time.Time
	// answerDeadline bounds the wait for the CLI to answer a message of
	// iterion's, armed when its turn is first seen taken and cleared when
	// nothing is being answered.
	answerDeadline time.Time
	idleSince      time.Time // when the select loop first saw the CLI's current idle
	// restSince: the first close with nothing held running since the
	// tracker's restEpoch last moved (restMark: its value then) or iterion
	// last nudged. Turns the CLI runs after it for work that never ends do not
	// restart it.
	restSince time.Time
	restMark  int
	// sourceBefore: a turn source ran by an earlier close; sourceTurns: a
	// turn closed after that — the CLI may have run it for that source (a
	// monitor's event or expiry, a teammate's message), and its report then
	// answers that, not the task. From then on the session never ends on a
	// turn's report at rest: it asks for the report.
	sourceBefore   bool
	sourceTurns    bool
	waitingEmitted bool
	versionWarned  bool
	// ended: the session ends at rest — on the CLI's idle, quiesced (the CLI
	// keeps alive only work iterion does not wait for), or at a turn's close
	// on a bound (restEnd).
	ended bool

	// terminated: work that never reported back when the session ended —
	// still running, or finished without its result provably delivered. It
	// is lost with the process and reported (event + session ledger for a
	// resumed transcript).
	terminated []bgTask
}

func newBgLifecycle(cfg backgroundLifecycleConfig, tracker *backgroundTracker, schema bool, maxTurns int, warn func(string, ...any), emit func(BackgroundWork)) *bgLifecycle {
	if emit == nil {
		emit = func(BackgroundWork) {}
	}
	return &bgLifecycle{cfg: cfg, tracker: tracker, schema: schema, maxTurns: maxTurns, warn: warn, emit: emit}
}

// active reports whether the lifecycle governs this session: enabled, the CLI
// reported background work, and the CLI is one whose protocol was probed.
func (l *bgLifecycle) active(v bgView) bool {
	if !l.cfg.enabled || !v.engaged {
		return false
	}
	if !cliVersionAtLeast(v.cliVersion, minBackgroundLifecycleCLI) {
		if !l.versionWarned {
			l.versionWarned = true
			l.warn("background-work lifecycle off for this session: claude CLI %s is older than %s, the oldest whose task protocol was verified", v.cliVersion, minBackgroundLifecycleCLI)
		}
		return false
	}
	return true
}

func (l *bgLifecycle) addResult(rm *claudesdk.ResultMessage) {
	l.tracker.mu.Lock()
	var so *soAttempt
	if l.tracker.lastSO != nil {
		cp := *l.tracker.lastSO
		so = &cp
	}
	l.tracker.mu.Unlock()
	l.agg.add(rm, so)
}

// final is the ResultMessage runSession returns. The structured output is kept
// when it is the main agent's final report: at rest, the one written in the
// last turn that made a request — every result delivered before it; after a
// wrap-up, only one written after a turn took the wrap-up. A session that
// ends at rest on a bound (restEnd) keeps that turn's; one that ends
// otherwise (an error result, the wrap-up's bound) keeps none: the report it
// holds may predate work it launched, and the node's usual recovery runs. When the
// lifecycle never engaged it is kept as the CLI returned it — the
// pre-lifecycle contract.
func (l *bgLifecycle) final() *claudesdk.ResultMessage {
	v := l.tracker.view()
	if !l.active(v) {
		return l.agg.result(true)
	}
	var keep bool
	switch {
	case !v.sawSO:
		keep = true
	case l.wrapUpSent:
		keep = l.reportAfterWrapUp() && l.lastSOAccepted()
	case l.ended:
		keep = v.reportInLastTurn && l.lastSOAccepted()
	}
	return l.agg.result(keep)
}

// onExit records the work that never reported back when the session ends on
// a path that never reached a close decision (pause, error, cancellation,
// watchdog): it is lost with the process. No-op when a decision already
// recorded it.
func (l *bgLifecycle) onExit(reason string) {
	if l.terminated != nil {
		return
	}
	if lost := l.tracker.view().lost; len(lost) > 0 {
		l.terminate(lost, reason)
	}
}

func (l *bgLifecycle) lastSOAccepted() bool {
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	so := l.tracker.lastSO
	return so != nil && l.agg.soToolUse == so.toolUseID
}

// reportAfterWrapUp: the main agent's last StructuredOutput call came after a
// turn took the wrap-up.
func (l *bgLifecycle) reportAfterWrapUp() bool {
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	so, taken := l.tracker.lastSO, l.tracker.msgTakenSeq
	return so != nil && taken > 0 && so.seq > taken
}

func (l *bgLifecycle) terminate(running []bgTask, reason string) {
	if len(running) == 0 {
		return
	}
	l.terminated = running
	l.warn("%s: %d background task(s) that did not report back to the agent are lost with the session: %s", reason, len(running), strings.Join(bgTaskLabels(running), "; "))
	l.emit(BackgroundWork{Phase: BackgroundAbandoned, Running: len(running), Tasks: bgTaskLabels(running), WaitedFor: l.waited(time.Now()), Reason: reason})
}

func (l *bgLifecycle) waited(now time.Time) time.Duration {
	if l.started.IsZero() {
		return 0
	}
	return now.Sub(l.started)
}

// startWave opens the wait episode for held work, once: its budget runs from
// the first result that left it running — or from the moment held work came
// back without a turn (a finished subagent the CLI resumed).
func (l *bgLifecycle) startWave(now time.Time) {
	if !l.started.IsZero() {
		return
	}
	l.started = now
	l.episodeSettles = l.tracker.settleCount()
	if l.cfg.wait > 0 {
		l.deadline = now.Add(l.cfg.wait)
	}
}

func (l *bgLifecycle) wrapUp(now time.Time, v bgView, reason string) (bgCloseAction, string) {
	l.wrapUpSent = true
	l.warn("%s with %d background task(s) still running: asking the agent to report now", reason, len(v.held))
	l.emit(BackgroundWork{Phase: BackgroundFinalizing, Running: len(v.held), Tasks: bgTaskLabels(v.held), WaitedFor: l.waited(now), Reason: reason})
	return bgReenterAfterSend, backgroundWrapUpMessage(v.held, reason)
}

// atClose decides what follows a turn's stream close that carried result last.
// It ends the session on an error result and on the close of the turn that
// answered the wrap-up; at rest, only on the node's turn budget, the grace or
// the ceiling while a turn source runs (restEnd) — otherwise the CLI's idle
// does (see timer). It sends the wrap-up when a budget or the ceiling is
// spent while held work runs, or at rest while a held result may still reach
// the agent (restBound).
func (l *bgLifecycle) atClose(now time.Time, last *claudesdk.ResultMessage) (bgCloseAction, string) {
	l.episodeCheck(now)
	v := l.tracker.view()
	if !l.active(v) {
		return bgReturn, ""
	}
	if last.IsError || last.Subtype != claudesdk.ResultSuccess {
		l.terminate(v.lost, fmt.Sprintf("the session ended on a %s result", last.Subtype))
		return bgReturn, ""
	}
	ran := l.tracker.takeSourceRan()
	if l.sourceBefore {
		l.sourceTurns = true
	}
	l.sourceBefore = l.sourceBefore || ran || v.turnSource
	if v.answering {
		// No turn has taken iterion's last message and answered it yet: this
		// turn ran for queued work, or held its query for a follower.
		return bgReenter, ""
	}
	if l.wrapUpSent {
		l.terminate(v.lost, "the wrap-up turn ended")
		return bgReturn, ""
	}
	// The ceiling: past one wave budget of turn sources running (a monitor,
	// a teammate: backgroundTracker.turnSourceLocked — backgroundTracker.
	// sourceTime), a close where one runs, or where one no agent stopped
	// ended since the last close that got here, asks for the report. Their
	// turns, each answered with work that ends, would otherwise restart the
	// grace — or open a wave with a budget of its own — forever, a monitor the
	// agent re-arms at its expiry included. A close with none involved is
	// never cut by it.
	ended := l.tracker.takeSourceEnded()
	pastCeiling := l.cfg.wait > 0 && (v.turnSource || ended) && l.tracker.sourceTime(now) >= l.cfg.wait
	ceilingReason := fmt.Sprintf("the CLI kept running turns on its own past the background wait budget (%s)", l.cfg.wait)
	if len(v.held) == 0 {
		// Nothing held runs: the CLI delivers what is owed, then reports idle.
		// Turns it keeps running meanwhile for work that never ends (a
		// monitor's events) end at their close once the node's turn budget is
		// spent, once the grace passed since the rest clock last restarted —
		// unless a held result is still on its way — and, whatever restarted
		// it, once turn sources ran one wave budget (the ceiling above).
		if mark := l.tracker.restEpoch(); l.restSince.IsZero() || mark != l.restMark {
			l.restSince, l.restMark = now, mark
		}
		// A bound spent asks for the report instead of ending on this turn's
		// when that report may not be the agent's answer: a held result may
		// still reach it — owed, or finished with no idle that held proving it
		// delivered since (it may sit in the CLI's queue behind the commands
		// the last turns ran for, or still be on its way there: the CLI queues
		// a subagent's result once its worktree is finalised) — or a turn
		// source may have prompted a turn (sourceTurns). The request queues
		// behind what the CLI queued.
		askReport := l.sourceTurns || v.owed || v.heldUndelivered
		if l.maxTurns > 0 && l.agg.numTurns >= l.maxTurns {
			return l.restBound(now, v, askReport, fmt.Sprintf("the node's turn budget (%d) is spent", l.maxTurns))
		}
		if l.cfg.autoTurnGrace > 0 && !now.Before(l.restSince.Add(l.cfg.autoTurnGrace)) && !l.resultOnItsWay(now) {
			return l.restBound(now, v, askReport, fmt.Sprintf("the CLI kept running turns for work iterion does not wait for past the grace (%s)", l.cfg.autoTurnGrace))
		}
		if pastCeiling {
			// Past it a turn source has prompted turns: that turn's report may
			// answer it, not the task.
			return l.restWrapUp(now, ceilingReason)
		}
		return bgReenter, ""
	}
	l.startWave(now)
	if l.maxTurns > 0 && l.agg.numTurns >= l.maxTurns {
		return l.wrapUp(now, v, fmt.Sprintf("the node's turn budget (%d) is spent", l.maxTurns))
	}
	if !l.deadline.IsZero() && !now.Before(l.deadline) {
		return l.wrapUp(now, v, fmt.Sprintf("the background wait budget (%s) is spent", l.cfg.wait))
	}
	if pastCeiling {
		// Each wave has a budget of its own; while a turn source runs, the
		// waves its turns launch do not renew the session's.
		return l.wrapUp(now, v, ceilingReason)
	}
	if !l.waitingEmitted {
		l.waitingEmitted = true
		l.emit(BackgroundWork{Phase: BackgroundWaiting, Running: len(v.held), Tasks: bgTaskLabels(v.held)})
	}
	return bgReenter, ""
}

// timer returns the lifecycle's governing timer for the current state, and
// whether the generic silence / no-progress watchdogs are suspended (the
// parent is idle by design). Only meaningful once the session was kept open
// past a result, and only until the next result: once a result arrived, its
// stream close is next and atClose decides. turnActive is the select loop's
// OWN reading (the messages it has processed), never the tracker's: the
// tracker runs ahead of the loop, and a turn the loop has not seen end yet is
// not a parent at rest.
func (l *bgLifecycle) timer(now time.Time, resultPending, turnActive bool) (d time.Duration, kind bgTimerKind, suspendGeneric bool) {
	if l.reentries == 0 || resultPending {
		return 0, bgTimerNone, false
	}
	v := l.tracker.view()
	if l.wrapUpSent {
		// The wrap-up turn's bound runs from the moment a turn took it: the
		// CLI may run turns it had queued first.
		if v.msgTaken && l.wrapUpTakenAt.IsZero() {
			l.wrapUpTakenAt = now
		}
		if l.wrapUpTakenAt.IsZero() || l.cfg.finalizeTimeout <= 0 {
			return 0, bgTimerNone, false
		}
		return l.wrapUpTakenAt.Add(l.cfg.finalizeTimeout).Sub(now), bgTimerFinalize, false
	}
	if turnActive {
		// A turn runs: the generic watchdogs govern it — never the wait
		// budget, whose wrap-up waits for the turn's close — and the next
		// phase at rest gets a fresh grace.
		l.autoTurnDeadline, l.idleSince = time.Time{}, time.Time{}
		return 0, bgTimerNone, false
	}
	if v.settled {
		if l.idleSince.IsZero() {
			l.idleSince = now
		}
		return l.idleSince.Add(l.cfg.idleSettle).Sub(now), bgTimerSettle, true
	}
	// Not at rest: the settle window restarts from the next idle that holds.
	l.idleSince = time.Time{}
	if len(v.held) > 0 {
		// Held work runs — back without a turn, maybe (the CLI resumed a
		// finished subagent): its wave has a budget, and the next phase at
		// rest gets a fresh grace.
		l.autoTurnDeadline = time.Time{}
		if l.started.IsZero() {
			l.startWave(now)
			if !l.waitingEmitted {
				l.waitingEmitted = true
				l.emit(BackgroundWork{Phase: BackgroundWaiting, Running: len(v.held), Tasks: bgTaskLabels(v.held)})
			}
		}
		if l.deadline.IsZero() {
			return 0, bgTimerNone, true
		}
		return l.deadline.Sub(now), bgTimerWait, true
	}
	// Nothing held runs and no turn either, yet the CLI is not at rest: it
	// owes a turn — a result to deliver, an answer to iterion's message — or
	// keeps alive work iterion does not wait for. A grace bounds it; no grace
	// (0): the silence watchdog does.
	if l.cfg.autoTurnGrace <= 0 {
		return 0, bgTimerNone, false
	}
	if l.autoTurnDeadline.IsZero() {
		l.autoTurnDeadline = now.Add(l.cfg.autoTurnGrace)
	}
	return l.autoTurnDeadline.Sub(now), bgTimerAutoTurn, true
}

// restBound acts on a bound spent at a close at rest: the session ends there
// on that turn's report, unless that report may not be the agent's answer —
// it is then asked for the report (restWrapUp).
func (l *bgLifecycle) restBound(now time.Time, v bgView, askReport bool, reason string) (bgCloseAction, string) {
	if askReport {
		return l.restWrapUp(now, reason)
	}
	return l.restEnd(v, reason)
}

// restWrapUp asks for the report at rest, when the last turn's may not be the
// agent's answer: only one written after a turn took the request is kept, and
// its bound (finalizeTimeout) runs from then.
func (l *bgLifecycle) restWrapUp(now time.Time, reason string) (bgCloseAction, string) {
	l.wrapUpSent = true
	l.warn("%s: asking the agent to report now", reason)
	l.emit(BackgroundWork{Phase: BackgroundFinalizing, WaitedFor: l.waited(now), Reason: reason})
	return bgReenterAfterSend, backgroundRestWrapUpMessage(reason)
}

// resultOnItsWay: a held result on its way to the CLI's queue — its end
// reported with no idle since, or its task gone from the CLI's set before its
// notification — for less than resultWait.
func (l *bgLifecycle) resultOnItsWay(now time.Time) bool {
	return l.cfg.resultWait > 0 && l.tracker.onItsWayWithin(now, l.cfg.resultWait)
}

// restEnd ends the session at a turn's close while nothing held runs and the
// CLI keeps running turns for work iterion does not wait for. The report is
// that turn's; what still runs, and every result no idle proved delivered, is
// lost with the process.
func (l *bgLifecycle) restEnd(v bgView, reason string) (bgCloseAction, string) {
	l.ended = true
	l.warn("%s — ending the session at this turn's close", reason)
	l.terminated = []bgTask{}
	l.terminate(v.lost, reason)
	return bgReturn, ""
}

// settle acts on the CLI's idle that held: everything it waited for came back
// and was delivered. The session ends there (settledEnd) — unless a turn
// source may have prompted its last turns (sourceTurns): it returns the
// request for the report to send first.
func (l *bgLifecycle) settle(now time.Time) string {
	if !l.sourceTurns {
		l.settledEnd(now)
		return ""
	}
	l.episodeCheck(now)
	l.tracker.settledDelivered()
	_, msg := l.restWrapUp(now, "the CLI is at rest, a task that keeps running (a monitor, a teammate) having prompted turns since")
	return msg
}

// settledEnd ends a session at rest on the CLI's idle that held: everything
// it waited for came back and was delivered. What still runs — work the CLI does not
// wait for either: a background shell, a finished subagent's own — is lost
// with the process.
func (l *bgLifecycle) settledEnd(now time.Time) {
	l.ended = true
	// A wave that came back after the select loop's last check is settled
	// here.
	l.episodeCheck(now)
	l.tracker.settledDelivered()
	l.terminated = []bgTask{}
	l.terminate(l.tracker.view().lost, "the session ended with work iterion does not wait for")
}

// episodeCheck ends a wait episode once the wave it waited for came back (the
// tracker's settle count moved past it): a next wave of background work gets
// a budget — and a nudge — of its own.
func (l *bgLifecycle) episodeCheck(now time.Time) {
	if l.started.IsZero() || l.wrapUpSent || l.tracker.settleCount() <= l.episodeSettles {
		return
	}
	if l.waitingEmitted {
		l.emit(BackgroundWork{Phase: BackgroundSettled, WaitedFor: l.waited(now)})
	}
	l.started, l.deadline = time.Time{}, time.Time{}
	l.waitingEmitted, l.autoTurnNudged = false, false
}

// autoTurnExpired handles the grace expiring with the parent idle, no held
// work running, and the CLI not at rest. It returns done when the session
// should end — with the error it ends on, if any — or a message to send
// first (the parent is idle, so it starts a turn).
func (l *bgLifecycle) autoTurnExpired(now time.Time) (done bool, msg string, err error) {
	v := l.tracker.view()
	l.autoTurnDeadline = now.Add(l.cfg.autoTurnGrace)
	if !v.answering {
		l.answerDeadline = time.Time{}
	}
	if v.answering {
		// The CLI took the message: it gets answerWait to answer it, not the
		// one grace that re-armed above — a replay and the turn's first
		// assistant message do not fit in the window that decides when to
		// nudge.
		if l.answerDeadline.IsZero() {
			l.answerDeadline = now.Add(l.cfg.answerWait)
		}
		if now.Before(l.answerDeadline) {
			return false, "", nil
		}
		reason := "the CLI took no turn to answer iterion's message"
		l.terminate(v.lost, reason)
		return true, "", &ErrTransient{Provider: BackendClaudeCode, Reason: "background work never delivered",
			Detail: reason + " (" + strings.Join(bgTaskLabels(v.lost), "; ") + "); the session's last answer may predate it"}
	}
	if l.resultOnItsWay(now) {
		// A held result left the CLI's set but has not reached its queue yet:
		// the CLI still waits for it, and a nudge could not deliver it.
		return false, "", nil
	}
	if v.owed && !l.autoTurnNudged {
		// A result may still be owed: a turn of iterion's lets the CLI deliver
		// whatever it queued before it — and ends on the CLI's idle, like the
		// first turn at rest.
		l.autoTurnNudged = true
		l.restSince = time.Time{}
		return false, backgroundAutoTurnNudge, nil
	}
	if l.sourceTurns {
		// Its last turns may answer a turn source, not the task.
		l.tracker.quiescedDelivered()
		_, msg := l.restWrapUp(now, "the CLI starts no more turns and never reports idle, a task that keeps running (a monitor, a teammate) having prompted turns")
		return false, msg, nil
	}
	// Quiesced: the CLI starts nothing more and never reports idle — it keeps
	// alive work iterion does not wait for (or never says it is at rest).
	var others []bgTask
	for _, task := range v.live {
		if !holdsSession(task.Type) {
			others = append(others, task)
		}
	}
	blockers := ""
	if len(others) > 0 {
		blockers = fmt.Sprintf(" (%d task(s) iterion does not wait for: %s)", len(others), strings.Join(bgTaskLabels(others), "; "))
	}
	l.warn("the CLI never reported idle after its last turn%s — ending the session", blockers)
	l.ended = true
	// A grace without a turn: nothing is queued any more — what is lost is
	// what still runs, and a result still on its way to the queue.
	var lost []bgTask
	for _, task := range v.lost {
		if task.inLive(v.live) || l.tracker.isAwaited(task.ID) || l.tracker.isPending(task.ID) {
			lost = append(lost, task)
		}
	}
	l.terminated = []bgTask{}
	l.terminate(lost, "the session ended with work iterion does not wait for")
	return true, "", nil
}

// finalizeExpired handles the wrap-up turn outliving its own bound: the work
// that never reported back is lost with the session, which ends on an error —
// its last answer is the one the wrap-up was sent to replace.
func (l *bgLifecycle) finalizeExpired() error {
	reason := fmt.Sprintf("the wrap-up turn did not end within %s", l.cfg.finalizeTimeout)
	l.warn("%s — ending the session", reason)
	if lost := l.tracker.view().lost; len(lost) > 0 {
		l.terminate(lost, reason)
	} else {
		l.emit(BackgroundWork{Phase: BackgroundAbandoned, WaitedFor: l.waited(time.Now()), Reason: reason})
	}
	return &ErrTransient{Provider: BackendClaudeCode, Reason: "background wrap-up did not end",
		Detail: reason + " (tune ITERION_CLAUDE_CODE_BACKGROUND_FINALIZE_TIMEOUT)"}
}
