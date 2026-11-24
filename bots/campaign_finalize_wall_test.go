package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCampaignFinalizeSaysWhyTheCampaignEnded: the edge that reaches finalize
// knows why the campaign ended, and the line under the handoff's title and the notice
// the closing review shows lead with it — a campaign whose wall left no room
// for one lot says so, instead of handing off a programme of zero lots with
// no cause (TestCampaignSaysWhyItStartsNoLot holds what each edge passes).
func TestCampaignFinalizeSaysWhyTheCampaignEnded(t *testing.T) {
	t.Parallel()
	const why = "NO LOT STARTED: the elapsed time (0 s) plus lot_room_s (54000 s, the time one lot may take) reaches 90% of the campaign wall (1620 s of 1800 s)"
	r := finalizeUnder(t, campaignClock{endedBy: why}, campaignSuitesPlan, map[string]string{"ci/test.sh": "#!/bin/sh\nexit 0\n"}, nil)
	if lines := strings.SplitN(r.handoff, "\n", 4); len(lines) < 3 || lines[2] != "Why the campaign ended: "+why {
		t.Fatalf("the handoff does not open on why the campaign ended:\n%s", r.handoff)
	}
	if n, _ := r.out["notice"].(string); !strings.HasPrefix(n, "ended: "+why+" — ") {
		t.Fatalf("notice = %q: the closing review must lead with why the campaign ended", n)
	}
}

// TestCampaignFinalizeWritesItsHandoffBeforeTheWall: finalize starts before
// 90% of the campaign's wall and is killed at the wall, and the commands it
// replays are the contract's — a blocked lot's gate, the suites, the oracle,
// each up to its gate_timeout_s. Each runs under its own wall or what the
// campaign's wall leaves before the handoff must be written (WRITE_S before
// it), whichever is shorter, so the handoff is written whatever they take;
// a command the campaign's wall stopped is reported as that, never as a red
// or a green on the tree. The clock below leaves 5 s to run commands in.
func TestCampaignFinalizeWritesItsHandoffBeforeTheWall(t *testing.T) {
	t.Parallel()
	const wall = 100000.0
	fiveSeconds := campaignClock{wall: wall, elapsed: wall - 900 - 5, endedBy: "programme exhausted: no eligible lot remains"}
	blocked := func(gate string) string {
		return "version: 1\ngate_timeout_s: 60\noracle:\n  refs_dir: .golden-master/refs\nlots:\n" +
			"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"" + gate + "\"\n"
	}
	timed := func(t *testing.T, clock campaignClock, plan string, files map[string]string) finalizeRun {
		t.Helper()
		started := time.Now()
		r := finalizeUnder(t, clock, plan, files, nil)
		if took := time.Since(started); took > 25*time.Second {
			t.Fatalf("finalize took %s: it waited on a command the campaign's wall had no room for", took)
		}
		return r
	}
	t.Run("a blocked lot's gate", func(t *testing.T) {
		t.Parallel()
		r := timed(t, fiveSeconds, blocked("sleep 30"), nil)
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "NOT JUDGED — `sleep 30`: the campaign's wall cut it after") ||
			!strings.Contains(l, "under its own wall of 60s") {
			t.Fatalf("L1 reads %q over a gate the campaign's wall cut", l)
		}
		if l := handoffLine(r.handoff, "NOT RUN"); !strings.Contains(l, "the campaign's wall left no time to start the oracle") {
			t.Fatalf("the oracle reads %q after the wall's room was spent:\n%s", l, r.handoff)
		}
		if subject := r.git("log", "-1", "--format=%s"); !strings.Contains(subject, "oracle not run") {
			t.Fatalf("commit subject = %q", subject)
		}
	})
	t.Run("the suites", func(t *testing.T) {
		t.Parallel()
		plan := strings.Replace(campaignSuitesPlan, "status: blocked", "status: done", 1)
		plan = strings.Replace(plan, "version: 1\n", "version: 1\ngate_timeout_s: 60\n", 1)
		r := timed(t, fiveSeconds, plan, map[string]string{"ci/test.sh": "#!/bin/sh\nexec sleep 30\n"})
		if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "NOT JUDGED: `sh ci/test.sh`: the campaign's wall cut it after") {
			t.Fatalf("the suites read %q", l)
		}
		if n, _ := r.out["notice"].(string); !strings.Contains(n, "suites not judged") {
			t.Fatalf("notice = %q", n)
		}
	})
	t.Run("a blocked lot whose gate is green, over cut suites", func(t *testing.T) {
		t.Parallel()
		plan := strings.Replace(campaignSuitesPlan, "version: 1\n", "version: 1\ngate_timeout_s: 60\n", 1)
		r := timed(t, fiveSeconds, plan, map[string]string{"ci/test.sh": "#!/bin/sh\nexec sleep 30\n"})
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "NOT JUDGED — its gate is green, the repository's suites were not judged") {
			t.Fatalf("L1 reads %q over suites the campaign's wall cut", l)
		}
	})
	t.Run("the oracle", func(t *testing.T) {
		t.Parallel()
		r := timed(t, fiveSeconds, strings.Replace(blocked("true"), "status: blocked", "status: done", 1),
			map[string]string{"verify.sh": "#!/bin/sh\nexec sleep 30\n"})
		if l := handoffLine(r.handoff, "NOT JUDGED"); !strings.Contains(l, "the campaign's wall cut the oracle after") ||
			!strings.Contains(l, "under its own wall of 60s") {
			t.Fatalf("the oracle reads %q:\n%s", l, r.handoff)
		}
		if green, _ := r.out["oracle_green"].(bool); green {
			t.Fatal("an oracle the campaign's wall cut reads green")
		}
	})
	t.Run("a run already past its wall", func(t *testing.T) {
		t.Parallel()
		r := timed(t, campaignClock{wall: wall, elapsed: wall + 60, endedBy: "x"}, blocked("true"), nil)
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "NOT JUDGED — `true`: the campaign's wall left no time to start it") {
			t.Fatalf("L1 reads %q on a run past its wall", l)
		}
	})
	t.Run("room enough", func(t *testing.T) {
		t.Parallel()
		r := timed(t, campaignClock{wall: wall, endedBy: "x"}, blocked("true"), nil)
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "REQUALIFIED") {
			t.Fatalf("L1 reads %q with the whole wall ahead", l)
		}
		if green, _ := r.out["oracle_green"].(bool); !green {
			t.Fatalf("the oracle did not run green with the whole wall ahead:\n%s", r.handoff)
		}
	})
}

