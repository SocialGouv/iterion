package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
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
// and the lot's own records — a path carrying its id, a path it creates that
// is named for no other lot — stay its to write.

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

// programmeOfLots is programmeRepo whose plan also declares the lots named,
// later lots of the programme, committed: the base a lot runs from.
func programmeOfLots(t *testing.T, ids ...string) (string, string, func(args ...string) string) {
	t.Helper()
	ws, base, git := programmeRepo(t)
	if len(ids) == 0 {
		return ws, base, git
	}
	addLots(t, ws, ids...)
	git("add", "-A")
	git("commit", "-qm", "the programme's later lots")
	return ws, git("rev-parse", "HEAD"), git
}

// addLots appends one lot per id to the plan in the working tree.
func addLots(t *testing.T, ws string, ids ...string) {
	t.Helper()
	body := readContract(t, ws, ".modernize/plan.yaml")
	for _, id := range ids {
		body += "  - id: " + id + "\n    title: \"a later lot\"\n    status: todo\n    exit_gate:\n      - \"true\"\n"
	}
	writeContract(t, ws, ".modernize/plan.yaml", body)
}

// A name under the contract's directory belongs to the lot whose id it
// carries, among the lots the plan declares at the base and the lots the
// landing adds: the outermost name that carries an id decides, and the longest
// id when several match. A lot writes its own records and records named for
// no lot; another lot's records are that lot's — frozen when the base holds
// them, refused when the lot creates them. A link stands for every path
// beneath it: a lot creates one only under its own id.
func TestModernizeContractRecordsBelongToTheLotTheyAreNamedFor(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	t.Run("an earlier lot whose id extends this one's: its record is that lot's", func(t *testing.T) {
		ws, _, git := programmeOfLots(t, "L1-b")
		writeContract(t, ws, ".modernize/L1-b-report.md", "# L1-b\n\nwhat L1-b found\n")
		git("add", "-A")
		git("commit", "-qm", "an earlier lot's record")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, ".modernize/L1-b-report.md", "# L1-b, rewritten by L1\n")
		git("commit", "-qam", "L1 rewrites L1-b's record")
		refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate),
			".modernize/L1-b-report.md", "changed — beside the plan")
	})
	for _, tc := range []struct {
		name, file, cause string
		lots              []string
		write             func(t *testing.T, ws string, git func(args ...string) string)
	}{
		{"another lot's sweep record, committed", ".modernize/sweeps/L22.md", "named for lot L22", []string{"L22"},
			func(t *testing.T, ws string, git func(args ...string) string) {
				writeContract(t, ws, ".modernize/sweeps/L22.md", "# sweep L22, written by L1\n")
				git("add", "-A")
				git("commit", "-qm", "L1 writes L22's sweep record")
			}},
		{"another lot's sweep record, left in the working tree", ".modernize/sweeps/L22.md", "named for lot L22", []string{"L22"},
			func(t *testing.T, ws string, _ func(args ...string) string) {
				writeContract(t, ws, ".modernize/sweeps/L22.md", "# sweep L22, written by L1\n")
			}},
		{"another lot's sweep record, staged under skip-worktree and gone from disk", ".modernize/sweeps/L22.md", "named for lot L22", []string{"L22"},
			func(t *testing.T, ws string, git func(args ...string) string) {
				writeContract(t, ws, ".modernize/sweeps/L22.md", "# sweep L22, written by L1\n")
				git("add", ".modernize/sweeps/L22.md")
				git("update-index", "--skip-worktree", ".modernize/sweeps/L22.md")
				if err := os.Remove(filepath.Join(ws, ".modernize", "sweeps", "L22.md")); err != nil {
					t.Fatal(err)
				}
			}},
		{"a record inside another lot's directory, named for this one", ".modernize/L22-captures/L1.json", "named for lot L22", []string{"L22"},
			func(t *testing.T, ws string, git func(args ...string) string) {
				writeContract(t, ws, ".modernize/L22-captures/L1.json", "{}\n")
				git("add", "-A")
				git("commit", "-qm", "L1 writes into L22's captures")
			}},
		{"the record of a lot this lot proposes", ".modernize/sweeps/L30.md", "named for lot L30", nil,
			func(t *testing.T, ws string, git func(args ...string) string) {
				addLots(t, ws, "L30")
				writeContract(t, ws, ".modernize/sweeps/L30.md", "# sweep L30, written by L1\n")
				git("add", "-A")
				git("commit", "-qm", "L1 proposes L30 and writes its sweep record")
			}},
		// The record's path made to exist without a file named for its lot:
		// `sweeps` linked to the lot's own captures, which hold `L22.md`.
		{"a link standing for another lot's record", ".modernize/sweeps", "as a link", []string{"L22"},
			func(t *testing.T, ws string, git func(args ...string) string) {
				writeContract(t, ws, ".modernize/L1-captures/L22.md", "# sweep L22, written by L1\n")
				if err := os.Symlink("L1-captures", filepath.Join(ws, ".modernize", "sweeps")); err != nil {
					t.Fatal(err)
				}
				git("add", "-A")
				git("commit", "-qm", "L1 links sweeps to its captures")
			}},
		{"a link committed, gone from the working tree", ".modernize/sweeps", "as a link", []string{"L22"},
			func(t *testing.T, ws string, git func(args ...string) string) {
				writeContract(t, ws, ".modernize/L1-captures/L22.md", "# sweep L22, written by L1\n")
				if err := os.Symlink("L1-captures", filepath.Join(ws, ".modernize", "sweeps")); err != nil {
					t.Fatal(err)
				}
				git("add", "-A")
				git("commit", "-qm", "L1 links sweeps to its captures")
				if err := os.Remove(filepath.Join(ws, ".modernize", "sweeps")); err != nil {
					t.Fatal(err)
				}
			}},
		{"a repository nested beside the plan", ".modernize/scratch/", "as a link", []string{"L22"},
			func(t *testing.T, ws string, _ func(args ...string) string) {
				writeContract(t, ws, ".modernize/scratch/sweeps/L22.md", "# sweep L22, written by L1\n")
				gittest.Run(t, filepath.Join(ws, ".modernize", "scratch"), "init", "-q")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, base, git := programmeOfLots(t, tc.lots...)
			tc.write(t, ws, git)
			refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate), tc.file, tc.cause)
		})
	}
	// The other face: what a lot owns stays its own, and a name the plan
	// declares no lot for is no lot's.
	t.Run("the lot with the longer id owns its records", func(t *testing.T) {
		ws, _, git := programmeOfLots(t, "L1-b")
		writeContract(t, ws, ".modernize/L1-b-report.md", "# L1-b, pass 1\n")
		git("add", "-A")
		git("commit", "-qm", "L1-b's first pass")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, ".modernize/L1-b-report.md", "# L1-b, pass 2\n")
		writeContract(t, ws, ".modernize/sweeps/L1-b.md", "# sweep L1-b\n")
		git("add", "-A")
		git("commit", "-qm", "L1-b's own records")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1-b", base, contractGate))
	})
	t.Run("a link under the lot's own id is its own", func(t *testing.T) {
		ws, base, git := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L1-captures/a.json", "{}\n")
		if err := os.Symlink("L1-captures", filepath.Join(ws, ".modernize", "L1-latest")); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "L1's captures and a link to them")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("the lot's records under odd names are judged", func(t *testing.T) {
		ws, base, git := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L1-odd\nname.md", "# a name holding a newline\n")
		writeContract(t, ws, ".modernize/L1-\xff.md", "# a name that is not UTF-8\n")
		git("add", "-A")
		git("commit", "-qm", "odd names")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("an owner's link whose target is not UTF-8, left alone", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		if err := os.Symlink("../docs/\xff", filepath.Join(ws, ".modernize", "archive")); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "the owner links its archive")
		base := git("rev-parse", "HEAD")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("the lot's own directory may hold files named for other lots", func(t *testing.T) {
		ws, base, git := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L1-captures/L22.json", "{}\n")
		git("add", "-A")
		git("commit", "-qm", "L1's captures")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("a name the plan declares no lot for is no lot's", func(t *testing.T) {
		ws, base, git := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L99-notes.md", "# notes under an id no lot carries\n")
		git("add", "-A")
		git("commit", "-qm", "notes")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("an ignored file is not part of the landing", func(t *testing.T) {
		ws, _, git := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/.gitignore", "*.tmp\n")
		git("add", "-A")
		git("commit", "-qm", "the owner ignores scratch files")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, ".modernize/sweeps/L22.md.tmp", "scratch\n")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
	t.Run("a filter on the lot's own records is not refused", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, ".gitattributes", ".modernize/L1-captures/** filter=quiet\n")
		writeContract(t, ws, ".modernize/L1-captures/a.json", "{}\n")
		git("add", "-A")
		git("commit", "-qm", "L1's captures, under a filter")
		acceptedAndJudged(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate))
	})
}

// The lots a name is matched against are read from the plan at the base: a
// base whose `lots` is not a list of lots leaves every name unowned, and a
// judge that cannot tell whose a record is must say so, not guess.
func TestModernizeContractLotsUnreadableAtTheBase(t *testing.T) {
	requireModernizeTools(t)
	ws, _, git := modernizeRepo(t, "version: 1\nlots:\n  L1:\n    status: todo\n")
	modernizeNet(t, ws)
	git("add", "-A")
	git("commit", "-qm", "the net")
	base := git("rev-parse", "HEAD")
	res, exit := modernizeLotVerifyEnv(t, toolScript(t, "modernize/main.bot", "lot_verify"), ws, "L1", base, contractGate, nil)
	if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "declares `lots` in an unreadable shape") {
		t.Fatalf("exit %d contract_unreadable=%v log %q — lots the judge cannot read leave whose records are whose unknown", exit, res.Unreadable, res.LogTail)
	}
	if gateRan(ws) {
		t.Fatal("the gate ran over a contract whose lots could not be read")
	}
}
