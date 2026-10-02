//go:build !windows

package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// A lead resume addressed by NAME. 2.1.280's Agent tool takes a `name`
// ("Makes it addressable via SendMessage({to: name})"), the CLI lists agents by
// name to the model ("Other agents active in this session, addressable via
// SendMessage({to: name, message}): ..."), and SendMessage resolves the name
// through its agentNameRegistry (Rct: Hv(to) ?? agentNameRegistry.get(to), then
// a case-insensitive yr() match) to the agent's task id, then resumes it
// (agent-stopped -> Zmn). Same scenario as
// TestBackground_ALeadResumeOfAWatchsOwnerIsNoWatchResume, only `to` differs.
func msgLeadResumeBy(t *testing.T, to string) (*lifecycleAt, bgCloseAction) {
	const t1 = "a0123456789abcdef" // 2.1.280 agent id shape: ^a(?:[\w-]{1,63}-)?[0-9a-f]{16}$
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	// The lead launches t1 NAMED build-watcher.
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "worker", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1)),
		lnTaskStarted(t1, "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent(t1))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 1.0s: the lead gives t1 a new job, addressed as `to`. The CLI resumes t1:
	// the re-register emits task_started for t1 with the SendMessage's tool use
	// (W5 toolUseId: s.toolUseId — the runEngine's toolUseContext).
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": to, "message": "also fix the flaky login test"})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")),
		lnTaskStarted(t1, "tuMsg", true, false),
		lnToolResult("tuMsg", `{"success":true,"message":"Resuming agent build-watcher","resumedAgentId":"a0123456789abcdef"}`, false, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("t1 fixes the flaky test; the suite runs")))
	if act := h.close(t0.Add(1100*time.Millisecond), "t1 fixes the flaky test; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("the suite passes; t1 still at work")))
	return h, h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work")
}

func TestBackground_ALeadResumeByTaskIDIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgLeadResumeBy(t, "a0123456789abcdef")
	if act != bgReenter {
		t.Fatalf("control (to: task id): action %v, wrapUpSent=%v", act, h.l.wrapUpSent)
	}
}

func TestBackground_ALeadResumeByNameIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgLeadResumeBy(t, "build-watcher")
	if act != bgReenter {
		t.Fatalf("to: NAME: action %v (wrapUpSent=%v, sourceTime=%v): t1, at work on the lead's new job (its wave to 3.1s), cut by the ceiling of a watch whose events go to t1 — the lead resume keyed on the literal `to`", act, h.l.wrapUpSent, h.tr.sourceTime(time.Now()))
	}
}

func TestBackground_ALeadResumeByAnotherCaseIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgLeadResumeBy(t, "A0123456789ABCDEF")
	if act != bgReenter {
		t.Fatalf("to: id written with case/space (the CLI resolves Hv(yr(to))): action %v (wrapUpSent=%v)", act, h.l.wrapUpSent)
	}
}

// The watch's own resume of an agent the lead resumed earlier re-registers it
// under the same call (resumeAgentBackground's W5: toolUseId falls back to the
// task record's, the lead's SendMessage): it is the watch's, and it counts.
// t1 parks on sm1 at 0.2s; the lead resumes it BY NAME at 0.4s; t1 parks again
// at 0.6s; sm1's event resumes it at 0.8s (task_started under tuMsg again) for
// 1.8s; t1 reports at 2.6s. Source time: 0.2 + 2.1 = 2.3s past the 2s budget.
func TestBackground_AWatchResumeAfterALeadResumeCounts(t *testing.T) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "worker", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1)),
		lnTaskStarted(t1, "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent(t1))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(400*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also bump the version"})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")),
		lnTaskStarted(t1, "tuMsg", true, false),
		lnToolResult("tuMsg", `{"success":true,"message":"Resuming agent build-watcher","resumedAgentId":"a0123456789abcdef"}`, false, ""),
		lnAssistant("c2", "", cText("t1 bumps the version")))
	if act := h.close(t0.Add(500*time.Millisecond), "t1 bumps the version"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuMsg"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("version bumped")))
	if act := h.close(t0.Add(700*time.Millisecond), "version bumped"); act != bgReenter {
		t.Fatalf("close 4: action %v", act)
	}
	// 0.8s: sm1's event resumes t1 — the watch's resume, re-registered under
	// the task record's toolUseId (the lead's earlier SendMessage).
	h.feedAt(t0.Add(800*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStarted(t1, "tuMsg", true, false))
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuMsg"),
		lnInit("2.1.280"), lnAssistant("e1", "", cText("the build broke again")))
	if act := h.close(t0.Add(2700*time.Millisecond), "the build broke again"); act != bgReenterAfterSend {
		t.Fatalf("action %v: the watch's own resume of t1 (re-registered under the lead's earlier message) read as a lead resume — the watch ran 2.3s of the 2s budget uncharged", act)
	}
}

