package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A commit stores the INDEX, not the working tree: for an entry `git add`
// leaves alone (skip-worktree, assume-unchanged, a stat it trusts), what the
// index holds is what lands, whatever the file on disk and HEAD say. And a
// clean filter — code of the judged party's choosing — decides what is stored
// each time a path is stored, free to answer the judge and the landing
// differently. The index is therefore judged as a third tree, and an index
// flag or a `filter` on a contract path is refused outright.
//
// The contract is also the plan's whole directory, not five names: what the
// base holds beside the plan is the owner's or an earlier lot's, byte-frozen,
// and the lot's own records — a path it creates, a path carrying its id —
// stay its to write.

const forgedCheck = `"check": "true"`

// stageOnly puts body in the index for rel, the file on disk untouched, and
// sets flag on the entry ("" for none).
func stageOnly(t *testing.T, ws string, git func(args ...string) string, rel, body, flag string) {
	t.Helper()
	blob := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blob, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git("update-index", "--cacheinfo", "100644,"+git("hash-object", "-w", blob)+","+rel)
	if flag != "" {
		git("update-index", flag, rel)
	}
}

func forgedOutcomes() string {
	return strings.Replace(contractOutcomes, `"check": "sh ci/runtime-gate.sh"`, forgedCheck, 1)
}

func TestModernizeContractIsJudgedInTheIndex(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	// One guard per scenario: the index read as a tree, then each flag alone
	// with nothing staged, so neither can stand in for the other.
	t.Run("a rewrite staged in the index, no flag: the index is judged", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		stageOnly(t, ws, git, ".modernize/outcomes.json", forgedOutcomes(), "")
		refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate),
			".modernize/outcomes.json", "outcomes[runtime-target].check changed (staged in the index)")
	})
	t.Run("skip-worktree on a contract path, nothing staged: refused for the flag", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		git("update-index", "--skip-worktree", ".modernize/outcomes.json")
		refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate),
			".modernize/outcomes.json", "flagged skip-worktree in the index")
	})
	t.Run("assume-unchanged on a contract path, nothing staged: refused for the flag", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		git("update-index", "--assume-unchanged", ".modernize/brief.yaml")
		refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate),
			".modernize/brief.yaml", "flagged assume-unchanged in the index")
	})
	t.Run("the reviewer's shape: a rewrite staged under skip-worktree, the disk and HEAD clean", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		stageOnly(t, ws, git, ".modernize/outcomes.json", forgedOutcomes(), "--skip-worktree")
		writeContract(t, ws, "product.txt", "the lot's product\n")
		got := landLot(t, ws, base, "true", nil)
		if got.converged || got.marked {
			t.Fatalf("an index-only rewrite converged: contract_rewritten=%q", got.verdict.ContractRewrite)
		}
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", got)
	})
	t.Run("a gate command stages a rewrite after the early check", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		forged := filepath.Join(ws, "ci", "forged.json")
		writeContract(t, ws, "ci/forged.json", forgedOutcomes())
		gate := withLotGate(t, ws, git,
			`git update-index --cacheinfo "100644,$(git hash-object -w `+forged+`),.modernize/outcomes.json"`+
				"\ngit update-index --skip-worktree .modernize/outcomes.json")
		got := landLot(t, ws, base, gate, nil)
		if got.converged || got.marked {
			t.Fatalf("a rewrite a gate command staged converged: contract_rewritten=%q", got.verdict.ContractRewrite)
		}
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", got)
	})
	// The other face: a legitimate write, staged and left uncommitted, lands.
	t.Run("the remediated entry's fix staged, not committed: it lands", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/defects-ledger.json",
			`"disposition": "open", "reason": "carried by L1"`, `"disposition": "fixed", "reason": "carried by L1"`)
		git("add", ".modernize/defects-ledger.json")
		got := landLot(t, ws, base, "true", nil)
		if !got.marked {
			t.Fatalf("a legitimate staged write was refused: contract_rewritten=%q notice=%q log=%s",
				got.verdict.ContractRewrite, got.notice, got.verdict.LogTail)
		}
	})
}

