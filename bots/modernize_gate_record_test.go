package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gate's predicate over a record in the contract's directory — the whole
// command `test -s <path>` or `[ -s <path> ]` — is judged on what lands. The
// gate's commands read the working tree; `done` is written on the commit,
// and on a cloud run HEAD alone is the landing. A record the commit does not
// carry as a non-empty file fails the gate, by name.
func TestModernizeGateRecordPredicateHoldsOnWhatLands(t *testing.T) {
	requireModernizeTools(t)
	const record = ".modernize/sweeps/L1.md"
	const gate = "test -s " + record

	failsOn := func(t *testing.T, got landed, why string) {
		t.Helper()
		if got.converged || got.marked || got.stop {
			t.Fatalf("lot L1 converged on a record that does not land: converged=%v marked=%v stop=%v log=%s",
				got.converged, got.marked, got.stop, got.verdict.LogTail)
		}
		if got.verdict.GatePassed {
			t.Fatalf("the gate passed on a record that does not land: log=%s", got.verdict.LogTail)
		}
		if !strings.Contains(got.verdict.LogTail, record+": the gate's predicate") || !strings.Contains(got.verdict.LogTail, why) {
			t.Fatalf("the refusal does not name the record and %q: %s", why, got.verdict.LogTail)
		}
	}
	t.Run("the record left uncommitted", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n\nthe drift probes\n")
		failsOn(t, landLot(t, ws, base, gate, nil), "the record is in no commit")
	})
	t.Run("a root .gitignore keeps the record out of every commit", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, ".gitignore", ".modernize/sweeps/\n")
		git("add", ".gitignore")
		git("commit", "-qm", "chore: ignore the sweep scratch")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, record, "# L1 sweep\n\nthe drift probes\n")
		git("add", "-A")
		failsOn(t, landLot(t, ws, base, gate, nil), "git ignores it")
	})
	t.Run("the record committed, removed by a later commit, put back on disk", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n")
		git("add", "-A")
		git("commit", "-qm", "the sweep record")
		git("rm", "-q", record)
		git("commit", "-qm", "drop it again")
		writeContract(t, ws, record, "# L1 sweep\n")
		failsOn(t, landLot(t, ws, base, gate, nil), "the record is in no commit")
	})
	t.Run("the record committed empty, written on disk since", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, record, "")
		git("add", "-A")
		git("commit", "-qm", "an empty sweep record")
		writeContract(t, ws, record, "# L1 sweep\n")
		failsOn(t, landLot(t, ws, base, gate, nil), "the commit carries it empty")
	})
	t.Run("the record committed as a symlink", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/L1-report.md", "# L1\n")
		if err := os.MkdirAll(filepath.Join(ws, ".modernize", "sweeps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../L1-report.md", filepath.Join(ws, record)); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "the sweep record, linked")
		failsOn(t, landLot(t, ws, base, gate, nil), "a symlink")
	})
	t.Run("the bracket form, quoted, the record left uncommitted", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n")
		failsOn(t, landLot(t, ws, base, `[ -s "`+record+`" ]`, nil), "the record is in no commit")
	})

	// The other face: a record the commit carries lands; a predicate over a
	// path outside the contract's directory, or a command that is not the
	// predicate, is the gate's own business.
	landsWith := func(t *testing.T, got landed) {
		t.Helper()
		if !got.converged || !got.marked {
			t.Fatalf("a record that lands was refused: converged=%v marked=%v notice=%q log=%s",
				got.converged, got.marked, got.notice, got.verdict.LogTail)
		}
	}
	t.Run("the record committed: it lands", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n")
		git("add", "-A")
		git("commit", "-qm", "the sweep record")
		landsWith(t, landLot(t, ws, base, gate, nil))
	})
	t.Run("an earlier attempt's record at HEAD, refreshed on disk: it lands", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, record, "stale\n")
		git("add", "-A")
		git("commit", "-qm", "the previous attempt's record")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, record, "# L1 sweep\n\nthe fresh record\n")
		landsWith(t, landLot(t, ws, base, gate, nil))
	})
	t.Run("a predicate over a path outside the contract's directory", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t)
		writeContract(t, ws, "build/out.txt", "a build output nobody commits\n")
		landsWith(t, landLot(t, ws, base, "test -s build/out.txt", nil))
	})
	t.Run("a predicate over a directory the commit carries files under", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/L1-captures/one.json", "{}\n")
		git("add", "-A")
		git("commit", "-qm", "the captures")
		landsWith(t, landLot(t, ws, base, "test -s .modernize/L1-captures", nil))
	})
	t.Run("a command that carries the predicate among other words", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n")
		landsWith(t, landLot(t, ws, base, gate+" && true", nil))
	})
}