// A PEER resume moves the resumed agent's ownership (2.1.280 Wt:
// N=LW(caller); `{...a,ownerAgentId:N}`): t2 (running) messages the parked
// t1 by name, the CLI resumes t1 under t2's SendMessage call and t1 reports to
// t2 from then on while t2 runs (X9e: owner running -> owner). sm1's events
// drive t2, not the lead — yet the static chain (t1's launch parent: the lead)
// keeps sm1 the lead's source. t1 parks on sm1 at 0.2s (a lead source, 1.3s);
// t2 is launched at 1.5s (its wave to 3.5s) and asks t1 at 1.6s; a lead shell
// ends at 2.6s, its turn closes at 2.7s while t2 works.
func TestBackground_APeerResumeMovesTheWatchToThePeer(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "watcher", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1")),
		lnTaskStarted("t1", "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
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
	// 1.5s: a lead turn (an operator nudge) launches t2 and a test shell.
	h.feedAt(t0.Add(1500*time.Millisecond), lnInit("2.1.280"))
	h.feed(launchLines("c1", "t2", "tuT2", srcMonitor("sm1"))...)
	h.feed(lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("t2 fixes the build; the suite runs")))
	if act := h.close(t0.Add(1500*time.Millisecond), "t2 fixes the build; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	// 1.6s: t2 asks build-watcher; the CLI resumes t1 under t2's call; t1
	// answers t2 and parks again at 1.8s.
	h.feedAt(t0.Add(1600*time.Millisecond),
		lnAssistant("s2", "tuT2", cToolUse("tuPeer", "SendMessage", map[string]any{"to": "build-watcher", "message": "what broke?"})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2"), shell, srcAgent("t1")),
		lnTaskStarted("t1", "tuPeer", true, false),
		lnToolResult("tuPeer", `{"success":true,"message":"Resuming agent build-watcher"}`, false, "tuT2"))
	h.feedAt(t0.Add(1800*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2"), shell), lnTaskNotif("t1", "tuPeer"))
	// 2.6s: the lead's test shell ends; its turn closes while t2 works.
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("tests pass; t2 still at work")))
	act := h.close(t0.Add(2700*time.Millisecond), "tests pass; t2 still at work")
	if act != bgReenter {
		t.Fatalf("action %v (wrapUpSent=%v): t2 — at work, its wave to 3.5s — cut by the ceiling of a watch whose events go to t2 since t2 resumed its agent (1.3s of real lead-source time)", act, h.l.wrapUpSent)
	}
}

// The control of the peer resume: no peer message — sm1 is the lead's source from 0.2s (2.5s
// by 2.7s, past the 2s budget): the close asks, as designed.
func TestBackground_AWatchWithoutAPeerResumeStaysTheLeads(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "watcher", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1")),
		lnTaskStarted("t1", "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
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
	h.feedAt(t0.Add(1500*time.Millisecond), lnInit("2.1.280"))
	h.feed(launchLines("c1", "t2", "tuT2", srcMonitor("sm1"))...)
	h.feed(lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("t2 fixes the build; the suite runs")))
	if act := h.close(t0.Add(1500*time.Millisecond), "t2 fixes the build; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("tests pass; t2 still at work")))
	if act := h.close(t0.Add(2700*time.Millisecond), "tests pass; t2 still at work"); act != bgReenterAfterSend {
		t.Fatalf("control: action %v — sm1 ran 2.5s as the lead's source", act)
	}
}

// A subagent that messages the agent that launched it (its parent, parked on
// its watch) does not take it over: the CLI's owner chain from the resumer
// reaches the resumed agent (Wt's Ze). t1 launches t1a and parks on sm1 at
// 0.2s; t1a (running) asks t1 at 0.4s, t1 answers and parks at 0.5s — t1 stays
// the lead's, sm1 the lead's source: by 2.7s it ran 2.4s, past the 2s budget,
// and the close where the lead launches t2 asks.
func TestBackground_AChildsMessageToItsParentMovesNothing(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "fix", "description": "fixer", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"), srcAgent("t1a"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// t1a asks its parent t1; the CLI resumes t1 under t1a's call, t1 stays
	// the lead's (its report goes to main: X9e) and parks again.
	h.feedAt(t0.Add(400*time.Millisecond),
		lnAssistant("s2", "tuT1a", cToolUse("tuUp", "SendMessage", map[string]any{"to": "t1", "message": "which log?"})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a"), srcAgent("t1")),
		lnTaskStarted("t1", "tuUp", true, false),
		lnToolResult("tuUp", `{"success":true,"message":"Resuming agent t1"}`, false, "tuT1a"))
	h.feedAt(t0.Add(500*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a")), lnTaskNotif("t1", "tuUp"),
		lnInit("2.1.280"), lnAssistant("c1", "", cText("t1 answered t1a")))
	if act := h.close(t0.Add(600*time.Millisecond), "t1 answered t1a"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1a", "tuT1a"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2", srcMonitor("sm1"))...)
	h.feed(lnAssistant("d2", "", cText("t1a done; t2 ships it")))
	if act := h.close(t0.Add(2700*time.Millisecond), "t1a done; t2 ships it"); act != bgReenterAfterSend {
		t.Fatalf("action %v: t1 handed to t1a, the agent it launched, by t1a's message — sm1 (the lead's source for 2.2s) went uncharged while t1a ran", act)
	}
}

// A resume a SUBAGENT asks for is the resumed agent's own work too, even once
// that subagent is gone: t1 parks on sm1 at 0.2s; t2 asks t1 at 0.5s and ends
// at 0.6s; t1 works on t2's request until 2.6s, then reports — to the lead,
// t2 being gone (X9e) — and the lead launches t3. sm1 ran 0.3s + 0.1s as the
// lead's source: t3's wave must not be cut.
func TestBackground_APeersRequestIsTheAgentsOwnWork(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "watcher", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1")),
		lnTaskStarted("t1", "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
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
	h.feedAt(t0.Add(400*time.Millisecond), lnInit("2.1.280"))
	h.feed(launchLines("c1", "t2", "tuT2", srcMonitor("sm1"))...)
	h.feed(lnAssistant("c2", "", cText("t2 checks the release")))
	if act := h.close(t0.Add(400*time.Millisecond), "t2 checks the release"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(500*time.Millisecond),
		lnAssistant("s2", "tuT2", cToolUse("tuPeer", "SendMessage", map[string]any{"to": "build-watcher", "message": "bisect the flaky build"})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t2"), srcAgent("t1")),
		lnTaskStarted("t1", "tuPeer", true, false),
		lnToolResult("tuPeer", `{"success":true,"message":"Resuming agent build-watcher"}`, false, "tuT2"))
	h.feedAt(t0.Add(600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1")), lnTaskNotif("t2", "tuT2"),
		lnInit("2.1.280"), lnAssistant("d1", "", cText("t2 done; t1 bisects")))
	if act := h.close(t0.Add(700*time.Millisecond), "t2 done; t1 bisects"); act != bgReenter {
		t.Fatalf("close 4: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1", "tuPeer"), lnInit("2.1.280"))
	h.feed(launchLines("e1", "t3", "tuT3", srcMonitor("sm1"))...)
	h.feed(lnAssistant("e2", "", cText("bisected; t3 writes the fix")))
	if act := h.close(t0.Add(2700*time.Millisecond), "bisected; t3 writes the fix"); act != bgReenter {
		t.Fatalf("action %v: t1's work on t2's request (2s) charged to its watch — t3's wave cut", act)
	}
}

// Print mode (`--print`: Te() true) routes a finished agent's report to
// its owner only while that owner RUNS, to the main agent otherwise (X9e:
// `y=...&&!Te()||Rn(g)&&g.status==="running"; return y&&e?Yr(e):qe()`); the
// owner's own owner never enters into it. t1 (a long worker) launches t1a,
// which launches t1b; t1b arms sm3 and parks at 0.3s; t1a ends at 0.4s (its
// report to t1, running). From then on sm3's events resume t1b, whose report
// goes to the MAIN agent (t1a is not running): sm3 is the lead's source while
// t1 still runs. t1 reports at 1.9s; at 2.6s sm3's event (via t1b) prompts a
// lead turn that launches t2: sm3 ran 2.3s as the lead's source, past the 2s
// budget — the close must ask.
func TestBackground_ADeepWatchReportsToTheLeadPastAFinishedOwner(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "p", "description": "sub", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(150*time.Millisecond),
		lnAssistant("s1", "tuT1a", cToolUse("tuT1b", "Agent", map[string]any{"prompt": "watch", "description": "subsub", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b")),
		lnTaskStarted("t1b", "tuT1b", true, false),
		lnToolResult("tuT1b", "Async agent launched successfully.", false, "tuT1a"))
	h.feedAt(t0.Add(200*time.Millisecond), subMonitorLines("s2", "tuT1b", "sm3", "tuSub3", srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b"))...)
	h.feedAt(t0.Add(300*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcAgent("t1a"), srcMonitor("sm3")), lnTaskNotif("t1b", "tuT1b"))
	h.feedAt(t0.Add(400*time.Millisecond), lnSnapshotOf(srcAgent("t1"), srcMonitor("sm3")), lnTaskNotif("t1a", "tuT1a"))
	// 1.9s: t1 reports.
	h.feedAt(t0.Add(1900*time.Millisecond), lnSnapshotOf(srcMonitor("sm3")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported")))
	if act := h.close(t0.Add(1950*time.Millisecond), "t1 reported"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	// 2.6s: sm3's event resumes t1b, which reports to the lead; the lead
	// launches t2.
	h.feedAt(t0.Add(2500*time.Millisecond), lnSnapshotOf(srcAgent("t1b"), srcMonitor("sm3")))
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm3")), lnTaskNotif("t1b", "tuT1b"), lnInit("2.1.280"))
	h.feed(launchLines("c1", "t2", "tuT2", srcMonitor("sm3"))...)
	h.feed(lnAssistant("c2", "", cText("the build broke; t2 fixes it")))
	if act := h.close(t0.Add(2700*time.Millisecond), "the build broke; t2 fixes it"); act != bgReenterAfterSend {
		t.Fatalf("action %v (sourceTime charged at the close: see tracker): sm3 drove the lead through t1b from 0.4s — t1a gone, t1 running is not t1b's owner — and ran 2.3s as its source, yet the close did not ask", act)
	}
}

// The lead stops the parked t1 by a name the CLI resolves; the result
// names the task it stopped (t1). The cascade reaches t1a's watch sm2 through
// the task the RESULT names, not the name the call gave.
func TestBackground_AParkedAgentStoppedByNameCoversItsAgentsWatch(t *testing.T) {
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
	h.feedAt(t0.Add(300*time.Millisecond), lnSnapshotOf(srcMonitor("sm2")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 reported; its watcher keeps an eye on the build")))
	if act := h.close(t0.Add(400*time.Millisecond), "t1 reported; its watcher keeps an eye on the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(2500*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuStop", "TaskStop", map[string]any{"task_id": "build-watcher"})),
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
		t.Fatalf("action %v — sm2, killed with t1a by the lead's stop of the parked t1 (named build-watcher), read as ending on its own", act)
	}
}

// msgWatcherParked: the lead launches t1 NAMED build-watcher, t1 arms sm1 and
// parks at 0.2s (its report to the lead), close 2 at 0.3s. sm1 is the lead's
// source from 0.2s.
func msgWatcherParked(t *testing.T, h *lifecycleAt, t0 time.Time, t1 string) {
	t.Helper()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(lnAssistant("a1", "", cToolUse("tuT1", "Agent", map[string]any{"prompt": "p", "description": "worker", "name": "build-watcher", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1)),
		lnTaskStarted(t1, "tuT1", true, false),
		lnToolResult("tuT1", "Async agent launched successfully.", false, ""))
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent(t1))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
}

// A lead message QUEUED to t1 while t1 runs (2.1.280 SendMessage
// agent-live: fV pushes it to pendingMessages, result "Message queued for
// delivery to X at its next tool round."), t1 ends its run without another tool
// round: dtr emits agentStrandedMessages, the print wake router resumes t1 with
// the MAIN-LOOP context (OG, toolUseContext = rC()): W5 re-registers t1 under
// its record's toolUseId (tuT1), not under the SendMessage call. t1 then works
// on the lead's request 0.8s→3.1s. Same request, same agent as
// TestBackground_ALeadResumeByTaskIDIsTheAgentsOwnWork: only the moment the
// lead sent it differs (t1 running instead of parked).
func msgLeadMessageDuringAWatchRun(t *testing.T, direct bool) (*lifecycleAt, bgCloseAction) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	// 0.9s: sm1's event wakes t1 (the wake router: task_started under the
	// record's toolUseId) — the watch's own resume.
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	if direct {
		// Control: t1's watch run ends first (0.95s), the lead then messages
		// the parked t1: a direct resume under the SendMessage call.
		h.feedAt(t0.Add(950*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"))
		h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
			lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})),
			lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")),
			lnTaskStarted(t1, "tuMsg", true, false),
			lnToolResult("tuMsg", `{"success":true,"message":"Resuming agent build-watcher","resumedAgentId":"a0123456789abcdef"}`, false, ""),
			lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
			lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
			jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
				"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
			lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
			lnAssistant("c3", "", cText("t1 fixes the flaky test; the suite runs")))
		if act := h.close(t0.Add(1100*time.Millisecond), "t1 fixes the flaky test; the suite runs"); act != bgReenter {
			t.Fatalf("close 3 (direct): action %v", act)
		}
	} else {
		// 1.0s: a lead turn sends the message while t1 runs its watch run:
		// queued, not a resume.
		h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
			lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})),
			lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
			lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
			lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
			jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
				"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
			lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
			lnAssistant("c3", "", cText("asked t1 for the flaky test; the suite runs")))
		if act := h.close(t0.Add(1100*time.Millisecond), "asked t1 for the flaky test; the suite runs"); act != bgReenter {
			t.Fatalf("close 3 (queued): action %v", act)
		}
		// 1.2s: t1's watch run ends with the lead's message still pending:
		// its notification, then the stranded resume (wake router, main-loop
		// context) re-registers it under its record's toolUseId.
		h.feedAt(t0.Add(1200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), shell), lnTaskNotif(t1, "tuT1"),
			lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell), lnTaskStartedPrompt(t1, "tuT1", "also fix the flaky login test"),
			lnInit("2.1.280"), lnAssistant("d0", "", cText("t1's watch report noted")))
		if act := h.close(t0.Add(1300*time.Millisecond), "t1's watch report noted"); act != bgReenter {
			t.Fatalf("close 4 (queued): action %v", act)
		}
	}
	// 2.6s: the suite ends; its turn closes at 2.7s while t1 works on the
	// lead's request (its wave to ~2.9s/3.1s).
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("e1", "", cText("the suite passes; t1 still at work")))
	return h, h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work")
}

func TestBackground_ALeadResumeAfterAWatchRunIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgLeadMessageDuringAWatchRun(t, true)
	if act != bgReenter {
		t.Fatalf("a direct resume after the watch run: action %v (sourceTime %v)", act, h.tr.sourceTime(h.at))
	}
}

func TestBackground_AStrandedLeadMessageIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgLeadMessageDuringAWatchRun(t, false)
	if act != bgReenter {
		t.Fatalf("stranded message resume: action %v (sourceTime %v) — t1's work on the lead's request charged to its watch, its wave cut", act, h.tr.sourceTime(h.at))
	}
}

// lnTaskStartedPrompt: a subagent's re-registration with the prompt it was
// resumed with (2.1.280 task_started carries W5's prompt).
func lnTaskStartedPrompt(taskID, toolUseID, prompt string) string {
	return jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": taskID, "tool_use_id": toolUseID,
		"description": "task " + taskID, "task_type": "local_agent", "is_backgrounded": true, "prompt": prompt, "session_id": "s1"})
}

