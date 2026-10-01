package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The contract is judged on what LANDS, not only on the tree the gate
// commands start from. Two trees land from a converged lot: the commit
// mark_done writes (HEAD plus the status flip) and the working tree the
// engine's finalize banks as a wip commit — which the merge endpoint accepts
// like any other run. The exit_gate commands run the worker's code after the
// early check, and a rewrite committed then hidden from the working tree
// passes a check that reads the working tree alone.
//
// Each case below runs what the graph runs: lot_verify, lot_gate's
// `converged` as the bot ships it, then mark_done when that holds. A rewrite
// is either refused, by name, or absent from BOTH trees that land.

// lotGateHolds evaluates one field of the shipped lot_gate over a lot_verify
// report, as the engine does: outputs.lot_verify.<field> and the bot's vars.
func lotGateHolds(t *testing.T, field string, report map[string]any) bool {
	t.Helper()
	cr := ir.Compile(parseBotUnit("modernize/main.bot").File)
	if cr.Workflow == nil {
		t.Fatal("modernize/main.bot does not compile")
	}
	gate, ok := cr.Workflow.Nodes["lot_gate"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("lot_gate is %T, want a compute node", cr.Workflow.Nodes["lot_gate"])
	}
	for _, e := range gate.Exprs {
		if e.Key != field {
			continue
		}
		v, err := e.AST.EvalBool(&expr.Context{
			Outputs: func(path []string) any {
				if len(path) == 2 && path[0] == "lot_verify" {
					return report[path[1]]
				}
				return nil
			},
			Vars: func(path []string) any {
				if len(path) == 1 && (path[0] == "reanchor" || path[0] == "extend") {
					return true
				}
				return nil
			},
		})
		if err != nil {
			t.Fatalf("evaluating lot_gate.%s: %v", field, err)
		}
		return v
	}
	t.Fatalf("lot_gate declares no %q", field)
	return false
}

// landed is what a lot left behind once the graph ran to its end.
type landed struct {
	verdict   modernizeLotVerifyOut
	converged bool
	stop      bool
	marked    bool
	refused   bool
	notice    string
}

// landLot runs lot_verify, then mark_done when lot_gate converges, fed the
// verdict the edge maps. `between` runs after the verdict and before
// mark_done — what a process the lot left running could do in that gap.
func landLot(t *testing.T, ws, base, gate string, between func()) landed {
	t.Helper()
	verify := toolScript(t, "modernize/main.bot", "lot_verify")
	mark := toolScript(t, "modernize/main.bot", "mark_done")
	res, raw, exit := modernizeLotVerifyRaw(t, verify, ws, "L1", base, gate, nil)
	if exit != 0 {
		t.Fatalf("lot_verify exited %d: %+v", exit, res)
	}
	out := landed{verdict: res, converged: lotGateHolds(t, "converged", raw), stop: lotGateHolds(t, "stop", raw)}
	if !out.converged {
		return out
	}
	if between != nil {
		between()
	}
	done := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
	out.marked, out.refused, out.notice = done.Marked, done.Refused, done.Notice
	return out
}

// withLotGate commits the worker's gate script, as a worker writes one, and
// returns the exit_gate command that runs it.
func withLotGate(t *testing.T, ws string, git func(args ...string) string, body string) string {
	t.Helper()
	writeContract(t, ws, "ci/lot-gate.sh", "#!/bin/sh\nset -e\n"+body+"\n")
	git("add", "ci/lot-gate.sh")
	git("commit", "-qm", "the worker writes its gate")
	return "sh ci/lot-gate.sh"
}

// undoFlip removes the one write mark_done makes, so what landed compares
// with the base byte for byte.
func undoFlip(text string) string {
	return strings.Replace(text, "    status: done\n    remediates", "    status: todo\n    remediates", 1)
}

