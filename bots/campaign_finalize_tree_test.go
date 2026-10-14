package bots

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCampaignFinalizeReadsEachRunAgainstItsLotsWall: a command two blocked
// lots share runs once, under the longer of their walls, and each lot reads the
// run against its own — a run longer than a lot's wall is that lot's
// GATE_TIMEOUT in lot_verify, so it is not that lot's green here.
func TestCampaignFinalizeReadsEachRunAgainstItsLotsWall(t *testing.T) {
	const head = "version: 1\ngate_timeout_s: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"
	t.Run("a gate two lots share", func(t *testing.T) {
		r := finalizeOn(t, head+
			"  - id: A\n    title: a\n    status: blocked\n    exit_gate:\n      - \"sleep 3\"\n"+
			"  - id: B\n    title: b\n    status: blocked\n    gate_timeout_s: 10\n    exit_gate:\n      - \"sleep 3\"\n", nil, nil)
		if a := handoffLine(r.handoff, "**A**"); !strings.Contains(a, "still blocked") || !strings.Contains(a, "past this lot's wall of 1s") {
			t.Fatalf("A (wall 1 s) reads %q over a 3 s gate", a)
		}
		if b := handoffLine(r.handoff, "**B**"); !strings.Contains(b, "REQUALIFIED") {
			t.Fatalf("B (wall 10 s) reads %q over a 3 s gate", b)
		}
	})
	t.Run("the suites, under a wall a lot that never ran gave", func(t *testing.T) {
		r := finalizeOn(t, "version: 1\ngate_timeout_s: 1\nsuites:\n  - \"sleep 3\"\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
			"  - id: A\n    title: a\n    status: blocked\n    exit_gate:\n      - \"true\"\n"+
			"  - id: Z\n    title: never reached\n    status: todo\n    gate_timeout_s: 10\n    exit_gate:\n      - \"true\"\n", nil, nil)
		if a := handoffLine(r.handoff, "**A**"); !strings.Contains(a, "the repository's suites are not") || !strings.Contains(a, "past this lot's wall of 1s") {
			t.Fatalf("A (wall 1 s) reads %q over a 3 s suite", a)
		}
	})
}

// TestCampaignFinalizeJudgesOnlyTheCommittedTree: the final tree is HEAD. A
// working tree that differs from it — behind an index flag, behind a status
// setting, behind a replace ref — is reported as not judged, the plan is read
// as committed, and what the oracle writes is not what the suites judge.
func TestCampaignFinalizeJudgesOnlyTheCommittedTree(t *testing.T) {
	red := map[string]string{"ci/test.sh": "#!/bin/sh\n[ -f ci/local.sh ] && exit 0\nexit 1\n"}
	notRequalified := func(t *testing.T, r finalizeRun) {
		t.Helper()
		if l := handoffLine(r.handoff, "**L1**"); strings.Contains(l, "REQUALIFIED") {
			t.Fatalf("L1 REQUALIFIED while HEAD's suite exits 1: %s\n%s", l, handoffLine(r.handoff, "suites on the final tree"))
		}
	}
	for name, after := range map[string]func(ws string, git func(...string) string){
		"an edit behind skip-worktree": func(ws string, git func(...string) string) {
			git("update-index", "--skip-worktree", "ci/test.sh")
			writeContract(t, ws, "ci/test.sh", "#!/bin/sh\nexit 0\n")
		},
		"the plan edited behind assume-unchanged": func(ws string, git func(...string) string) {
			git("update-index", "--assume-unchanged", ".modernize/plan.yaml")
			writeContract(t, ws, ".modernize/plan.yaml", strings.Replace(campaignSuitesPlan, `"sh ci/test.sh"`, `"true"`, 1))
		},
		"an untracked file status is told not to show": func(ws string, git func(...string) string) {
			git("config", "status.showUntrackedFiles", "no")
			writeContract(t, ws, "ci/local.sh", "# never committed\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := finalizeOn(t, campaignSuitesPlan, red, after)
			notRequalified(t, r)
			if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "NOT JUDGED") {
				t.Fatalf("the suites read %q over a working tree that is not HEAD", l)
			}
		})
	}
	t.Run("a replace ref", func(t *testing.T) {
		r := finalizeOn(t, campaignSuitesPlan, red, func(ws string, git func(...string) string) {
			real := git("rev-parse", "HEAD")
			writeContract(t, ws, "ci/test.sh", "#!/bin/sh\nexit 0\n")
			git("add", "ci/test.sh")
			git("replace", real, git("commit-tree", git("write-tree"), "-p", git("rev-parse", "HEAD^"), "-m", "fake"))
			git("checkout", "--", ".")
			git("reset", "-q")
		})
		notRequalified(t, r)
	})
	t.Run("the oracle rewrites the suite's script", func(t *testing.T) {
		r := finalizeOn(t, campaignSuitesPlan, map[string]string{
			"ci/test.sh": "#!/bin/sh\nexit 1\n",
			"verify.sh":  "#!/bin/sh\nprintf '#!/bin/sh\\nexit 0\\n' > ci/test.sh\necho '{\"mode\":\"gate\"}'\nexit 0\n",
		}, nil)
		notRequalified(t, r)
		if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "RED") {
			t.Fatalf("the suites read %q: they must judge HEAD's script, before the oracle runs", l)
		}
	})
}