func TestModernizeContractRefusesAFilterOnItsPaths(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	t.Run("the reviewer's shape: a clean filter keyed on the judge's environment", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		writeContract(t, ws, ".gitattributes", ".modernize/outcomes.json filter=owner\n")
		writeContract(t, ws, "ci/owner-clean.sh", "#!/bin/sh\n"+
			"if [ -n \"$GIT_NO_REPLACE_OBJECTS\" ]; then cat; else sed 's#sh ci/runtime-gate.sh#true#'; fi\n")
		git("config", "filter.owner.clean", "sh "+filepath.Join(ws, "ci/owner-clean.sh"))
		git("add", ".gitattributes", "ci/owner-clean.sh")
		git("commit", "-qm", "the lot brings its own filter")
		got := landLot(t, ws, base, "true", nil)
		if got.converged || got.marked {
			t.Fatalf("a contract path under a clean filter converged: contract_rewritten=%q", got.verdict.ContractRewrite)
		}
		if !strings.Contains(strings.Join(got.verdict.ContractRewrite, "\n"), ".modernize/outcomes.json: a `filter` attribute (owner)") {
			t.Fatalf("contract_rewritten = %q, want the filter named", got.verdict.ContractRewrite)
		}
	})
	t.Run("a filter set from the repository's own attributes, nothing committed", func(t *testing.T) {
		ws, base, _ := programmeRepo(t)
		writeContract(t, ws, ".git/info/attributes", ".modernize/brief.yaml filter=quiet\n")
		refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate),
			".modernize/brief.yaml", "a `filter` attribute (quiet)")
	})
	// The other face: attributes that only fix line endings are not filters.
	t.Run("end-of-line attributes on the contract are not refused", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		writeContract(t, ws, ".gitattributes", ".modernize/** text eol=lf\n")
		git("add", ".gitattributes")
		git("commit", "-qm", "normalise the contract's line endings")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
}

// programmeWithRecords is programmeRepo plus what a programme's directory
// carries after a few lots: an owner file beside the plan, an earlier lot's
// report, a lot whose id extends this one's, and a sweep record.
func programmeWithRecords(t *testing.T) (string, string, func(args ...string) string) {
	t.Helper()
	ws, _, git := programmeRepo(t)
	writeContract(t, ws, ".modernize/goals.yaml", "goals:\n  - id: supported-runtime\n    statement: \"under active support\"\n")
	writeContract(t, ws, ".modernize/L0-report.md", "# L0\n\nwhat L0 found\n")
	writeContract(t, ws, ".modernize/L10-report.md", "# L10\n\nwhat L10 found\n")
	writeContract(t, ws, ".modernize/sweeps/L0.md", "# sweep L0\n")
	writeContract(t, ws, ".modernize/L1-report.md", "# L1, pass 1\n")
	git("add", "-A")
	git("commit", "-qm", "the directory after a few lots")
	return ws, git("rev-parse", "HEAD"), git
}