// TestCampaignFinalizeMeasuresTheTreeOnceAfterACut: WRITE_S holds what
// finalize does after the command the campaign's wall cut — the tree measured
// again once, HEAD read, the handoff added and committed — at those calls'
// walls. When the wall cuts a blocked lot's gate, the suites behind it never
// start and leave the tree as the gates did: finalize measures it once, after
// the gates, not a second time for suites that did not run. Counted on the
// git calls the REAL finalize makes, through a git that logs them.
func TestCampaignFinalizeMeasuresTheTreeOnceAfterACut(t *testing.T) {
	t.Parallel()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "git-calls.log")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	const wall = 100000.0
	plan := strings.Replace(campaignSuitesPlan, "version: 1\n", "version: 1\ngate_timeout_s: 60\n", 1)
	plan = strings.Replace(plan, "      - \"true\"\n", "      - \"sleep 30\"\n", 1)
	r := finalizeUnderEnv(t, campaignClock{wall: wall, elapsed: wall - 900 - 5, endedBy: "x"}, plan,
		map[string]string{"ci/test.sh": "#!/bin/sh\nexit 0\n"}, nil,
		[]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")})
	if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "the campaign's wall cut it after") {
		t.Fatalf("L1 reads %q: the scenario needs the wall to cut its gate", l)
	}
	if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "left no time to start it") {
		t.Fatalf("the suites read %q: the scenario needs them never started", l)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	statuses := 0
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, " status --porcelain") {
			statuses++
		}
	}
	if statuses != 2 {
		t.Fatalf("finalize read the tree's status %d times — once before its commands, once after the cut gate; a measurement for suites that never started spends the handoff's own time:\n%s", statuses, raw)
	}
}
