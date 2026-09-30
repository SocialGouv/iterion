package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The contract a lot must not rewrite is the plan AND the files its owner
// keeps beside it: what the programme owes (outcomes.json, each `check` a
// verdict the campaign runs), what the owner decided and permits
// (brief.yaml), how a blocked divergence is judged (ARBITRAGE.md), and the
// defects register (defects-ledger.json). lot_verify refuses a rewrite of any
// of them before a single gate command runs, one named cause per file —
// and still accepts the three writes a lot owns: its own status, an added
// lot, and in the register a new entry or an entry its lot `remediates`.
//
// Every fixture below is synthetic; the two lists are the two faces of the
// same guard, and a refusal of a legitimate lot is a defect as much as the
// rewrite that goes through.

const contractPlan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "escape the search term before the query"
    status: todo
    remediates: [D-2]
    brief_targets: []
    exit_gate:
      - "true"
  - id: L2
    title: "raise the runtime"
    status: todo
    depends_on: [L1]
    exit_gate:
      - "true"
`

const contractOutcomes = `{"outcomes": [
  {"id": "runtime-target", "goal_id": "supported-runtime",
   "states": "the service runs on the supported runtime",
   "check": "sh ci/runtime-gate.sh", "arbitration": ""},
  {"id": "defects-closed", "goal_id": "supported-runtime",
   "states": "no registered defect is open",
   "check": "python3 ci/defects-check.py", "arbitration": ""}
]}
`

const contractBrief = `version: 1
objective: |
  Keep the service on a supported runtime.
goals:
  - id: supported-runtime
    statement: "the service runs on a runtime under active support"
permitted_changes:
  - "dependency majors, when the behaviour net stays green"
forbidden_changes:
  - "the public API surface"
decisions:
  - id: runtime-line
    decision: "the next long-term line"