// A conversion git cannot apply to a contract path — a `working-tree-encoding`
// the file does not satisfy, a clean filter that fails — is refused by name,
// with the attribute, and goes back to the worker: the contract is readable,
// what git would store from it is not.
func TestModernizeContractNamesTheConversionItCannotStore(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	withRecord := func(t *testing.T) (string, string, func(args ...string) string) {
		t.Helper()
		ws, _, git := programmeOfLots(t, "L0")
		writeContract(t, ws, ".modernize/L0-report.md", "what L0 found\n")
		git("add", "-A")
		git("commit", "-qm", "L0's record, landed")
		return ws, git("rev-parse", "HEAD"), git
	}
	t.Run("a working-tree-encoding the files do not satisfy", func(t *testing.T) {
		ws, base, _ := withRecord(t)
		writeContract(t, ws, ".gitattributes", "*.md working-tree-encoding=UTF-16\n")
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate, nil)
		if exit != 0 || res.Unreadable {
			t.Fatalf("a conversion the files do not satisfy read as an unreadable contract: exit=%d log=%s", exit, res.LogTail)
		}
		refusedNaming(t, ws, res, ".modernize/ARBITRAGE.md", "`working-tree-encoding=UTF-16`")
		refusedNaming(t, ws, res, ".modernize/L0-report.md", "`working-tree-encoding=UTF-16`")
		for _, entry := range res.ContractRewrite {
			if strings.HasPrefix(entry, ".modernize/L0-report.md: changed") {
				t.Fatalf("a file git cannot store was also called a rewrite: %q", res.ContractRewrite)
			}
		}
	})
	t.Run("a clean filter that fails", func(t *testing.T) {
		ws, base, git := withRecord(t)
		writeContract(t, ws, ".gitattributes", ".modernize/brief.yaml filter=broken\n")
		git("config", "filter.broken.clean", "false")
		git("config", "filter.broken.required", "true")
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate, nil)
		if exit != 0 || res.Unreadable {
			t.Fatalf("a failing filter read as an unreadable contract: exit=%d log=%s", exit, res.LogTail)
		}
		refusedNaming(t, ws, res, ".modernize/brief.yaml", "conversion attribute `filter=broken`")
	})
	t.Run("an encoding put on the contract after the verdict", func(t *testing.T) {
		ws, base, _ := withRecord(t)
		got := landLot(t, ws, base, "true", func() {
			writeContract(t, ws, ".gitattributes", "*.md working-tree-encoding=UTF-16\n")
		})
		if got.marked || !got.refused || !strings.Contains(got.notice, "conversion attribute(s) `working-tree-encoding=UTF-16`") {
			t.Fatalf("mark_done did not name the conversion it could not apply: marked=%v refused=%v notice=%q",
				got.marked, got.refused, got.notice)
		}
	})
}

// The verdict rides the checkpoint and the edges on every pass: it names the
// table's files and holds the rest of the directory — the lot's captures
// included — by count and digest, whatever their number.
func TestModernizeVerdictIsBoundedWhateverTheDirectory(t *testing.T) {
	requireModernizeTools(t)
	ws, base, _ := programmeOfLots(t)
	dir := filepath.Join(ws, ".modernize", "L1-captures")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("c%05d.json", i)), []byte(fmt.Sprintf("{\"i\": %d}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := landLot(t, ws, base, "true", nil)
	if !got.marked {
		t.Fatalf("a lot with 2000 captures was refused: %q %q", got.verdict.ContractRewrite, got.notice)
	}
	if n := len(got.verdict.ContractTree); n > 2048 {
		t.Fatalf("the verdict grows with the directory: contract_tree is %d bytes for 2000 captures", n)
	}
}

// `--stdin-paths` reads one path per line, and a line loses its trailing CR:
// batched, a name ending in CR would be answered by its twin without one. An
// earlier lot's record named so, rewritten while the lot writes its twin with
// the record's old bytes, is still a rewrite.
func TestModernizeContractHashesANameEndingInCRAsItself(t *testing.T) {
	requireModernizeTools(t)
	ws, _, git := programmeOfLots(t)
	writeContract(t, ws, ".modernize/L0-note.md\r", "what L0 noted\n")
	git("add", "-A")
	git("commit", "-qm", "an earlier lot's record")
	base := git("rev-parse", "HEAD")
	writeContract(t, ws, ".modernize/L0-note.md\r", "nothing to see\n")
	writeContract(t, ws, ".modernize/L0-note.md", "what L0 noted\n")
	res := modernizeLotVerify(t, toolScript(t, "modernize/main.bot", "lot_verify"), ws, "L1", base, contractGate)
	refusedNaming(t, ws, res, ".modernize/L0-note.md\r", "changed — beside the plan")
}