// nothingLandsUnderDone asserts the one property: when the gate's `done` is
// committed, neither the commit nor the working tree the finalize banks
// carries rel other than the base does; when it is not, the refusal names rel.
func nothingLandsUnderDone(t *testing.T, ws, base, rel string, got landed) {
	t.Helper()
	if !got.marked {
		if !strings.Contains(strings.Join(got.verdict.ContractRewrite, "\n"), rel) && !strings.Contains(got.notice, rel) {
			t.Fatalf("not marked, but no refusal names %s: converged=%v contract_rewritten=%q notice=%q log=%s",
				rel, got.converged, got.verdict.ContractRewrite, got.notice, got.verdict.LogTail)
		}
		// A refusal goes back to the worker; a stop would end the run
		// `finished`, the rewrite in its tree, and a router lands those.
		if got.stop {
			t.Fatalf("a refused verdict stops the run: lot_gate.stop holds with contract_rewritten=%q", got.verdict.ContractRewrite)
		}
		return
	}
	want := gittest.Run(t, ws, "--no-replace-objects", "show", base+":"+rel)
	if head := undoFlip(gittest.Run(t, ws, "--no-replace-objects", "show", "HEAD:"+rel)); head != want {
		t.Fatalf("a rewrite of %s landed under done, in the gate's commit:\n%s", rel, head)
	}
	tree, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatalf("%s is gone from the working tree the finalize banks: %v", rel, err)
	}
	if got := undoFlip(strings.TrimSpace(string(tree))); got != want {
		t.Fatalf("a rewrite of %s landed under done, in the working tree the finalize banks:\n%s", rel, got)
	}
}

const rewriteCheck = `sed -i 's#"check": "sh ci/runtime-gate.sh"#"check": "true"#' .modernize/outcomes.json`

func TestModernizeContractIsJudgedOnWhatLands(t *testing.T) {
	requireModernizeTools(t)

	t.Run("a gate command commits a rewrite after the early check", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git, rewriteCheck+"\ngit commit -qam 'the gate rewrites a check'")
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", landLot(t, ws, base, gate, nil))
	})
	t.Run("a gate command leaves a rewrite in the working tree", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git, rewriteCheck)
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", landLot(t, ws, base, gate, nil))
	})
	t.Run("a gate command commits a register entry the lot does not remediate", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git,
			`sed -i 's#"disposition": "open", "reason": "no lot#"disposition": "preserved", "reason": "no lot#' .modernize/defects-ledger.json`+
				"\ngit commit -qam 'the gate closes a defect'")
		nothingLandsUnderDone(t, ws, base, ".modernize/defects-ledger.json", landLot(t, ws, base, gate, nil))
	})
	t.Run("a gate command commits another lot's gate away", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git,
			`sed -i '$d' .modernize/plan.yaml`+"\ngit commit -qam 'the gate drops L2s gate'")
		nothingLandsUnderDone(t, ws, base, ".modernize/plan.yaml", landLot(t, ws, base, gate, nil))
	})
	t.Run("a rewrite committed, the working tree restored", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
		git("commit", "-qam", "the worker rewrites a check")
		writeContract(t, ws, ".modernize/outcomes.json", contractOutcomes)
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", landLot(t, ws, base, "true", nil))
	})
	t.Run("a rewrite committed, the restore hidden by assume-unchanged", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
		git("commit", "-qam", "the worker rewrites a check")
		git("update-index", "--assume-unchanged", ".modernize/outcomes.json")
		writeContract(t, ws, ".modernize/outcomes.json", contractOutcomes)
		if st := git("status", "--porcelain"); st != "" {
			t.Fatalf("the fixture must read clean to git, status says %q", st)
		}
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", landLot(t, ws, base, "true", nil))
	})

	t.Run("a rewrite committed, the index and the working tree both put back", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		baseBlob := git("rev-parse", base+":.modernize/outcomes.json")
		editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
		git("commit", "-qam", "the worker rewrites a check")
		git("update-index", "--cacheinfo", "100644,"+baseBlob+",.modernize/outcomes.json")
		writeContract(t, ws, ".modernize/outcomes.json", contractOutcomes)
		// Only HEAD's tree carries the rewrite now.
		got := landLot(t, ws, base, "true", nil)
		if !strings.Contains(strings.Join(got.verdict.ContractRewrite, "\n"), ".modernize/outcomes.json: outcomes[runtime-target].check changed (committed at HEAD") {
			t.Fatalf("a rewrite only HEAD carries was not named: contract_rewritten=%q", got.verdict.ContractRewrite)
		}
		nothingLandsUnderDone(t, ws, base, ".modernize/outcomes.json", got)
	})
	t.Run("a done committed by the worker, the working tree restored", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/plan.yaml", "    status: todo\n    remediates", "    status: done\n    remediates")
		git("commit", "-qam", "the worker writes the verdict")
		editContract(t, ws, ".modernize/plan.yaml", "    status: done\n    remediates", "    status: todo\n    remediates")
		got := landLot(t, ws, base, "true", nil)
		if !got.verdict.DoneSelfWritten || got.converged || got.stop {
			t.Fatalf("a done the worker committed passed for the gate's: done_self_written=%v converged=%v stop=%v (log %s)",
				got.verdict.DoneSelfWritten, got.converged, got.stop, got.verdict.LogTail)
		}
	})
	t.Run("a gate command commits the lot's own done", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git,
			`sed -i '0,/^    status: todo$/s//    status: done/' .modernize/plan.yaml`+"\ngit commit -qam 'the gate writes the verdict'")
		got := landLot(t, ws, base, gate, nil)
		if !got.verdict.DoneSelfWritten || got.converged || got.stop {
			t.Fatalf("a done a gate command committed passed for the gate's: done_self_written=%v converged=%v stop=%v (log %s)",
				got.verdict.DoneSelfWritten, got.converged, got.stop, got.verdict.LogTail)
		}
	})

	// The other face: what a lot may write still lands when its gate command
	// is what writes it.
	t.Run("a gate command commits the fix of the entry the lot remediates: it lands", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		gate := withLotGate(t, ws, git,
			`sed -i 's#"disposition": "open", "reason": "carried by L1"#"disposition": "fixed", "reason": "carried by L1"#' .modernize/defects-ledger.json`+
				"\ngit commit -qam 'the gate records the fix'")
		got := landLot(t, ws, base, gate, nil)
		if !got.marked {
			t.Fatalf("a legitimate write was refused at landing: converged=%v contract_rewritten=%q notice=%q log=%s",
				got.converged, got.verdict.ContractRewrite, got.notice, got.verdict.LogTail)
		}
		if !strings.Contains(git("--no-replace-objects", "show", "HEAD:.modernize/defects-ledger.json"), `"disposition": "fixed", "reason": "carried by L1"`) {
			t.Fatal("the remediated entry's fix did not land under done")
		}
	})
}

