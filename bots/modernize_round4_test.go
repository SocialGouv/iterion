package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// A record's path is judged as the commit carries it: through the committed
// links of its way. The base may ship `sweeps` as a link to the owner's
// records directory, and the lot records behind it, as the base dictates —
// what the ADR freezes is the link, and the record lives at the path the
// commit really holds.

func TestModernizeRecordBehindACommittedLinkLands(t *testing.T) {
	requireModernizeTools(t)
	t.Run("behind a link the base holds", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		if err := os.MkdirAll(filepath.Join(ws, ".modernize", "records"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeContract(t, ws, ".modernize/records/L1.md", "# L1 sweep\n\nproduced behind the base's link\n")
		if err := os.Symlink("records", filepath.Join(ws, ".modernize", "sweeps")); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "the owner's records dir; sweeps is a link, as the base ships it")
		base := git("rev-parse", "HEAD")
		got := landLot(t, ws, base, "test -s .modernize/sweeps/L1.md", nil)
		if !got.converged || !got.marked {
			t.Fatalf("a record committed behind a link the base holds was refused: converged=%v marked=%v log=%s",
				got.converged, got.marked, got.verdict.LogTail)
		}
		if !got.verdict.GatePassed {
			t.Fatalf("the gate did not pass on a record that lands: %s", got.verdict.LogTail)
		}
	})
	t.Run("behind the lot's own link", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/L1-captures/one.json", "{\"probe\": \"drift-1\"}\n")
		if err := os.Symlink("L1-captures", filepath.Join(ws, ".modernize", "L1-rec")); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "L1's captures, linked under its own id")
		base := git("rev-parse", "HEAD")
		got := landLot(t, ws, base, "test -s .modernize/L1-rec/one.json", nil)
		if !got.converged || !got.marked {
			t.Fatalf("the lot's own record, tested through the lot's own link, was refused: converged=%v marked=%v log=%s",
				got.converged, got.marked, got.verdict.LogTail)
		}
	})
	t.Run("a record each tree carries at its own path", func(t *testing.T) {
		// The lot reorganizes its own records and lands them through the
		// bank: HEAD carries the sweep behind the committed link, the
		// landing carries it at the literal path the working tree holds.
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/L1-captures/sweep.md", "# L1 sweep\n\nthe drift probes\n")
		if err := os.Symlink("L1-captures", filepath.Join(ws, ".modernize", "L1-rec")); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "the sweep behind the lot's own link")
		base := git("rev-parse", "HEAD")
		// The reorganization, uncommitted: the link replaced by a real
		// directory, the file moved into it.
		if err := os.Remove(filepath.Join(ws, ".modernize", "L1-rec")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws, ".modernize", "L1-rec"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(ws, ".modernize", "L1-captures", "sweep.md"),
			filepath.Join(ws, ".modernize", "L1-rec", "sweep.md")); err != nil {
			t.Fatal(err)
		}
		got := landLot(t, ws, base, "test -s .modernize/L1-rec/sweep.md", nil)
		if !got.converged || !got.marked {
			t.Fatalf("a record present in both landing trees was refused: converged=%v marked=%v log=%s",
				got.converged, got.marked, got.verdict.LogTail)
		}
	})
	t.Run("a spelling through ., the record committed", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/sweeps/L1.md", "# L1 sweep\n")
		git("add", "-A")
		git("commit", "-qm", "the sweep record")
		base := git("rev-parse", "HEAD")
		got := landLot(t, ws, base, "test -s ./.modernize/sweeps/L1.md", nil)
		if !got.converged || !got.marked {
			t.Fatalf("a committed record tested as ./… was refused: converged=%v marked=%v log=%s",
				got.converged, got.marked, got.verdict.LogTail)
		}
	})
	t.Run("a frozen record whose name holds a newline", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, ".modernize/L0-note\n2.md", "what L0 noted\n")
		git("add", "-A")
		git("commit", "-qm", "an earlier lot's record, newline in the name")
		base := git("rev-parse", "HEAD")
		got := landLot(t, ws, base, "true", nil)
		if !got.converged || !got.marked {
			t.Fatalf("a lot over a frozen record whose name holds a newline was refused: converged=%v marked=%v rewrites=%q log=%s",
				got.converged, got.marked, got.verdict.ContractRewrite, got.verdict.LogTail)
		}
	})
}

