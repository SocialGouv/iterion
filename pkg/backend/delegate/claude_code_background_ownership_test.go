//go:build !windows

package delegate

import (
	"testing"
	"time"
)

// An owner's stop made before its watch started. The lead stops t1 while it runs (TaskStop t1
// succeeds), later resumes it with SendMessage (2.1.280 resumes an agent a
// model's TaskStop killed: no stoppedByUser marker), and t1 arms a NEW watch
// sm1, finishes and parks on it. sm1 runs the budget out and expires on its
// own; its expiry resumes t1, whose report prompts a turn that launches t2.
// No agent stopped sm1: the close must ask, as for an owned watch that
// expired on its own (TestBackground_AnOwnedWatchThatExpiredOnItsOwnAsksForTheReport).
func ownProbeStaleOwnerStop(t *testing.T, stopBefore bool) bgCloseAction {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), lnInit("2.1.280"))
	if stopBefore {
		// The lead stops t1 while it runs: success (JSON result naming t1).
		h.feed(lnAssistant("b0", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
			lnSnapshotOf(),
			jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1", "tool_use_id": "tuT1", "status": "killed", "summary": "stopped t1", "output_file": "", "session_id": "s1"}),
			lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""))
	} else {
		h.feed(lnSnapshotOf(), lnTaskNotif("t1", "tuT1"))
	}
	// The lead resumes t1 with new instructions.
	h.feed(lnAssistant("b1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "t1", "message": "watch the build until green"})),
		lnSnapshotOf(srcAgent("t1")),
		lnToolResult("tuMsg", "Message sent; agent resumed.", false, ""),
		lnAssistant("b2", "", cText("t1 watches the build")))
	if act := h.close(t0.Add(200*time.Millisecond), "t1 watches the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(300*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"))...)
	h.feedAt(t0.Add(400*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("c1", "", cText("t1 parked on its watch")))
	if act := h.close(t0.Add(500*time.Millisecond), "t1 parked on its watch"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	// sm1 expires on its own at 2.8s (source time from 0.4s: 2.4s > 2s).
	h.feedAt(t0.Add(2800*time.Millisecond), lnSnapshotOf(srcAgent("t1")))
	h.feedAt(t0.Add(2900*time.Millisecond), lnSnapshotOf(), lnTaskNotif("t1", "tuT1"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2")...)
	h.feed(lnAssistant("d2", "", cText("t1's watch timed out; t2 re-runs CI")))
	return h.close(t0.Add(3*time.Second), "t1's watch timed out; t2 re-runs CI")
}

func TestBackground_AResumedOwnersWatchExpiringOnItsOwnAsks(t *testing.T) {
	if act := ownProbeStaleOwnerStop(t, false); act != bgReenterAfterSend {
		t.Fatalf("control (no stop at all): action %v, want the ask", act)
	}
}

func TestBackground_AStopBeforeAWatchStartedDoesNotHideItsExpiry(t *testing.T) {
	if act := ownProbeStaleOwnerStop(t, true); act != bgReenterAfterSend {
		t.Fatalf("action %v: a stop of t1 BEFORE it was resumed and armed sm1 hid sm1's own expiry past the budget — the wave launched then keeps a budget of its own", act)
	}
}

// A nested watch counts as the lead's turn source once every agent above its
// owner finished, not while one still runs. 2.1.280: a subagent's background agent is owned by
// that subagent (He = LW(callerAgentId) ?? main): t1a's reports — the watch's
// events resuming it — go to t1 while t1 runs, never to the lead. The lead's
// own monitor ran 1.5s of the 2s budget and was stopped; t1 (a long worker)
// is launched at 1.5s (its wave: budget to 3.5s); t1 launches t1a, which arms
// sm2 and parks at 1.7s while t1 keeps working. At 2.5s a test shell of the
// lead's ends and prompts a turn: no lead turn source ran since mon1's stop.
func ownProbeNestedWatch(t *testing.T) (*lifecycleAt, bgCloseAction) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(armLines("a1", "mon1", "tuMon")...)
	h.feed(lnAssistant("a2", "", cText("watching the log")))
	if act := h.close(t0, "watching the log"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	// 1.5s: an event's turn; the lead stops mon1, launches t1 and a test shell.
	h.feedAt(t0.Add(1500*time.Millisecond), lnIdle(), lnRunning(), lnInit("2.1.280"),
		lnAssistant("b1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "mon1"})),
		lnSnapshotOf(),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: mon1 (tail -f app.log)","task_id":"mon1","task_type":"local_bash","command":"tail -f app.log"}`, false, ""))
	h.feed(launchLines("b2", "t1", "tuT1")...)
	h.feed(lnAssistant("b3", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("b4", "", cText("t1 refactors; tests running")))
	if act := h.close(t0.Add(1500*time.Millisecond), "t1 refactors; tests running"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// t1 launches t1a (background), which arms sm2 and parks; t1 keeps working.
	h.feedAt(t0.Add(1600*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch the build log", "description": "sub-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), shell, srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(1650*time.Millisecond), subMonitorLines("s1", "tuT1a", "sm2", "tuSub2", srcAgent("t1"), shell, srcAgent("t1a"))...)
	h.feedAt(t0.Add(1700*time.Millisecond), lnSnapshotOf(srcAgent("t1"), shell, srcMonitor("sm2")))
	// 2.5s: the lead's test shell ends; its turn notes it, t1 still at work.
	h.feedAt(t0.Add(2500*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("c1", "", cText("tests pass; t1 still at work")))
	return h, h.close(t0.Add(2600*time.Millisecond), "tests pass; t1 still at work")
}

func TestBackground_ANestedWatchIsNoLeadSourceWhileAnAgentAboveRuns(t *testing.T) {
	h, act := ownProbeNestedWatch(t)
	if act != bgReenter {
		t.Fatalf("action %v (wrapUpSent=%v): t1 — still at work, its wave's own budget running to 3.5s — was cut by the ceiling of a watch whose events go to t1, not to the lead", act, h.l.wrapUpSent)
	}
}

// A background subagent stops its own watch. t1, finished, is parked on its
// watch sm1, which runs the budget out; sm1's event resumes t1 (back in the
// set) while a lead turn (a test shell's end) runs. t1 stops sm1 itself — the
// CLI drops sm1 from its set at the kill, t1's tool result follows — before
// the lead's close: the watch ended because an agent stopped it.
func TestBackground_ASubagentsOwnStopDuringALeadTurnSparesTheWave(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"),
		lnAssistant("b1", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("b2", "", cText("t1 in; tests running")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 in; tests running"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 2.5s: sm1's event resumes t1; the lead's test shell ends, its turn starts.
	h.feedAt(t0.Add(2500*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1"), shell))
	h.feedAt(t0.Add(2550*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"))
	// t1 stops its own watch: the call, then the kill (the set drops sm1).
	h.feedAt(t0.Add(2600*time.Millisecond),
		lnAssistant("s2", "tuT1", cToolUse("tuSubStop", "TaskStop", map[string]any{"task_id": "sm1"})),
		lnSnapshotOf(srcAgent("t1")))
	h.feed(lnToolResult("tuSubStop", `{"message":"Successfully stopped task: sm1 (tail -f build.log)","task_id":"sm1","task_type":"local_bash","command":"tail -f build.log"}`, false, "tuT1"))
	h.feed(lnAssistant("c1", "", cText("tests pass")))
	if act := h.close(t0.Add(2700*time.Millisecond), "tests pass"); act != bgReenter {
		t.Fatalf("action %v: t1 stopped its own watch, and the lead's wave was cut as if the watch had ended on its own", act)
	}
}

// A stop of a running agent does not reach its own agents' watches in
// 2.1.280: TaskStop's descendant cascade runs only for a stopped agent that
// was parked (`let Z=Vv(M); if(await X.kill(...),Z){...uJe...}`, Vv = a
// completed local_agent with keepalive reasons); a running agent's own
// monitors and shells die in its loop's cleanup (es/Sr), its background
// children do not. t1 runs; its agent t1a arms sm2 and parks. The lead stops
// t1 while it runs: t1a and sm2 survive; sm2 later expires on its own past
// the budget, its expiry notice resumes t1a, whose report (t1 gone) prompts a
// lead turn that launches t2.
func ownProbeRunningAncestorStop(t *testing.T) bgCloseAction {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch", "description": "sub-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1a", "sm2", "tuSub2", srcAgent("t1"), srcAgent("t1a"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")))
	// 0.3s: a lead turn (the lead reconsiders) stops t1 while it RUNS; t1a and
	// sm2 are not t1's cascade (t1 was not parked) and keep going.
	h.feedAt(t0.Add(300*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("b1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnSnapshotOf(srcMonitor("sm2")),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1", "tool_use_id": "tuT1", "status": "killed", "summary": "stopped t1", "output_file": "", "session_id": "s1"}),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""),
		lnAssistant("b2", "", cText("t1 stopped; its watcher keeps an eye on the build")))
	if act := h.close(t0.Add(400*time.Millisecond), "t1 stopped; its watcher keeps an eye on the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 2.8s: sm2 expires ON ITS OWN; its notice resumes t1a, which reports.
	h.feedAt(t0.Add(2800*time.Millisecond), lnSnapshotOf(srcAgent("t1a")))
	h.feedAt(t0.Add(2900*time.Millisecond), lnSnapshotOf(), lnTaskNotif("t1a", "tuT1a"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2")...)
	h.feed(lnAssistant("d2", "", cText("the watch timed out; t2 re-runs CI")))
	return h.close(t0.Add(3*time.Second), "the watch timed out; t2 re-runs CI")
}

func TestBackground_ARunningAgentsStopLeavesItsAgentsWatchUnstopped(t *testing.T) {
	if act := ownProbeRunningAncestorStop(t); act != bgReenterAfterSend {
		t.Fatalf("action %v: sm2 — alive after t1's stop (no cascade from a running agent) — expired on its own past the budget, read as stopped through t1", act)
	}
}

// The cascade from a parked agent: t1 launches t1a, which
// arms sm2 and parks; t1 finishes and parks too (keepalive agent:t1a) — sm2's
// events now reach the lead through both. Past the budget the lead stops t1,
// PARKED: 2.1.280 stops its descendants with it (Z=Vv(M) → uJe), sm2 dies
// with t1a. The wave launched with the stop is spared.
func TestBackground_AParkedAgentsStopCoversItsAgentsWatch(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch", "description": "sub-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1a", "sm2", "tuSub2", srcAgent("t1"), srcAgent("t1a"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")))
	// t1 finishes, parked on t1a: its report prompts the lead's turn.
	h.feedAt(t0.Add(300*time.Millisecond), lnSnapshotOf(srcMonitor("sm2")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported; its watcher keeps an eye on the build")))
	if act := h.close(t0.Add(400*time.Millisecond), "t1 reported; its watcher keeps an eye on the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 2.5s (past the budget): the lead stops t1, parked; the cascade takes
	// t1a and sm2.
	h.feedAt(t0.Add(2500*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnSnapshotOf(),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1a", "tool_use_id": "tuT1a", "status": "stopped", "summary": "stopped t1a", "output_file": "", "session_id": "s1"}),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""))
	h.feed(launchLines("c2", "t2", "tuT2")...)
	h.feed(lnAssistant("c3", "", cText("t1 stopped; t2 writes the release notes")))
	h.tr.mu.Lock()
	ws := h.tr.wasSource["sm2"]
	h.tr.mu.Unlock()
	if !ws {
		t.Fatal("scenario broken: sm2 never ran as a turn source")
	}
	if act := h.close(t0.Add(3*time.Second), "t1 stopped; t2 writes the release notes"); act != bgReenter {
		t.Fatalf("action %v — sm2, killed with t1a by the lead's stop of the parked t1, read as ending on its own", act)
	}
}

// A finished subagent's watch stays a lead source through the resumes it causes,
// not through one the lead asks for: wasSource kept it one through ANY
// resume of its owner — a resume the LEAD asked for (SendMessage: a new, long
// job) included, during which the watch's events go to t1, which runs, not to
// the lead. t1 parks on sm1 at 0.2s (source from 0.2s); at 1.0s the lead gives
// t1 a new job (SendMessage), t1 runs; at 2.6s a lead test shell ends and its
// turn closes while t1 still works (its wave: from the SendMessage close, to
// 3.1s).
func TestBackground_ALeadResumeOfAWatchsOwnerIsNoWatchResume(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 1.0s: the lead (an operator nudge's turn) gives t1 a new job and starts
	// a test shell. The CLI re-registers the agent it resumes under the
	// message's call (resumeAgentBackground's W5: toolUseId of the SendMessage
	// call's context; the terminal claim of its park lets task_started out).
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "t1", "message": "also fix the flaky login test"})),
		lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1")),
		lnTaskStarted("t1", "tuMsg", true, false),
		lnToolResult("tuMsg", `{"success":true,"message":"Resuming agent t1","resumedAgentId":"t1"}`, false, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("t1 fixes the flaky test; the suite runs")))
	if act := h.close(t0.Add(1100*time.Millisecond), "t1 fixes the flaky test; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("the suite passes; t1 still at work")))
	if act := h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work"); act != bgReenter {
		t.Fatalf("action %v: t1, at work on the lead's new job (its wave to 3.1s), cut by the ceiling of a watch whose events go to t1", act)
	}
}

func ownershipCfg() backgroundLifecycleConfig {
	return backgroundLifecycleConfig{wait: 2 * time.Second, autoTurnGrace: time.Hour, finalizeTimeout: time.Minute, idleSettle: time.Second}
}

// The CLI's cascade from a PARKED agent reaches every descendant: t1
// parked for t1a, t1a parked for t1b, t1b parked on its watch sm3. The lead
// stops t1 past the budget and launches t2 with the stop: sm3 ended because
// an agent stopped it (three agents down), the wave must not be cut.
func TestBackground_ACascadeReachesAWatchThreeAgentsDown(t *testing.T) {
	h := newLifecycleAt(t, ownershipCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch", "description": "sub", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(80*time.Millisecond),
		lnAssistant("s1", "tuT1a", cToolUse("tuT1b", "Agent", map[string]any{"prompt": "watch", "description": "subsub", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b")),
		lnTaskStarted("t1b", "tuT1b", true, false),
		lnToolResult("tuT1b", "Async agent launched successfully.", false, "tuT1a"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s2", "tuT1b", "sm3", "tuSub3", srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm3")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported; its agents watch the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 reported; its agents watch the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(2500*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnSnapshotOf(),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1a", "tool_use_id": "tuT1a", "status": "stopped", "summary": "stopped t1a", "output_file": "", "session_id": "s1"}),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1b", "tool_use_id": "tuT1b", "status": "stopped", "summary": "stopped t1b", "output_file": "", "session_id": "s1"}),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""))
	h.feed(launchLines("c2", "t2", "tuT2")...)
	h.feed(lnAssistant("c3", "", cText("t1 stopped; t2 writes the release notes")))
	if !h.tr.wasSource["sm3"] {
		t.Fatal("scenario broken: sm3 never ran as a turn source")
	}
	if act := h.close(t0.Add(3*time.Second), "t1 stopped; t2 writes the release notes"); act != bgReenter {
		t.Errorf("action %v — sm3, killed three agents down by the stop of t1, read as ending on its own, and the wave cut", act)
	}
}

// Only a stop call's result names a stopped task: a tool whose JSON
// result happens to carry a task_id (an MCP job tracker) stops nothing. mon1
// expires on its own past the budget; its notice's turn queries the tracker
// and launches t2 — the wave must be cut.
func TestBackground_AToolResultNamingATaskIsNoStop(t *testing.T) {
	h := newLifecycleAt(t, ownershipCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(armLines("a1", "mon1", "tuMon")...)
	h.feed(lnAssistant("a2", "", cText("watching the log")))
	if act := h.close(t0, "watching the log"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(2500*time.Millisecond), lnSnapshotOf(), lnTaskNotif("mon1", "tuMon"), lnInit("2.1.280"))
	h.feed(lnAssistant("b1", "", cToolUse("tuQ", "mcp__jobs__status", map[string]any{"job": "mon1"})),
		lnToolResult("tuQ", `{"task_id":"mon1","state":"expired"}`, false, ""))
	h.feed(launchLines("b2", "t2", "tuT2")...)
	h.feed(lnAssistant("b3", "", cText("the watch expired; t2 re-checks the logs")))
	if act := h.close(t0.Add(3*time.Second), "the watch expired; t2 re-checks the logs"); act != bgReenterAfterSend {
		t.Errorf("action %v — a tool result naming mon1 read as the agent's stop of it", act)
	}
}

// The lead stops a subagent while it runs — resumed by its own watch's event:
// the stop aborts the subagent's loop, whose cleanup kills its own monitors,
// the watch included. The lead launches t2 with the stop: the watch ended
// because an agent stopped it, the wave is spared.
func TestBackground_ARunningOwnersStopCoversItsOwnWatch(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"),
		lnAssistant("b1", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("b2", "", cText("t1 in; tests running")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 in; tests running"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 2.5s: sm1's event resumes t1; the lead's test shell ends, its turn
	// starts, and the lead stops t1 while it runs.
	h.feedAt(t0.Add(2500*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1"), shell))
	h.feedAt(t0.Add(2550*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnSnapshotOf(),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1", "tool_use_id": "tuT1", "status": "killed", "summary": "stopped t1", "output_file": "", "session_id": "s1"}),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""))
	h.feed(launchLines("c2", "t2", "tuT2")...)
	h.feed(lnAssistant("c3", "", cText("tests pass; t1 stopped, t2 writes the notes")))
	if act := h.close(t0.Add(2700*time.Millisecond), "tests pass; t1 stopped, t2 writes the notes"); act != bgReenter {
		t.Fatalf("action %v: the lead stopped t1 while it ran, its watch died with it, and the wave launched with the stop was cut", act)
	}
}

// A subagent that parked once and runs again (the lead gave it a new job) is
// running at the lead's stop: its agents are not its cascade. t1 launches t1a,
// which arms sm2 and parks; t1 parks, the lead resumes it with a message, then
// stops it while it runs. sm2 survives, and later expires on its own past the
// budget: the turn its end prompts is cut.
func TestBackground_AStopOfAResumedSubagentLeavesItsAgentsWatchUnstopped(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch", "description": "sub-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1a", "sm2", "tuSub2", srcAgent("t1"), srcAgent("t1a"))...)
	h.feedAt(t0.Add(150*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")))
	// t1 finishes and parks; the lead reads its report.
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm2")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported")))
	if act := h.close(t0.Add(250*time.Millisecond), "t1 reported"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// The lead gives t1 a new job, then stops it while it runs.
	h.feedAt(t0.Add(300*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "t1", "message": "refactor the client"})),
		lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")),
		lnToolResult("tuMsg", "Message sent; agent resumed.", false, ""),
		lnAssistant("c2", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnSnapshotOf(srcMonitor("sm2")),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1", "tool_use_id": "tuT1", "status": "killed", "summary": "stopped t1", "output_file": "", "session_id": "s1"}),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""),
		lnAssistant("c3", "", cText("t1 stopped; its watcher keeps an eye on the build")))
	if act := h.close(t0.Add(400*time.Millisecond), "t1 stopped; its watcher keeps an eye on the build"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	// 2.8s: sm2 expires on its own; its notice resumes t1a, which reports.
	h.feedAt(t0.Add(2800*time.Millisecond), lnSnapshotOf(srcAgent("t1a")))
	h.feedAt(t0.Add(2900*time.Millisecond), lnSnapshotOf(), lnTaskNotif("t1a", "tuT1a"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2")...)
	h.feed(lnAssistant("d2", "", cText("the watch timed out; t2 re-runs CI")))
	if act := h.close(t0.Add(3*time.Second), "the watch timed out; t2 re-runs CI"); act != bgReenterAfterSend {
		t.Errorf("action %v: sm2 — alive after the stop of t1, which ran — expired on its own past the budget, read as stopped through t1's earlier park", act)
	}
}

// A message the lead sent a subagent while it ran is not a resume of its
// later park: t1, messaged at launch, arms a watch and finishes; the watch
// resumes t1 for 1.9s, then t1 reports again — the watch ran through that
// resume, and past the budget the close asks for the report.
func TestBackground_AnEarlierLeadMessageIsNoResumeOfALaterPark(t *testing.T) {
	h := newLifecycleAt(t, backgroundLifecycleConfig{wait: 2 * time.Second, autoTurnGrace: time.Hour, finalizeTimeout: time.Minute, idleSettle: time.Second}, 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a1m", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "t1", "message": "also check the flaky test"})),
		lnToolResult("tuMsg", "Message sent.", false, ""))
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 in; its watch runs")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 in; its watch runs"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 0.5s: the watch's event resumes t1, for 1.9s.
	h.feedAt(t0.Add(500*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm1")))
	h.feedAt(t0.Add(2400*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("c1", "", cText("t1 reports again")))
	if act := h.close(t0.Add(2500*time.Millisecond), "t1 reports again"); act != bgReenterAfterSend {
		t.Fatalf("action %v: a message sent to t1 before it parked took its watch's resume off the ceiling", act)
	}
}

// The stopped agent's state at the stop call is read from the set as it was
// then, whatever the order of the stop's result and the set's update: the
// same resumed t1 stopped while it runs, its result before the set drops it.
func TestBackground_AStopOfAResumedSubagentReadsTheSetAtTheCall(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "watch", "description": "sub-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1a", "sm2", "tuSub2", srcAgent("t1"), srcAgent("t1a"))...)
	h.feedAt(t0.Add(150*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")))
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm2")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported")))
	if act := h.close(t0.Add(250*time.Millisecond), "t1 reported"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(300*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "t1", "message": "refactor the client"})),
		lnSnapshotOf(srcAgent("t1"), srcMonitor("sm2")),
		lnToolResult("tuMsg", "Message sent; agent resumed.", false, ""),
		lnAssistant("c2", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "t1"})),
		lnToolResult("tuStop", `{"message":"Successfully stopped task: t1 (worker t1)","task_id":"t1","task_type":"local_agent","command":"worker t1"}`, false, ""),
		lnSnapshotOf(srcMonitor("sm2")),
		jsonLine(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "t1", "tool_use_id": "tuT1", "status": "killed", "summary": "stopped t1", "output_file": "", "session_id": "s1"}),
		lnAssistant("c3", "", cText("t1 stopped; its watcher keeps an eye on the build")))
	if act := h.close(t0.Add(400*time.Millisecond), "t1 stopped; its watcher keeps an eye on the build"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2800*time.Millisecond), lnSnapshotOf(srcAgent("t1a")))
	h.feedAt(t0.Add(2900*time.Millisecond), lnSnapshotOf(), lnTaskNotif("t1a", "tuT1a"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2")...)
	h.feed(lnAssistant("d2", "", cText("the watch timed out; t2 re-runs CI")))
	if act := h.close(t0.Add(3*time.Second), "the watch timed out; t2 re-runs CI"); act != bgReenterAfterSend {
		t.Errorf("action %v: sm2 — alive after the stop of t1, which ran — expired on its own past the budget, read as stopped through t1's earlier park", act)
	}
}