// TestCampaignFinalizeWritesAHandoffWhateverThePlanHolds: a plan yq reads (exit
// 0) in a shape no reader takes is NOT READ, and a gate no shell can run is not
// requalifiable — the handoff is written either way.
func TestCampaignFinalizeWritesAHandoffWhateverThePlanHolds(t *testing.T) {
	for name, plan := range map[string]string{
		"a trailing document separator": campaignSuitesPlan + "---\n",
		"a second document":             campaignSuitesPlan + "---\nreviewed: true\n",
		"lots as a mapping":             "version: 1\nsuites: \"true\"\nlots:\n  L1:\n    status: blocked\n",
		"a lot that is not a mapping":   "version: 1\nsuites: \"true\"\nlots:\n  - L1\n",
		"a sequence at its top":         "- id: L1\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := finalizeOn(t, plan, map[string]string{"ci/test.sh": "#!/bin/sh\nexit 0\n"}, nil)
			if !strings.Contains(r.handoff, "Programme: NOT READ") {
				t.Fatalf("the handoff must say the plan was not read:\n%s", r.handoff)
			}
		})
	}
	t.Run("a NUL byte in a blocked lot's gate", func(t *testing.T) {
		r := finalizeOn(t, "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"true\\0\"\n", nil, nil)
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "NUL byte — not requalifiable") {
			t.Fatalf("L1 reads %q", l)
		}
	})
}

// TestCampaignFinalizeSummarySaysWhatTheSuitesGave: the notice the closing review
// shows and the handoff's commit subject carry the suites' verdict and an
// oracle that did not run, not only the oracle's colour.
func TestCampaignFinalizeSummarySaysWhatTheSuitesGave(t *testing.T) {
	t.Run("red suites", func(t *testing.T) {
		r := finalizeOn(t, strings.Replace(campaignSuitesPlan, "status: blocked", "status: done", 1),
			map[string]string{"ci/test.sh": "#!/bin/sh\nexit 1\n"}, nil)
		if n, _ := r.out["notice"].(string); !strings.Contains(n, "suites red") {
			t.Fatalf("notice = %q", n)
		}
		if subject := r.git("log", "-1", "--format=%s"); !strings.Contains(subject, "suites red") {
			t.Fatalf("commit subject = %q", subject)
		}
	})
	t.Run("an oracle that did not run", func(t *testing.T) {
		r := finalizeOn(t, "version: 1\ngate_timeout_s: soon\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
			"  - id: L1\n    title: t\n    status: done\n    exit_gate:\n      - \"true\"\n", nil, nil)
		if subject := r.git("log", "-1", "--format=%s"); !strings.Contains(subject, "oracle not run") {
			t.Fatalf("commit subject = %q", subject)
		}
	})
}

// TestCampaignFinalizeReportsTheRunOfARepeatedSuite: a command declared twice
// runs as declared, and the report names the run that was red, with its own
// exit code.
func TestCampaignFinalizeReportsTheRunOfARepeatedSuite(t *testing.T) {
	plan := strings.Replace(campaignSuitesPlan, "  - \"sh ci/test.sh\"\n", "  - \"sh ci/test.sh\"\n  - \"sh ci/test.sh\"\n", 1)
	// Red on its first run, green on its second.
	flaky := "#!/bin/sh\nif [ -e .git/second ]; then exit 0; fi\ntouch .git/second\nexit 1\n"
	r := finalizeOn(t, plan, map[string]string{"ci/test.sh": flaky}, nil)
	if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "RED — `sh ci/test.sh` exited 1") {
		t.Fatalf("the suites read %q", l)
	}
}

// TestCampaignFinalizeCommitsOnlyItsOwnFiles: what a child run left in the index
// is not the supervisor's to land under "campaign: handoff".
func TestCampaignFinalizeCommitsOnlyItsOwnFiles(t *testing.T) {
	r := finalizeOn(t, campaignSuitesPlan, map[string]string{"ci/test.sh": "#!/bin/sh\nexit 1\n"},
		func(ws string, git func(...string) string) {
			writeContract(t, ws, "src/worker.txt", "work in flight\n")
			git("add", "src/worker.txt")
		})
	if took := r.git("show", "--name-only", "--format=", "HEAD"); strings.Contains(took, "src/worker.txt") {
		t.Fatalf("the handoff commit took the worker's staged file: %q", took)
	}
	if staged := r.git("diff", "--cached", "--name-only"); !strings.Contains(staged, "src/worker.txt") {
		t.Fatalf("the worker's staged file is no longer staged: %q", staged)
	}
}