// TestModernizeMarkDoneCommitsOnlyWhatWasJudged isolates mark_done's own
// guard: the verdict judged one HEAD and one working tree, and mark_done
// writes the gate's word on exactly those or refuses — a commit or an edit
// arriving after the verdict is not what the verdict answered for.
func TestModernizeMarkDoneCommitsOnlyWhatWasJudged(t *testing.T) {
	requireModernizeTools(t)

	t.Run("HEAD moved after the verdict", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			// A commit that leaves every contract file as judged: only HEAD
			// moved, so only the HEAD guard can see it.
			writeContract(t, ws, "docs/after-the-verdict.md", "written after the verdict\n")
			git("add", "docs/after-the-verdict.md")
			git("commit", "-qm", "a commit after the verdict")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, "HEAD moved") {
			t.Fatalf("mark_done committed on a HEAD the verdict never judged: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("the working tree moved after the verdict", func(t *testing.T) {
		ws, base, _ := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, ".modernize/outcomes.json") {
			t.Fatalf("mark_done committed over a working tree the verdict never judged: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("no verdict to answer for", func(t *testing.T) {
		ws, base, _ := programmeRepo(t)
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		res := modernizeMarkDoneJudged(t, mark, ws, "L1", base, "", "", 0)
		if res.Marked || !res.Refused || !strings.Contains(res.Notice, "no verdict to answer for") {
			t.Fatalf("mark_done marked with no verdict to answer for, or refused for another cause: %+v", res)
		}
	})
	// The cause is asserted, not only the refusal: an empty tree also reads
	// as every path of the directory having appeared, and that refusal would
	// hide a guard that no longer holds — with the plan at the repository's
	// root, nothing would refuse at all.
	t.Run("a verdict without its judged tree", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		res := modernizeMarkDoneJudged(t, mark, ws, "L1", base, git("rev-parse", "HEAD"), "", 0)
		if res.Marked || !res.Refused || !strings.Contains(res.Notice, "no verdict to answer for") {
			t.Fatalf("mark_done marked with a HEAD and no working tree to answer for, or refused for another cause: %+v", res)
		}
	})
	t.Run("the plan edited after the verdict is not taken for the gate's own line", func(t *testing.T) {
		ws, base, _ := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			writeContract(t, ws, ".modernize/plan.yaml", strings.TrimSuffix(contractPlan, "      - \"true\"\n"))
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, ".modernize/plan.yaml") {
			t.Fatalf("mark_done committed over a plan edited after the verdict: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("a commit on the judged HEAD that is not the gate's own is not taken for it", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			// A commit of a forged plan on the judged HEAD, the working tree
			// left as judged: only the recognition of the gate's own commit
			// stands between it and a landing.
			forged := filepath.Join(t.TempDir(), "plan.yaml")
			if err := os.WriteFile(forged, []byte(strings.TrimSuffix(contractPlan, "      - \"true\"\n")), 0o644); err != nil {
				t.Fatal(err)
			}
			blob := git("hash-object", "-w", forged)
			index := filepath.Join(t.TempDir(), "index")
			cmd := gittest.Cmd(ws, "read-tree", "HEAD")
			cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("read-tree: %v %s", err, out)
			}
			cmd = gittest.Cmd(ws, "update-index", "--cacheinfo", "100644,"+blob+",.modernize/plan.yaml")
			cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("update-index: %v %s", err, out)
			}
			cmd = gittest.Cmd(ws, "write-tree")
			cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
			tree, err := cmd.Output()
			if err != nil {
				t.Fatalf("write-tree: %v", err)
			}
			head := git("rev-parse", "HEAD")
			sha := git("commit-tree", strings.TrimSpace(string(tree)), "-p", head, "-m", "L1: done — gate, oracle and references green at "+base[:12])
			git("update-ref", "HEAD", sha, head)
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, "HEAD moved") {
			t.Fatalf("a forged commit on the judged HEAD was taken for the gate's own: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})

	t.Run("the index moved after the verdict", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			blob := filepath.Join(t.TempDir(), "forged.json")
			if err := os.WriteFile(blob, []byte(strings.Replace(contractOutcomes, `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			git("update-index", "--cacheinfo", "100644,"+git("hash-object", "-w", blob)+",.modernize/outcomes.json")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, ".modernize/outcomes.json in the index") {
			t.Fatalf("mark_done committed over an index the verdict never judged: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("a filter put on a contract path after the verdict", func(t *testing.T) {
		ws, base, _ := programmeRepo(t)
		got := landLot(t, ws, base, "true", func() {
			writeContract(t, ws, ".git/info/attributes", ".modernize/outcomes.json filter=late\n")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, "filter") {
			t.Fatalf("mark_done committed under a filter the verdict never saw: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("an earlier lot's record moved after the verdict", func(t *testing.T) {
		ws, _, git := programmeRepo(t)
		writeContract(t, ws, ".modernize/L0-report.md", "# L0\n")
		git("add", ".modernize/L0-report.md")
		git("commit", "-qm", "an earlier lot's record")
		base := git("rev-parse", "HEAD")
		got := landLot(t, ws, base, "true", func() {
			writeContract(t, ws, ".modernize/L0-report.md", "# L0, rewritten after the verdict\n")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, ".modernize/L0-report.md") {
			t.Fatalf("mark_done committed over a record the verdict never judged: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("another lot's record appeared after the verdict", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t, "L22")
		got := landLot(t, ws, base, "true", func() {
			writeContract(t, ws, ".modernize/sweeps/L22.md", "# sweep L22, written after the verdict\n")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, ".modernize/sweeps/L22.md appeared") {
			t.Fatalf("mark_done committed over a record that appeared after the verdict: marked=%v refused=%v notice=%q", got.marked, got.refused, got.notice)
		}
	})
	t.Run("the lot's own record left uncommitted at the verdict: marked", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L1-notes.md", "# what L1 noted\n")
		got := landLot(t, ws, base, "true", nil)
		if !got.marked || got.refused {
			t.Fatalf("the lot's own record, judged by the verdict, stopped the gate's word: marked=%v refused=%v notice=%q contract_rewritten=%q",
				got.marked, got.refused, got.notice, got.verdict.ContractRewrite)
		}
	})
	t.Run("the lot's records under odd names, judged by the verdict: marked", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t, "L22")
		writeContract(t, ws, ".modernize/L1-odd\nname.md", "# a name holding a newline\n")
		writeContract(t, ws, ".modernize/L1-\xff.md", "# a name that is not UTF-8\n")
		got := landLot(t, ws, base, "true", nil)
		if !got.marked || got.refused {
			t.Fatalf("records under odd names stopped the gate's word: marked=%v refused=%v notice=%q contract_rewritten=%q log=%s",
				got.marked, got.refused, got.notice, got.verdict.ContractRewrite, got.verdict.LogTail)
		}
	})
	t.Run("a worker's commit storing the plan as a symlink is not the gate's own", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		res := modernizeLotVerify(t, toolScript(t, "modernize/main.bot", "lot_verify"), ws, "L1", base, "true")
		// The same bytes as the gate's flip, stored at mode 120000, on the
		// judged HEAD, the working tree untouched.
		flipped := strings.Replace(contractPlan, "    status: todo\n    remediates", "    status: done\n    remediates", 1)
		blobFile := filepath.Join(t.TempDir(), "plan.yaml")
		if err := os.WriteFile(blobFile, []byte(flipped), 0o644); err != nil {
			t.Fatal(err)
		}
		blob := git("hash-object", "-w", blobFile)
		index := filepath.Join(t.TempDir(), "index")
		indexed := func(args ...string) string {
			t.Helper()
			cmd := gittest.Cmd(ws, args...)
			cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
			return strings.TrimSpace(string(out))
		}
		indexed("read-tree", res.ContractHead)
		indexed("update-index", "--cacheinfo", "120000,"+blob+",.modernize/plan.yaml")
		tree := indexed("write-tree")
		sha := git("commit-tree", tree, "-p", res.ContractHead, "-m", "chore: the worker's own subject")
		git("update-ref", "HEAD", sha, res.ContractHead)
		done := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
		if done.Marked || !done.Refused || !strings.Contains(done.Notice, "HEAD moved") {
			t.Fatalf("a commit storing the plan as a symlink was taken for the gate's own: %+v", done)
		}
	})

	// The other face: an attempt of mark_done that died part-way is resumed
	// on the SAME verdict, and must finish, not be refused for its own work.
	t.Run("an attempt that committed and died is not committed twice", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		verify := toolScript(t, "modernize/main.bot", "lot_verify")
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		res := modernizeLotVerify(t, verify, ws, "L1", base, "true")
		first := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
		if !first.Marked {
			t.Fatalf("the first attempt did not mark: %+v", first)
		}
		again := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
		if again.Refused || again.Marked {
			t.Fatalf("the re-execution on the same verdict was refused or committed twice: %+v", again)
		}
		if head := git("rev-parse", "HEAD"); head != first.Commit {
			t.Fatalf("HEAD moved on the re-execution: %s, the gate's commit %s", head, first.Commit)
		}
	})
	t.Run("an attempt that wrote its line and died, a proposal left uncommitted: committed", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		verify := toolScript(t, "modernize/main.bot", "lot_verify")
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		// The proposal a lot may write, left uncommitted; the verdict accepts it.
		writeContract(t, ws, ".modernize/plan.yaml", contractPlan+"  - id: L3\n    title: \"a lot this one proposes\"\n    status: todo\n    exit_gate:\n      - \"true\"\n")
		res := modernizeLotVerify(t, verify, ws, "L1", base, "true")
		if len(res.ContractRewrite) != 0 {
			t.Fatalf("an added lot is a proposal the verdict accepts, got %q", res.ContractRewrite)
		}
		editContract(t, ws, ".modernize/plan.yaml", "    status: todo\n    remediates", "    status: done\n    remediates")
		done := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
		if !done.Marked || done.Commit == "" {
			t.Fatalf("the interrupted attempt's own line was refused on a legitimate plan: %+v", done)
		}
		if head := git("rev-parse", "HEAD"); head != done.Commit {
			t.Fatalf("HEAD %s is not the gate's commit %s", head, done.Commit)
		}
	})
	t.Run("an attempt that wrote its line and died before committing is committed", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		verify := toolScript(t, "modernize/main.bot", "lot_verify")
		mark := toolScript(t, "modernize/main.bot", "mark_done")
		res := modernizeLotVerify(t, verify, ws, "L1", base, "true")
		editContract(t, ws, ".modernize/plan.yaml", "    status: todo\n    remediates", "    status: done\n    remediates")
		done := modernizeMarkDoneJudged(t, mark, ws, "L1", base, res.ContractHead, res.ContractTree, 0)
		if !done.Marked || done.Commit == "" {
			t.Fatalf("the interrupted attempt's own line was refused: %+v", done)
		}
		if head := git("rev-parse", "HEAD"); head != done.Commit {
			t.Fatalf("HEAD %s is not the gate's commit %s", head, done.Commit)
		}
	})
}