owner: "programme owner"
`

const contractDoctrine = "# Arbitration doctrine\n\n## Classes\n\n- canonicalize: an artefact nobody chose\n"

const contractRegister = `{
 "register": "the programme's defects",
 "dispositions": {"open": "still there", "fixed": "repaired, commit named", "preserved": "kept by an owner decision"},
 "defects": [
  {"id": "D-1", "found": "report L0, section 2", "disposition": "open", "reason": "no lot carries it yet"},
  {"id": "D-2", "found": "report L0, section 3", "disposition": "open", "reason": "carried by L1"},
  {"id": "D-3", "found": "report L0, section 4", "disposition": "fixed", "commit": "abc1234"}
 ]
}
`

// contractGate leaves a marker when it runs: its absence proves the refusal
// came before any gate command.
const contractGate = "sh -c 'echo ran > gate.marker'"

// contractFiles is the programme's whole contract, relative to the root.
var contractFiles = map[string]string{
	".modernize/outcomes.json":       contractOutcomes,
	".modernize/brief.yaml":          contractBrief,
	".modernize/ARBITRAGE.md":        contractDoctrine,
	".modernize/defects-ledger.json": contractRegister,
}

// programmeRepo commits the plan, every file beside it and the smallest net,
// minus the files named in `without` — and returns the base.
func programmeRepo(t *testing.T, without ...string) (string, string, func(args ...string) string) {
	t.Helper()
	ws, _, git := modernizeRepo(t, contractPlan)
	modernizeNet(t, ws)
	skip := map[string]bool{}
	for _, rel := range without {
		skip[rel] = true
	}
	for rel, body := range contractFiles {
		if !skip[rel] {
			writeContract(t, ws, rel, body)
		}
	}
	git("add", "-A")
	git("commit", "-qm", "the contract and its net")
	return ws, git("rev-parse", "HEAD"), git
}

func writeContract(t *testing.T, ws, rel, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readContract(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// editContract replaces one exact span, and fails when the fixture no longer
// carries it — a scenario that edits nothing would pass for the wrong reason.
func editContract(t *testing.T, ws, rel, from, to string) {
	t.Helper()
	body := readContract(t, ws, rel)
	if strings.Count(body, from) != 1 {
		t.Fatalf("%s carries %q %d times, want once", rel, from, strings.Count(body, from))
	}
	writeContract(t, ws, rel, strings.Replace(body, from, to, 1))
}

// editRegister rewrites the register's entry list through JSON, so an entry
// can be added, dropped or reordered without a text anchor.
func editRegister(t *testing.T, ws string, edit func(entries []any) []any) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(readContract(t, ws, ".modernize/defects-ledger.json")), &doc); err != nil {
		t.Fatal(err)
	}
	doc["defects"] = edit(doc["defects"].([]any))
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeContract(t, ws, ".modernize/defects-ledger.json", string(out)+"\n")
}

func gateRan(ws string) bool {
	_, err := os.Stat(filepath.Join(ws, "gate.marker"))
	return err == nil
}

// refusedNaming asserts the rewrite was refused before any gate command, with
// a cause that names the file and what moved in it.
func refusedNaming(t *testing.T, ws string, res modernizeLotVerifyOut, file, cause string) {
	t.Helper()
	found := false
	for _, entry := range res.ContractRewrite {
		if strings.HasPrefix(entry, file+": ") && strings.Contains(entry, cause) {
			found = true
		}
	}
	if !found {
		t.Fatalf("contract_rewritten = %q, want an entry naming %s with %q (log %s)", res.ContractRewrite, file, cause, res.LogTail)
	}
	if res.GatePassed || res.OraclePassed || res.RefsUntouched || res.LotBlocked {
		t.Fatalf("a refused verdict must be red on every conjunct and never a stop: %+v", res)
	}
	if gateRan(ws) {
		t.Fatalf("the exit_gate ran: a rewritten contract must be refused before any gate command")
	}
	if !strings.Contains(res.LogTail, "read-only inside a lot") {
		t.Fatalf("log_tail = %q, want the rule and the way out", res.LogTail)
	}
}

// acceptedAndJudged asserts nothing was refused and the verdict was taken on
// the tree: the gate ran and every conjunct is green.
func acceptedAndJudged(t *testing.T, ws string, res modernizeLotVerifyOut) {
	t.Helper()
	if len(res.ContractRewrite) != 0 || res.DoneSelfWritten || res.Unreadable {
		t.Fatalf("a legitimate write was refused: contract_rewritten=%q unreadable=%v (log %s)", res.ContractRewrite, res.Unreadable, res.LogTail)
	}
	if !res.GatePassed || !res.OraclePassed || !res.RefsUntouched {
		t.Fatalf("expected a green verdict, got %+v", res)
	}
	if !gateRan(ws) {
		t.Fatalf("the exit_gate never ran on an accepted tree")
	}
}

func TestModernizeLotVerifyRefusesEveryContractFileRewrite(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	for _, tc := range []struct {
		name, file, cause string
		without           []string
		rewrite           func(t *testing.T, ws string, git func(args ...string) string)
		// ownCommit: the case commits by itself, or deliberately leaves the
		// rewrite uncommitted.
		ownCommit bool
	}{
		{name: "an outcome's check rewritten to true", file: ".modernize/outcomes.json", cause: "outcomes[runtime-target].check changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
			}},
		{name: "an outcome dropped", file: ".modernize/outcomes.json", cause: "outcomes[defects-closed] REMOVED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				body := readContract(t, ws, ".modernize/outcomes.json")
				cut := strings.Index(body, ",\n  {\"id\": \"defects-closed\"")
				writeContract(t, ws, ".modernize/outcomes.json", body[:cut]+"\n]}\n")
			}},
		{name: "the outcomes deleted", file: ".modernize/outcomes.json", cause: "DELETED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				if err := os.Remove(filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an outcome's check shadowed by a duplicate key", file: ".modernize/outcomes.json", cause: "duplicate key check",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true", "check": "sh ci/runtime-gate.sh"`)
			}},
		{name: "the outcomes swapped for a symlink to an identical copy", file: ".modernize/outcomes.json", cause: "a file at the run's base, a symlink now",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, "docs/outcomes.json", contractOutcomes)
				if err := os.Remove(filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../docs/outcomes.json", filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an outcome rewritten behind assume-unchanged, never committed", file: ".modernize/outcomes.json", cause: "outcomes[runtime-target].check changed",
			ownCommit: true,
			rewrite: func(t *testing.T, ws string, git func(...string) string) {
				git("update-index", "--assume-unchanged", ".modernize/outcomes.json")
				editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
				if st := git("status", "--porcelain"); st != "" {
					t.Fatalf("the fixture must hide the edit from git, status says %q", st)
				}
			}},
		{name: "an outcome rewritten, the base's blob answered by a replace ref", file: ".modernize/outcomes.json", cause: "outcomes[runtime-target].check changed",
			ownCommit: true,
			rewrite: func(t *testing.T, ws string, git func(...string) string) {
				real := git("rev-parse", "HEAD:.modernize/outcomes.json")
				editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
				git("commit", "-qam", "the worker rewrites a check")
				git("replace", real, git("rev-parse", "HEAD:.modernize/outcomes.json"))
				if !strings.Contains(git("show", "HEAD~1:.modernize/outcomes.json"), `"check": "true"`) {
					t.Fatal("the fixture must make the base read as the rewrite")
				}
			}},
		{name: "an outcome laundered by a clean filter: the file shows the base, git stores the rewrite", file: ".modernize/outcomes.json", cause: "outcomes[runtime-target].check changed",
			ownCommit: true,
			rewrite: func(t *testing.T, ws string, git func(...string) string) {
				filter := filepath.Join(t.TempDir(), "launder.sh")
				if err := os.WriteFile(filter, []byte("#!/bin/sh\nsed 's#\"check\": \"sh ci/runtime-gate.sh\"#\"check\": \"true\"#'\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				git("config", "filter.launder.clean", "sh "+filter)
				git("config", "filter.launder.smudge", "cat")
				writeContract(t, ws, ".gitattributes", ".modernize/outcomes.json filter=launder\n")
				git("add", ".gitattributes")
				git("add", "--renormalize", ".modernize/outcomes.json")
				git("commit", "-qm", "store a rewrite the file does not show")
				if !strings.Contains(git("show", "HEAD:.modernize/outcomes.json"), `"check": "true"`) ||
					strings.Contains(readContract(t, ws, ".modernize/outcomes.json"), `"check": "true"`) ||
					git("status", "--porcelain") != "" {
					t.Fatal("the fixture must store the rewrite, show the base, and read clean")
				}
			}},
		{name: "the outcomes swapped for a symlink, left uncommitted", file: ".modernize/outcomes.json", cause: "a file at the run's base, a symlink now",
			ownCommit: true,
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				// Uncommitted: HEAD still carries the base's file, so only the
				// working tree's own reading can see the swap the finalize banks.
				writeContract(t, ws, "docs/outcomes.json", contractOutcomes)
				if err := os.Remove(filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../docs/outcomes.json", filepath.Join(ws, ".modernize", "outcomes.json")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an outcome laundered by a clean filter, left uncommitted: git would store the rewrite", file: ".modernize/outcomes.json", cause: "outcomes[runtime-target].check changed",
			ownCommit: true,
			rewrite: func(t *testing.T, ws string, git func(...string) string) {
				// The file still shows the base's bytes and HEAD still holds
				// them: only what `git add` would store — what the finalize
				// banks — carries the rewrite.
				filter := filepath.Join(t.TempDir(), "launder.sh")
				if err := os.WriteFile(filter, []byte("#!/bin/sh\nsed 's#\"check\": \"sh ci/runtime-gate.sh\"#\"check\": \"true\"#'\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				git("config", "filter.launder.clean", "sh "+filter)
				git("config", "filter.launder.smudge", "cat")
				writeContract(t, ws, ".gitattributes", ".modernize/outcomes.json filter=launder\n")
				if readContract(t, ws, ".modernize/outcomes.json") != contractOutcomes {
					t.Fatal("the fixture must leave the file showing the base's bytes")
				}
			}},
		{name: "the outcomes created by the lot", file: ".modernize/outcomes.json", cause: "created during the lot",
			without: []string{".modernize/outcomes.json"},
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, ".modernize/outcomes.json", contractOutcomes)
			}},
		{name: "a brief goal renamed", file: ".modernize/brief.yaml", cause: "goals[supported-runtime] REMOVED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/brief.yaml", "id: supported-runtime", "id: supported-rt")
			}},
		{name: "a permission the lot granted itself", file: ".modernize/brief.yaml", cause: "permitted_changes changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/brief.yaml", "permitted_changes:\n", "permitted_changes:\n  - \"whatever the lot needs\"\n")
			}},
		{name: "the brief deleted", file: ".modernize/brief.yaml", cause: "DELETED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				if err := os.Remove(filepath.Join(ws, ".modernize", "brief.yaml")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "the arbitration doctrine edited", file: ".modernize/ARBITRAGE.md", cause: "changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/ARBITRAGE.md", "an artefact nobody chose", "anything the lot says")
			}},
		{name: "the arbitration doctrine created by the lot", file: ".modernize/ARBITRAGE.md", cause: "created during the lot",
			without: []string{".modernize/ARBITRAGE.md"},
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, ".modernize/ARBITRAGE.md", contractDoctrine)
			}},
		{name: "a registered defect dropped", file: ".modernize/defects-ledger.json", cause: "defects[D-1] REMOVED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editRegister(t, ws, func(e []any) []any { return e[1:] })
			}},
		{name: "a defect the lot does not remediate closed by the worker", file: ".modernize/defects-ledger.json", cause: "defects[D-1] changed (disposition)",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/defects-ledger.json",
					`"disposition": "open", "reason": "no lot carries it yet"`, `"disposition": "preserved", "reason": "no lot carries it yet"`)
			}},
		{name: "the register's vocabulary rewritten", file: ".modernize/defects-ledger.json", cause: "dispositions changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/defects-ledger.json", `"open": "still there"`, `"open": "counts as closed"`)
			}},
		{name: "a homonym entry added ahead of a registered one", file: ".modernize/defects-ledger.json", cause: "defects[D-1] appears 2 times",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editRegister(t, ws, func(e []any) []any {
					return append([]any{map[string]any{"id": "D-1", "disposition": "fixed"}}, e...)
				})
			}},
		{name: "even a remediated entry never leaves the register", file: ".modernize/defects-ledger.json", cause: "defects[D-2] REMOVED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editRegister(t, ws, func(e []any) []any { return []any{e[0], e[2]} })
			}},
		{name: "the register deleted", file: ".modernize/defects-ledger.json", cause: "DELETED",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				if err := os.Remove(filepath.Join(ws, ".modernize", "defects-ledger.json")); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "the lot's own licence widened", file: ".modernize/plan.yaml", cause: "L1.remediates changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/plan.yaml", "remediates: [D-2]", "remediates: [D-2, D-1]")
			}},
		{name: "a field no allowlist named: brief_targets", file: ".modernize/plan.yaml", cause: "L1.brief_targets changed",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/plan.yaml", "brief_targets: []", "brief_targets: [runtime]")
			}},
		{name: "a top-level key added to the plan", file: ".modernize/plan.yaml", cause: "top-level proposals moved",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/plan.yaml", "version: 1\n", "version: 1\nproposals:\n  - id: P1\n    question: \"answered by the lot\"\n")
			}},
		{name: "an added lot with no id", file: ".modernize/plan.yaml", cause: "carries no string id",
			rewrite: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, ".modernize/plan.yaml", contractPlan+"  - title: \"next\"\n    status: todo\n    exit_gate: [\"true\"]\n")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, base, git := programmeRepo(t, tc.without...)
			tc.rewrite(t, ws, git)
			if !tc.ownCommit {
				git("add", "-A")
				git("commit", "-qm", "the worker rewrites the contract")
			}
			refusedNaming(t, ws, modernizeLotVerify(t, script, ws, "L1", base, contractGate), tc.file, tc.cause)
		})
	}

	// Two files rewritten by one lot — an outcome's check made vacuous and a
	// brief goal renamed: each file gets its own named cause.
	t.Run("a vacuous check and a renamed goal in one lot: both named", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/outcomes.json", `"check": "sh ci/runtime-gate.sh"`, `"check": "true"`)
		editContract(t, ws, ".modernize/brief.yaml", "id: supported-runtime", "id: supported-rt")
		git("commit", "-qam", "a vacuous check and a renamed goal")
		res := modernizeLotVerify(t, script, ws, "L1", base, contractGate)
		refusedNaming(t, ws, res, ".modernize/outcomes.json", "outcomes[runtime-target].check changed")
		refusedNaming(t, ws, res, ".modernize/brief.yaml", "goals[supported-rt] added")
	})
}