// watchWakePrompt: the wake router's prompt for a parked agent — its queued
// task-notifications joined (a Monitor event: Ci + D).
func watchWakePrompt(monitorID, event string) string {
	return "<task-notification>\n<task-id>" + monitorID + "</task-id>\n<summary>Monitor event: \"build log\"</summary>\n<event>" + event + "</event>\n</task-notification>"
}

// lnToolResultBlocks: a tool result whose content is one text block (2.1.280
// SendMessage's mapToolResultToToolResultBlockParam).
func lnToolResultBlocks(toolUseID, text, parent string) string {
	m := map[string]any{"type": "user", "session_id": "s1", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": toolUseID,
			"content": []any{map[string]any{"type": "text", "text": text}}, "is_error": false}}}}
	if parent != "" {
		m["parent_tool_use_id"] = parent
	}
	return jsonLine(m)
}

// The lead's message QUEUED while t1 runs its watch run, taken at its
// next tool round: t1 works on it until past 2.7s without parking. No
// re-registration at all.
func TestBackground_ALeadMessageQueuedForARunningAgentIsItsOwnWork(t *testing.T) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})),
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("asked t1 for the flaky test; the suite runs")))
	if act := h.close(t0.Add(1100*time.Millisecond), "asked t1 for the flaky test; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("e1", "", cText("the suite passes; t1 still at work")))
	act := h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work")
	if act != bgReenter {
		t.Fatalf("queued message taken in t1's run: action %v (sourceTime %v) — t1's work on the lead's request charged to its watch, its wave cut", act, h.tr.sourceTime(h.at))
	}
}