func TestModernizeContractIsTheWholeDirectory(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	t.Run("the reviewer's shape: an owner file beside the plan rewritten by a gate", func(t *testing.T) {
		ws, base, git := programmeWithRecords(t)
		gate := withLotGate(t, ws, git,
			`sed -i 's#under active support#whatever the lot prefers#' .modernize/goals.yaml`+
				"\ngit commit -qam 'the gate rewrites the owner file'")
		nothingLandsUnderDone(t, ws, base, ".modernize/goals.yaml", landLot(t, ws, base, gate, nil))
	})
	for _, tc := range []struct {
		name, file, cause string
		rewrite           func(t *testing.T, ws string)
	}{
		{"an earlier lot's report rewritten", ".modernize/L0-report.md", "changed — beside the plan",
			func(t *testing.T, ws string) {
				writeContract(t, ws, ".modernize/L0-report.md", "# L0\n\nnothing to see\n")
			}},
		{"an earlier lot's report deleted", ".modernize/L0-report.md", "DELETED",
			func(t *testing.T, ws string) {
				if err := os.Remove(filepath.Join(ws, ".modernize", "L0-report.md")); err != nil {
					t.Fatal(err)
				}
			}},
		{"an earlier lot's sweep record rewritten", ".modernize/sweeps/L0.md", "changed — beside the plan",
			func(t *testing.T, ws string) { writeContract(t, ws, ".modernize/sweeps/L0.md", "# nothing swept\n") }},
		{"a lot whose id extends this one's is not this one's", ".modernize/L10-report.md", "changed — beside the plan",
			func(t *testing.T, ws string) {
				writeContract(t, ws, ".modernize/L10-report.md", "# L10, rewritten by L1\n")
			}},
		{"an owner file beside the plan swapped for a symlink", ".modernize/goals.yaml", "changed — beside the plan",
			func(t *testing.T, ws string) {
				if err := os.Remove(filepath.Join(ws, ".modernize", "goals.yaml")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../docs/goals.yaml", filepath.Join(ws, ".modernize", "goals.yaml")); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, base, git := programmeWithRecords(t)
			tc.rewrite(t, ws)
			git("add", "-A")
			git("commit", "-qm", "the worker rewrites the directory")
			refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate), tc.file, tc.cause)
		})
	}
	// The other face: what a lot is asked to write beside the plan stays its own.
	t.Run("the lot's own records: created, updated, and new files of any name", func(t *testing.T) {
		ws, base, git := programmeWithRecords(t)
		writeContract(t, ws, ".modernize/L1-report.md", "# L1, pass 2\n\nwhat changed\n")
		writeContract(t, ws, ".modernize/L1-probe.py", "print('probe')\n")
		writeContract(t, ws, ".modernize/L1-captures/a.json", "{}\n")
		writeContract(t, ws, ".modernize/sweeps/L1.md", "# sweep L1\n")
		writeContract(t, ws, ".modernize/pdf-probe.py", "print('a probe the lot created')\n")
		git("add", "-A")
		git("commit", "-qm", "the lot's own records")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("a gate command writing the lot's own report: it lands", func(t *testing.T) {
		ws, base, git := programmeWithRecords(t)
		gate := withLotGate(t, ws, git, "printf '# L1, written by its gate\\n' > .modernize/L1-report.md\ngit commit -qam 'the report'")
		got := landLot(t, ws, base, gate, nil)
		if !got.marked {
			t.Fatalf("the lot's own report was refused: contract_rewritten=%q notice=%q", got.verdict.ContractRewrite, got.notice)
		}
	})
}

func TestModernizeContractFilesAreRegularAtTheBase(t *testing.T) {
	requireModernizeTools(t)
	ws, _, git := programmeRepo(t)
	writeContract(t, ws, "docs/outcomes.json", contractOutcomes)
	if err := os.Remove(filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../docs/outcomes.json", filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "an outcomes file the contract only links to")
	base := git("rev-parse", "HEAD")
	res, exit := modernizeLotVerifyEnv(t, toolScript(t, "modernize/main.bot", "lot_verify"), ws, "L1", base, contractGate, nil)
	if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, ".modernize/outcomes.json is a symlink at the run's base") {
		t.Fatalf("exit %d contract_unreadable=%v log %q — a contract file the base only links to cannot be judged", exit, res.Unreadable, res.LogTail)
	}
	if gateRan(ws) {
		t.Fatal("the gate ran over a contract file it could not judge")
	}
}

func TestModernizePlanReadRefusesANonRegularPlan(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "plan_read")
	for _, tc := range []struct {
		name  string
		shape func(t *testing.T, plan string)
	}{
		{"a dangling symlink (a plan committed as mode 120000, checked out)", func(t *testing.T, plan string) {
			if err := os.Symlink(contractPlan, plan); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink to a real plan", func(t *testing.T, plan string) {
			real := filepath.Join(filepath.Dir(plan), "real-plan.yaml")
			if err := os.WriteFile(real, []byte(contractPlan), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real-plan.yaml", plan); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory", func(t *testing.T, plan string) {
			if err := os.MkdirAll(plan, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, _, _ := programmeRepo(t)
			plan := filepath.Join(ws, ".modernize", "plan.yaml")
			if err := os.Remove(plan); err != nil {
				t.Fatal(err)
			}
			tc.shape(t, plan)
			res := modernizePlanReadAt(t, script, ws)
			if res.NothingToDo || !res.Refused || !strings.Contains(res.Notice, "CONTRACT_UNREADABLE") {
				t.Fatalf("plan_read read a non-regular plan as %+v — want CONTRACT_UNREADABLE, never a no-op", res)
			}
		})
	}
	t.Run("an absent plan is still the legitimate no-op", func(t *testing.T) {
		ws, _, _ := programmeRepo(t)
		if err := os.Remove(filepath.Join(ws, ".modernize", "plan.yaml")); err != nil {
			t.Fatal(err)
		}
		res := modernizePlanReadAt(t, script, ws)
		if !res.NothingToDo || res.Refused {
			t.Fatalf("an absent plan must stay nothing_to_do: %+v", res)
		}
	})
}