func TestModernizeLotVerifyAcceptsTheLotsOwnContractWrites(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	for _, tc := range []struct {
		name    string
		without []string
		write   func(t *testing.T, ws string, git func(args ...string) string)
		// inBase: the write is part of the base itself — the lot writes
		// nothing, only the checkout differs from what git stores.
		inBase bool
		check  func(t *testing.T, res modernizeLotVerifyOut)
	}{
		{name: "nothing touched", write: func(*testing.T, string, func(...string) string) {}},
		{name: "the fix recorded on the entry the lot remediates",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/defects-ledger.json",
					`"disposition": "open", "reason": "carried by L1"`,
					`"disposition": "fixed", "reason": "the formatter reads the month", "commit": "def5678"`)
			}},
		{name: "a defect the lot found, registered",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				editRegister(t, ws, func(e []any) []any {
					return append(e, map[string]any{"id": "D-4", "found": "the L1 report", "disposition": "open", "reason": "outside L1"})
				})
			}},
		{name: "the register reindented and its keys reordered",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				var doc any
				if err := json.Unmarshal([]byte(contractRegister), &doc); err != nil {
					t.Fatal(err)
				}
				out, err := json.MarshalIndent(doc, "", "    ")
				if err != nil {
					t.Fatal(err)
				}
				writeContract(t, ws, ".modernize/defects-ledger.json", string(out)+"\n")
			}},
		{name: "the owner's files reformatted: same documents",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/outcomes.json", "{\"outcomes\": [\n", "{\n  \"outcomes\": [\n")
				editContract(t, ws, ".modernize/brief.yaml", "owner:", "# who arbitrates\nowner:")
			}},
		{name: "the register composed by the lot when the base has none",
			without: []string{".modernize/defects-ledger.json"},
			write: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, ".modernize/defects-ledger.json", `{"defects": [{"id": "D-9", "found": "the L1 report", "disposition": "fixed", "commit": "0a1b2c3"}]}`+"\n")
			}},
		{name: "a programme with the plan alone",
			without: []string{".modernize/outcomes.json", ".modernize/brief.yaml", ".modernize/ARBITRAGE.md", ".modernize/defects-ledger.json"},
			write:   func(*testing.T, string, func(...string) string) {}},
		{name: "a lot added as a proposal",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				writeContract(t, ws, ".modernize/plan.yaml", contractPlan+"  - id: L3\n    title: \"pin the locale\"\n    status: todo\n    exit_gate: [\"true\"]\n")
			}},
		{name: "the lot's own status: blocked, believed as a stop",
			write: func(t *testing.T, ws string, _ func(...string) string) {
				editContract(t, ws, ".modernize/plan.yaml", "    status: todo\n    remediates", "    status: blocked\n    remediates")
			},
			check: func(t *testing.T, res modernizeLotVerifyOut) {
				if !res.LotBlocked {
					t.Fatalf("the lot's own blocked status must stay a stop: %+v", res)
				}
			}},
		{name: "a checkout whose line endings git converts", inBase: true,
			write: func(t *testing.T, ws string, git func(...string) string) {
				writeContract(t, ws, ".gitattributes", ".modernize/** text eol=crlf\n")
				git("add", ".gitattributes")
				git("commit", "-qm", "check the contract out with CRLF")
				git("rm", "-q", "--cached", "-r", ".")
				git("reset", "-q", "--hard")
				if !strings.Contains(readContract(t, ws, ".modernize/ARBITRAGE.md"), "\r\n") {
					t.Fatal("the fixture must check the doctrine out with CRLF")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, base, git := programmeRepo(t, tc.without...)
			tc.write(t, ws, git)
			if tc.inBase {
				base = git("rev-parse", "HEAD")
			} else {
				git("add", "-A")
				git("commit", "-qm", "the lot's own write", "--allow-empty")
			}
			res := modernizeLotVerify(t, script, ws, "L1", base, contractGate)
			acceptedAndJudged(t, ws, res)
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

// TestModernizeLotVerifyRemediatesIsReadAtTheBase pins the licence to the
// owner's plan: a worker that widens its own `remediates` and closes the
// defect in the same lot is refused TWICE — once for the plan it rewrote,
// once for the entry its base never assigned to it. Each guard is asserted
// on its own cause, so neither can hide behind the other.
func TestModernizeLotVerifyRemediatesIsReadAtTheBase(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")

	t.Run("widened and used in the same lot: both causes named", func(t *testing.T) {
		ws, base, git := programmeRepo(t)
		editContract(t, ws, ".modernize/plan.yaml", "remediates: [D-2]", "remediates: [D-2, D-1]")
		editContract(t, ws, ".modernize/defects-ledger.json",
			`"disposition": "open", "reason": "no lot carries it yet"`, `"disposition": "fixed", "reason": "no lot carries it yet"`)
		git("commit", "-qam", "the worker assigns itself D-1 and closes it")
		res := modernizeLotVerify(t, script, ws, "L1", base, contractGate)
		refusedNaming(t, ws, res, ".modernize/defects-ledger.json", "defects[D-1] changed (disposition)")
		refusedNaming(t, ws, res, ".modernize/plan.yaml", "L1.remediates changed")
	})

	t.Run("an unreadable licence is a typed refusal, never an empty one", func(t *testing.T) {
		ws, _, git := programmeRepo(t)
		editContract(t, ws, ".modernize/plan.yaml", "remediates: [D-2]", "remediates: {D-2: true}")
		git("commit", "-qam", "the owner writes remediates as a mapping")
		base := git("rev-parse", "HEAD")
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate, nil)
		if exit != 0 || !res.Unreadable || !strings.Contains(res.LogTail, "remediates on lot L1") {
			t.Fatalf("exit %d contract_unreadable=%v log %q — want CONTRACT_UNREADABLE naming remediates", exit, res.Unreadable, res.LogTail)
		}
		if gateRan(ws) {
			t.Fatal("the gate ran under a licence the node could not read")
		}
	})
}

// TestModernizeGateReadsHistoryAsCommitted: a replace ref (refs/replace/) is
// a local ref the worker can write, and git honours it on every read. Each
// node that judges or writes from the base or HEAD reads history as
// committed, and each is exercised on its own: the references' diff in
// lot_verify, and the plan mark_done commits under the gate's subject.
func TestModernizeGateReadsHistoryAsCommitted(t *testing.T) {
	requireModernizeTools(t)

	t.Run("lot_verify: a moved reference, the base's tree answered by the moved one", func(t *testing.T) {
		script := toolScript(t, "modernize/main.bot", "lot_verify")
		ws, base, git := programmeRepo(t)
		writeContract(t, ws, ".golden-master/refs/001.txt", "STATUS 500\n")
		git("commit", "-qam", "the worker moves a reference")
		// A blob-level replace does not fool a diff, which compares ids; the
		// base's whole tree answered by HEAD's does.
		git("replace", git("rev-parse", base+"^{tree}"), git("rev-parse", "HEAD^{tree}"))
		if git("diff", "--name-only", base, "--", ".golden-master") != "" {
			t.Fatal("the fixture must hide the move from a diff that honours replace refs")
		}
		res := modernizeLotVerify(t, script, ws, "L1", base, "true")
		if res.RefsUntouched || !strings.Contains(res.LogTail, ".golden-master/refs/001.txt") {
			t.Fatalf("a moved reference read through a replace ref must still be named: refs_untouched=%v (log %s)", res.RefsUntouched, res.LogTail)
		}
	})

	t.Run("mark_done: the flip lands on the plan as committed", func(t *testing.T) {
		script := toolScript(t, "modernize/main.bot", "mark_done")
		ws, base, git := programmeRepo(t)
		real := git("rev-parse", "HEAD:.modernize/plan.yaml")
		forged := filepath.Join(t.TempDir(), "plan.yaml")
		if err := os.WriteFile(forged, []byte(strings.Replace(contractPlan, "    depends_on: [L1]\n    exit_gate:\n      - \"true\"\n", "    depends_on: [L1]\n    exit_gate: []\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		git("replace", real, git("hash-object", "-w", forged))
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked {
			t.Fatalf("mark_done did not mark: %+v", res)
		}
		landed := git("--no-replace-objects", "show", "HEAD:.modernize/plan.yaml")
		if !strings.Contains(landed, "    depends_on: [L1]\n    exit_gate:\n      - \"true\"") || strings.Contains(landed, "exit_gate: []") {
			t.Fatalf("the gate committed a plan the owner never wrote:\n%s", landed)
		}
		if strings.Count(landed, "status: done") != 1 {
			t.Fatalf("the gate's commit must carry exactly one done:\n%s", landed)
		}
	})
}