// The predicate holds on what LANDS. A cloud run pushes HEAD; a run with a
// working tree lands HEAD plus the bank's commit, and the bank stages `git
// add -A :/` — a record the index has dropped, and a root .gitignore keeps
// the bank from re-adding, never lands, whatever HEAD carries.
func TestModernizeRecordThatCannotLandFailsTheGate(t *testing.T) {
	requireModernizeTools(t)
	const record = ".modernize/sweeps/L1.md"
	t.Run("removed from the index behind a root .gitignore", func(t *testing.T) {
		ws, _, git := programmeOfLots(t)
		writeContract(t, ws, record, "# L1 sweep\n\nthe drift probes\n")
		git("add", "-A")
		git("commit", "-qm", "the previous attempt's record")
		base := git("rev-parse", "HEAD")
		writeContract(t, ws, ".gitignore", ".modernize/sweeps/\n")
		git("add", ".gitignore")
		git("rm", "--cached", "-q", record)
		got := landLot(t, ws, base, "test -s "+record, nil)
		if got.converged || got.marked || got.stop {
			t.Fatalf("lot L1 converged on a record that cannot land: converged=%v marked=%v stop=%v",
				got.converged, got.marked, got.stop)
		}
		if got.verdict.GatePassed {
			t.Fatalf("the gate passed on a record that cannot land: %s", got.verdict.LogTail)
		}
		if !containsAll(got.verdict.LogTail, record+": the gate's predicate", "would not carry") {
			t.Fatalf("the refusal does not name the record and the landing that loses it: %s", got.verdict.LogTail)
		}
	})
	t.Run("the commit carries it as a directory", func(t *testing.T) {
		ws, base, git := programmeOfLots(t)
		writeContract(t, ws, record+"/junk.txt", "not a sweep record\n")
		git("add", "-A")
		git("commit", "-qm", "a directory where the record's name goes")
		got := landLot(t, ws, base, "test -s "+record, nil)
		if got.converged || got.marked {
			t.Fatalf("a directory where the record's name goes was taken for a landed record: log=%s", got.verdict.LogTail)
		}
		if !containsAll(got.verdict.LogTail, "as a directory, not as a file") {
			t.Fatalf("the refusal does not name the directory: %s", got.verdict.LogTail)
		}
	})
}

// The bank's staging mirrors the engine's — including its probe: git refuses
// an exclusion whose path is ignored, the bank drops exactly that exclusion,
// and the simulation must too. An exclusion spelled where git would refuse
// makes the staging fail, and with it every lot. Judge lot_verify directly:
// the shape is the env the engine provisions on every tool process.
func TestModernizeBankedTreeProbesItsExclusions(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	ws, _, git := programmeOfLots(t)
	writeContract(t, ws, ".gitignore", ".claude/\n")
	git("add", ".gitignore")
	git("commit", "-qm", "the mirror stays out of the tree")
	base := git("rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate,
		append(os.Environ(), "ITERION_TREE_NOISE=':(exclude,top).claude'"))
	if exit != 0 || res.Unreadable {
		t.Fatalf("a lot in a repository that ignores the mirror could not converge: exit=%d unreadable=%v log=%s",
			exit, res.Unreadable, res.LogTail)
	}
	if len(res.ContractRewrite) != 0 || res.GatePassed != true {
		t.Fatalf("an untouched lot was judged over: rewritten=%q gate=%v log=%s",
			res.ContractRewrite, res.GatePassed, res.LogTail)
	}
}

// The exclusion probe asks the LITERAL path, as the engine's own staging
// probe does: a Prefix entry's pathspec ends in a wildcard, and git keys the
// add-refusal on the literal part. A file named exactly like the prefix,
// untracked and ignored, must not stop every lot from converging.
func TestModernizeBankedTreeProbesThePrefixNotTheWildcard(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	ws, _, git := programmeOfLots(t)
	writeContract(t, ws, ".gitignore", ".iterion-script-\n")
	git("add", ".gitignore")
	git("commit", "-qm", "the literal scratch name is ignored")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(ws, ".iterion-script-"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate,
		append(os.Environ(), "ITERION_TREE_NOISE=':(exclude,top).iterion-script-*'"))
	if exit != 0 || res.Unreadable {
		t.Fatalf("a lot cannot converge when a literal .iterion-script- file exists: exit=%d unreadable=%v log=%s",
			exit, res.Unreadable, res.LogTail)
	}
	t.Run("control: the wildcard exclusion on an absent path is harmless", func(t *testing.T) {
		ws, base, _ := programmeOfLots(t)
		res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, contractGate,
			append(os.Environ(), "ITERION_TREE_NOISE=':(exclude,top).iterion-script-*'"))
		if exit != 0 || res.Unreadable {
			t.Fatalf("control failed: exit=%d unreadable=%v log=%s", exit, res.Unreadable, res.LogTail)
		}
	})
}

// mark_done's own writes — the ref update that lands the gate's word, and the
// index entry that follows it — run no hook: a hook in the run's tree must
// not refuse them, or ride them.
func TestModernizeMarkDoneWritesRunNoRepositoryHook(t *testing.T) {
	requireModernizeTools(t)
	ws, base, _ := programmeOfLots(t, "L22")
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"reference-transaction": "#!/bin/sh\n[ \"$1\" = prepared ] && exit 1\nexit 0\n",
		"post-index-change":     "#!/bin/sh\nexit 1\n",
		"pre-commit":            "#!/bin/sh\nexit 1\n",
		"post-commit":           "#!/bin/sh\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := landLot(t, ws, base, "true", nil)
	if !got.marked || got.refused {
		t.Fatalf("the gate's word could not land past the run's hooks: marked=%v refused=%v notice=%q",
			got.marked, got.refused, got.notice)
	}
	head := gittest.Run(t, ws, "rev-parse", "HEAD")
	if subject := gittest.Run(t, ws, "log", "-1", "--format=%s", head); subject[:3] != "L1:" {
		t.Fatalf("HEAD is %q, not the gate's own commit: a hook moved it", subject)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