// TestCampaignFinalizeKillsWhatACommandStarted: a command past its wall is
// killed with everything it started — nothing writes to the tree after the
// handoff is written.
func TestCampaignFinalizeKillsWhatACommandStarted(t *testing.T) {
	late := filepath.Join(t.TempDir(), "late")
	gate := "sh -c '(sleep 2; touch " + late + ") & wait'"
	r := finalizeOn(t, "version: 1\ngate_timeout_s: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
		"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - "+strconv.Quote(gate)+"\n", nil, nil)
	if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "still blocked") {
		t.Fatalf("L1 reads %q over a gate past its wall", l)
	}
	time.Sleep(3 * time.Second)
	if pathExists(late) {
		t.Fatal("a process the gate started wrote after finalize returned")
	}
}

// TestCampaignFinalizeRefusesWhatTheCommandsLeftBehind, suites or no suites:
// the tree is measured after every command that ran — a blocked lot does not
// requalify over a tree its own gate polluted, a green suite that writes or
// commits is not a green verdict, and the oracle does not judge what moved
// under it.
func TestCampaignFinalizeRefusesWhatTheCommandsLeftBehind(t *testing.T) {
	t.Run("a gate that writes, without suites declared", func(t *testing.T) {
		r := finalizeOn(t, "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
			"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"sh -c 'echo wrote > leftover.txt'\"\n", nil, nil)
		if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "not requalifiable") {
			t.Fatalf("L1 reads %q", l)
		}
		if l := handoffLine(r.handoff, "NOT JUDGED"); !strings.Contains(l, "left a tree no landing carries") {
			t.Fatalf("the oracle line reads %q", l)
		}
	})
	t.Run("a green suite that writes a report", func(t *testing.T) {
		r := finalizeOn(t, campaignSuitesPlan, map[string]string{
			"ci/test.sh": "#!/bin/sh\necho report > suite-report.txt\nexit 0\n"}, nil)
		if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "NOT JUDGED: the suites left a tree no landing carries (suite-report.txt)") {
			t.Fatalf("the suites read %q", l)
		}
		if l := handoffLine(r.handoff, "**L1**"); strings.Contains(l, "REQUALIFIED") {
			t.Fatalf("L1 reads %q", l)
		}
	})
	t.Run("a green suite that commits", func(t *testing.T) {
		r := finalizeOn(t, campaignSuitesPlan, map[string]string{
			"ci/test.sh": "#!/bin/sh\necho x >> suite.txt && git add -A && git commit -qm 'suite commit'\n"}, nil)
		if l := handoffLine(r.handoff, "suites on the final tree"); !strings.Contains(l, "NOT JUDGED: the suites moved HEAD.") {
			t.Fatalf("the suites read %q", l)
		}
		if l := handoffLine(r.handoff, "**L1**"); strings.Contains(l, "REQUALIFIED") {
			t.Fatalf("L1 reads %q", l)
		}
	})
}

// TestCampaignFinalizeHoldsIdsToBeKeys: the requalification verdicts, the
// steward's plan and the child runs' only_lot all read through a lot's id — a
// plan whose ids are not non-empty unique strings is NOT READ, and never a
// verdict keyed by something else.
func TestCampaignFinalizeHoldsIdsToBeKeys(t *testing.T) {
	for name, plan := range map[string]string{
		"a non-scalar id": "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n" +
			"  - id: [\"L1\"]\n    title: t\n    status: blocked\n    exit_gate:\n      - \"true\"\n",
		"duplicate ids": "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n" +
			"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"false\"\n" +
			"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"true\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := finalizeOn(t, plan, nil, nil)
			if !strings.Contains(r.handoff, "Programme: NOT READ") {
				t.Fatalf("the handoff must say the plan was not read:\n%s", r.handoff)
			}
		})
	}
}

// TestCampaignFinalizeSubjectForNoSuites: a contract with no suites reads as a
// word in the subject and the notice, not as a sentence fragment.
func TestCampaignFinalizeSubjectForNoSuites(t *testing.T) {
	r := finalizeOn(t, "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
		"  - id: L1\n    title: t\n    status: done\n    exit_gate:\n      - \"true\"\n", nil, nil)
	if subject := r.git("log", "-1", "--format=%s"); !strings.HasSuffix(strings.TrimSpace(subject), "suites none") {
		t.Fatalf("commit subject = %q, want it to end in \"suites none\"", subject)
	}
}

// TestCampaignFinalizeReadsARunPastItsLotsWallByMillis: a run the wall would
// have killed is past the lot's wall, whatever the tenth of a second says —
// lot_verify times the same command out.
func TestCampaignFinalizeReadsARunPastItsLotsWallByMillis(t *testing.T) {
	r := finalizeOn(t, "version: 1\ngate_timeout_s: 1\nsuites:\n  - \"sleep 1.01\"\noracle:\n  refs_dir: .golden-master/refs\nlots:\n"+
		"  - id: L1\n    title: t\n    status: blocked\n    exit_gate:\n      - \"true\"\n"+
		"  - id: Z\n    title: never reached\n    status: todo\n    gate_timeout_s: 10\n    exit_gate:\n      - \"true\"\n", nil, nil)
	if l := handoffLine(r.handoff, "**L1**"); !strings.Contains(l, "past this lot's wall of 1s") {
		t.Fatalf("L1 reads %q over a suite lot_verify times out", l)
	}
}