// The watch's own wake still counts: a re-registration whose prompt is its
// task-notifications. t1 parks at 0.2s, sm1 wakes it at 0.9s, it works on
// the event until past 2.7s: sm1 ran 2.5s as the lead's source.
func TestBackground_AWatchWakeOfAParkedAgentStillCounts(t *testing.T) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.280"),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("the suite runs")))
	if act := h.close(t0.Add(1100*time.Millisecond), "the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.280"), lnAssistant("e1", "", cText("the suite passes; t1 still at work")))
	if act := h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work"); act != bgReenterAfterSend {
		t.Fatalf("control: the watch's own wake must count: action %v (sourceTime %v)", act, h.tr.sourceTime(h.at))
	}
}

// A GRANDCHILD's message to its grandparent moves nothing either — the
// CLI's owner chain from the resumer reaches the resumed agent two levels up.
// t1 (the lead's) launches t1a, which launches t1b; t1 arms sm1 and parks at
// 0.2s; t1b asks t1 at 0.4s; t1 answers and parks at 0.5s; t1a and t1b end
// at 2.6s and the lead launches t2: sm1 was the lead's source 0.2s + 2.1s.
func TestBackground_AGrandchildsMessageToItsGrandparentMovesNothing(t *testing.T) {
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	h.feedAt(t0, lnInit("2.1.280"))
	h.feed(launchLines("a1", "t1", "tuT1")...)
	h.feed(lnAssistant("a2", "", cText("WAITING")))
	if act := h.close(t0, "WAITING"); act != bgReenter {
		t.Fatalf("close 1: action %v", act)
	}
	h.feedAt(t0.Add(50*time.Millisecond),
		lnAssistant("s0", "tuT1", cToolUse("tuT1a", "Agent", map[string]any{"prompt": "fix", "description": "fixer", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a")),
		lnTaskStarted("t1a", "tuT1a", true, false),
		lnToolResult("tuT1a", "Async agent launched successfully.", false, "tuT1"))
	h.feedAt(t0.Add(60*time.Millisecond),
		lnAssistant("s0b", "tuT1a", cToolUse("tuT1b", "Agent", map[string]any{"prompt": "dig", "description": "digger", "run_in_background": true})),
		lnSnapshotOf(srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b")),
		lnTaskStarted("t1b", "tuT1b", true, false),
		lnToolResult("tuT1b", "Async agent launched successfully.", false, "tuT1a"))
	h.feedAt(t0.Add(100*time.Millisecond), subMonitorLines("s1", "tuT1", "sm1", "tuSub", srcAgent("t1"), srcAgent("t1a"), srcAgent("t1b"))...)
	h.feedAt(t0.Add(200*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a"), srcAgent("t1b")), lnTaskNotif("t1", "tuT1"),
		lnInit("2.1.280"), lnAssistant("b1", "", cText("t1 is watching the build")))
	if act := h.close(t0.Add(300*time.Millisecond), "t1 is watching the build"); act != bgReenter {
		t.Fatalf("close 2: action %v", act)
	}
	h.feedAt(t0.Add(400*time.Millisecond),
		lnAssistant("s2", "tuT1b", cToolUse("tuUp", "SendMessage", map[string]any{"to": "t1", "message": "which log?"})),
		lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a"), srcAgent("t1b"), srcAgent("t1")),
		lnTaskStarted("t1", "tuUp", true, false),
		lnToolResult("tuUp", `{"success":true,"message":"Resuming agent t1"}`, false, "tuT1b"))
	h.feedAt(t0.Add(500*time.Millisecond), lnSnapshotOf(srcMonitor("sm1"), srcAgent("t1a"), srcAgent("t1b")), lnTaskNotif("t1", "tuUp"),
		lnInit("2.1.280"), lnAssistant("c1", "", cText("t1 answered t1b")))
	if act := h.close(t0.Add(600*time.Millisecond), "t1 answered t1b"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif("t1b", "tuT1b"), lnTaskNotif("t1a", "tuT1a"), lnInit("2.1.280"))
	h.feed(launchLines("d1", "t2", "tuT2", srcMonitor("sm1"))...)
	h.feed(lnAssistant("d2", "", cText("t1a done; t2 ships it")))
	if act := h.close(t0.Add(2700*time.Millisecond), "t1a done; t2 ships it"); act != bgReenterAfterSend {
		t.Fatalf("action %v: t1 handed to t1b, an agent below it, by t1b's message — sm1 (the lead's source for 2.3s) went uncharged while t1b ran", act)
	}
}

// msgAlreadyWaking: t1 (named build-watcher) arms sm1 and parks at 0.2s. At 0.9s
// sm1's event makes the wake router resume t1: its record is `resuming`, its
// status still `completed` — out of the CLI's set (lm(): running|pending) until
// its registration. At 1.0s a lead turn messages build-watcher: SendMessage
// resolves agent-stopped, Avn meets the in-flight resume ($q), AK queues the
// message: "build-watcher is already waking; message queued for its next tool
// round (...)" with the pin. The registration (snapshot + task_started under
// tuT1 with the watch's notifications as prompt) lands at 1.15s (resultFirst)
// or before the lead's turn (control). t1 takes the lead's message at its
// first tool round and works on it until past 2.7s.
func msgAlreadyWaking(t *testing.T, resultFirst bool) (*lifecycleAt, bgCloseAction) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	reg := []string{lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED"))}
	if !resultFirst {
		h.feedAt(t0.Add(900*time.Millisecond), reg...)
	}
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.282"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})),
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"build-watcher is already waking; message queued for its next tool round (not delivered if the agent turns out to have been stopped).","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(append(func() []map[string]any {
			if resultFirst {
				return []map[string]any{srcMonitor("sm1")}
			}
			return []map[string]any{srcAgent(t1), srcMonitor("sm1")}
		}(), shell)...),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("asked t1 for the flaky test; the suite runs")))
	if act := h.close(t0.Add(1100*time.Millisecond), "asked t1 for the flaky test; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	if resultFirst {
		h.feedAt(t0.Add(1150*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
			lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.282"), lnAssistant("e1", "", cText("the suite passes; t1 still at work")))
	return h, h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 still at work")
}

// msgStaleQueuedResult: the lead's SendMessage to t1 is queued while t1 runs its
// watch wake W1 (agent-live); a PostToolUse hook delays the tool_result (the
// CLI emits it once its hooks answered — live: "Slow PostToolUse hooks:
// 8004ms for SendMessage", the result landing after the recipient took the
// message, finished and parked). t1 takes the message in W1, parks at 1.3s;
// sm1 wakes it again at 1.5s (W2, the watch's own run); the result lands at
// 1.6s with t1 in the set. W2 is the watch's.
func msgStaleQueuedResult(t *testing.T) (*lifecycleAt, bgCloseAction) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.282"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})))
	h.feedAt(t0.Add(1300*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"))
	h.feedAt(t0.Add(1500*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED AGAIN")))
	h.feedAt(t0.Add(1600*time.Millisecond),
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("asked t1; the suite runs")))
	if act := h.close(t0.Add(1700*time.Millisecond), "asked t1; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.282"), lnAssistant("e1", "", cText("the suite passes; t1 on the watch's second event")))
	return h, h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 on the watch's second event")
}

// A lead message the CLI queues for an agent already being woken (a watch's
// resume in flight: "X is already waking; message queued for its next tool
// round") is taken by the run the wake starts — the agent's own work, in
// either order of the result and the agent's re-registration (live-proven on
// 2.1.282: the result can land first, while the agent is out of the set).
func TestBackground_AnAlreadyWakingAgentsQueuedMessageIsItsOwnWork(t *testing.T) {
	for _, resultFirst := range []bool{true, false} {
		h, act := msgAlreadyWaking(t, resultFirst)
		if act != bgReenter {
			t.Errorf("result first %v: action %v (sourceTime %v) — t1's work on the lead's request charged to its watch", resultFirst, act, h.tr.sourceTime(h.at))
		}
	}
}

// A queued message's result that lands after the agent took the message,
// parked and was woken again by its watch (a slow PostToolUse hook delays
// it — live-proven) belongs to the run that is over: the watch's next run
// is the watch's.
func TestBackground_AStaleQueuedResultDoesNotAbsolveTheNextWatchRun(t *testing.T) {
	h, act := msgStaleQueuedResult(t)
	if act != bgReenterAfterSend {
		t.Fatalf("action %v (sourceTime %v) — the watch's own second run read as the lead's message work: the watch escapes the ceiling", act, h.tr.sourceTime(h.at))
	}
}

// A queued message's work lasts until the agent parks: an agent that never
// parks after one is still bounded, by its wave's budget (it is held work). t1 parks at 0.2s, sm1 wakes it at 0.9s,
// the lead's message is queued at 1.0s (msgResumed), t1 then never parks;
// the lead's turns keep closing.
func TestBackground_AnAgentThatNeverParksAfterAQueuedMessageIsBoundByItsWave(t *testing.T) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.282"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "keep going"})),
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c3", "", cText("t1 keeps going")))
	var acts []bgCloseAction
	for i, at := range []time.Duration{1100, 2000, 2900, 3300, 4000} {
		if i > 0 {
			h.feedAt(t0.Add(at*time.Millisecond-50*time.Millisecond), lnInit("2.1.282"), lnAssistant("x", "", cText("tick")))
		}
		act := h.close(t0.Add(at*time.Millisecond), "tick")
		acts = append(acts, act)
		if act == bgReenterAfterSend {
			break
		}
	}
	if acts[len(acts)-1] != bgReenterAfterSend {
		t.Fatalf("never cut: %v", acts)
	}
}

// msgSendRunsAfterARewake: the round-17 stale witness's stream, with the truth reversed.
// t1 (build-watcher) runs W1 (sm1's wake, 0.9s). At 1.0s a lead turn streams
// ONE assistant message [Bash (a 0.6s build, foreground), SendMessage(to
// build-watcher)]: both tool_use lines at once, SendMessage waiting for the
// Bash (live: f2). t1 ends W1 and parks at 1.3s; sm1 wakes it again at 1.5s
// (W2). The Bash ends at 1.6s, SendMessage runs: t1 runs W2, the message is
// queued for W2's next tool round — W2 takes it and works on it past 2.7s.
// stamp: the fix's fact (the PostToolUse hook of the call, right before its
// result).
func msgSendRunsAfterARewake(t *testing.T, stamp bool) (*lifecycleAt, bgCloseAction) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.282"),
		lnAssistant("c1", "", cToolUse("tuBuild", "Bash", map[string]any{"command": "make build"})),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})))
	h.feedAt(t0.Add(1300*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"))
	h.feedAt(t0.Add(1500*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED AGAIN")))
	h.feedAt(t0.Add(1600*time.Millisecond), lnToolResult("tuBuild", "built", false, ""))
	if stamp {
		id := "tuMsg"
		if _, err := stampSendExecution(h.tr)(context.Background(), claudesdk.HookCallbackInput{ToolName: "SendMessage", ToolUseID: &id}); err != nil {
			t.Fatal(err)
		}
	}
	h.feed(
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("built; asked t1; the suite runs")))
	if act := h.close(t0.Add(1700*time.Millisecond), "built; asked t1; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.282"), lnAssistant("e1", "", cText("the suite passes; t1 on the lead's request")))
	return h, h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 on the lead's request")
}

// A SendMessage the CLI runs after the calls before it in its message — its
// tool_use streamed long before — while its recipient parked and was woken
// again: the new run takes the message, the agent's own work. Where the call
// ran is its PostToolUse hook (live-proven on 2.1.282: the hook fired after
// the re-wake, ahead of the result).
func TestBackground_AMessageRunAfterARewakeIsTheAgentsOwnWork(t *testing.T) {
	h, act := msgSendRunsAfterARewake(t, true)
	if act != bgReenter {
		t.Fatalf("action %v (sourceTime %v) — the run that took the lead's message (sent after the re-wake) charged to the watch: the wave is cut", act, h.tr.sourceTime(h.at))
	}
}

// A SendMessage that ran before its recipient parked — its result alone
// late, behind a slow hook — leaves the watch's next run the watch's, stamped
// where it really ran.
func TestBackground_AStaleResultStampedWhereTheCallRanStaysTheWatchs(t *testing.T) {
	const t1 = "a0123456789abcdef"
	h := newLifecycleAt(t, refusedStopCfg(), 0)
	t0 := time.Now()
	shell := map[string]any{"task_id": "b1", "task_type": "local_bash", "description": "npm test"}
	msgWatcherParked(t, h, t0, t1)
	h.feedAt(t0.Add(900*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED")))
	h.feedAt(t0.Add(1000*time.Millisecond), lnInit("2.1.282"),
		lnAssistant("c1", "", cToolUse("tuMsg", "SendMessage", map[string]any{"to": "build-watcher", "message": "also fix the flaky login test"})))
	id := "tuMsg"
	if _, err := stampSendExecution(h.tr)(context.Background(), claudesdk.HookCallbackInput{ToolName: "SendMessage", ToolUseID: &id}); err != nil {
		t.Fatal(err)
	}
	h.feedAt(t0.Add(1300*time.Millisecond), lnSnapshotOf(srcMonitor("sm1")), lnTaskNotif(t1, "tuT1"))
	h.feedAt(t0.Add(1500*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskStartedPrompt(t1, "tuT1", watchWakePrompt("sm1", "BUILD FAILED AGAIN")))
	h.feedAt(t0.Add(1600*time.Millisecond),
		lnToolResultBlocks("tuMsg", `{"success":true,"message":"Message queued for delivery to build-watcher at its next tool round.","pin":{"id":"a0123456789abcdef","name":"build-watcher","ref":"99ebe6"}}`, ""),
		lnAssistant("c2", "", cToolUse("tuB1", "Bash", map[string]any{"command": "npm test", "run_in_background": true})),
		lnSnapshotOf(srcAgent(t1), srcMonitor("sm1"), shell),
		jsonLine(map[string]any{"type": "system", "subtype": "task_started", "task_id": "b1", "tool_use_id": "tuB1",
			"description": "npm test", "task_type": "local_bash", "is_backgrounded": true, "session_id": "s1"}),
		lnToolResult("tuB1", "Command running in background with ID: b1", false, ""),
		lnAssistant("c3", "", cText("asked t1; the suite runs")))
	if act := h.close(t0.Add(1700*time.Millisecond), "asked t1; the suite runs"); act != bgReenter {
		t.Fatalf("close 3: action %v", act)
	}
	h.feedAt(t0.Add(2600*time.Millisecond), lnSnapshotOf(srcAgent(t1), srcMonitor("sm1")), lnTaskNotif("b1", "tuB1"),
		lnInit("2.1.282"), lnAssistant("e1", "", cText("the suite passes; t1 on the watch's second event")))
	if act := h.close(t0.Add(2700*time.Millisecond), "the suite passes; t1 on the watch's second event"); act != bgReenterAfterSend {
		t.Fatalf("action %v (sourceTime %v) — the watch's own second run read as the lead's message work", act, h.tr.sourceTime(h.at))
	}
}
