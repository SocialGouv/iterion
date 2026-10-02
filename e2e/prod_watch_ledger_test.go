package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The ledger bound (#2064): a ledger file past ledger.max_bytes is abandoned
// under a generation name and a fresh one starts (never a line-trim — a
// merge=union file grafts a concurrent push's side back over a trim); the
// newest ledger.keep rotations stay tracked, older ones leave the tree, and
// the rotation is SAID (summary, commit output, commit message), never silent.

// ledgerRun feeds commit_state once with state_commit=false: rotation is a
// working-tree act, independent of the commit. preFill maps state-dir file
// names to the bytes they hold BEFORE the tick runs.
func ledgerRun(t *testing.T, wf *ir.Workflow, h *pwHarness, gen int, ledger map[string]any,
	delta, tickLine string, preFill map[string]string) map[string]any {
	t.Helper()
	st := filepath.Join(h.scratch, "state_next.json")
	if err := os.WriteFile(st, []byte(fmt.Sprintf(`{"version":1,"generation":%d,"cursors":{"loki":{}},"incidents":{},"health":{}}`, gen+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	if err := os.WriteFile(al, []byte(delta), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := filepath.Join(h.scratch, "tick.json")
	if err := os.WriteFile(tk, []byte(tickLine), 0o644); err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(h.ws, ".prod-watch")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range preFill {
		if err := os.WriteFile(filepath.Join(sd, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": gen, "state_commit": false,
		"workspace": h.ws, "state_dir": ".prod-watch", "ledger": ledger}, nil, nil))
	if err != nil {
		t.Fatalf("commit_state: %v %s", err, stderr)
	}
	return out
}

// bigLines builds n lines of ~100 bytes each — the shape a flooded alert log
// takes (a fold note's line carries its members' names).
func bigLines(n int) string {
	s := ""
	for k := 0; k < n; k++ {
		s += fmt.Sprintf(`{"at":"2026-10-02T0%d:00:00Z","fp":"loki:t%03d","title":"minted pattern %03d","count":3}`+"\n", k%10, k, k)
	}
	return s
}

func sdPath(h *pwHarness, name string) string { return filepath.Join(h.ws, ".prod-watch", name) }

func mustExist(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatal(err)
	return false
}

// TestProdWatch_LedgerRotationAbandonsAFullFile: the file past the bound is
// renamed to the generation, the fresh one starts EMPTY (the delta of THIS
// tick went into the rotated file — the append precedes the bound), and the
// rotation is said in the summary and the output.
func TestProdWatch_LedgerRotationAbandonsAFullFile(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := bigLines(3)
	out := ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 100, "keep": 8},
		`{"at":"x","fp":"probe:api"}`+"\n", `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": old})
	rots, _ := out["rotations"].([]any)
	if len(rots) != 1 {
		t.Fatalf("one rotation, said in the output: %v", out)
	}
	r := rots[0].(map[string]any)
	if r["ledger"] != "alertlog.jsonl" || r["rotated_to"] != "alertlog-5.jsonl" {
		t.Fatalf("the rotation names the file and the generation: %v", r)
	}
	if got, _ := os.ReadFile(sdPath(h, "alertlog-5.jsonl")); string(got) != old+`{"at":"x","fp":"probe:api"}`+"\n" {
		t.Fatalf("the rotated file holds the past AND this tick's line: %q", got)
	}
	if !mustExist(t, sdPath(h, "alertlog.jsonl")) {
		t.Fatalf("the fresh file must EXIST after the tick — the ledgers exist after every tick, empty or not")
	}
	if fresh, _ := os.ReadFile(sdPath(h, "alertlog.jsonl")); len(fresh) != 0 {
		t.Fatalf("the fresh file starts empty, the next append is its first: %q", fresh)
	}
	if s, _ := out["summary"].(string); !strings.Contains(s, "rotated alertlog.jsonl -> alertlog-5.jsonl") {
		t.Fatalf("the trim is said in the summary: %v", out["summary"])
	}
	// ticks.jsonl sits under the same bound on its own: untouched here, so
	// no rotation of it either.
	if mustExist(t, sdPath(h, "ticks-5.jsonl")) {
		t.Fatalf("ticks.jsonl under the bound must not rotate")
	}
}

// TestProdWatch_LedgerBelowTheBoundNeverRotates: quiet ledgers stay the two
// files they have always been.
func TestProdWatch_LedgerBelowTheBoundNeverRotates(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 100000, "keep": 8},
		`{"at":"x","fp":"probe:api"}`+"\n", `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": bigLines(1)})
	if rots, _ := out["rotations"].([]any); len(rots) != 0 {
		t.Fatalf("a ledger under its bound rotates nothing: %v", rots)
	}
	for _, name := range []string{"alertlog-5.jsonl", "ticks-5.jsonl"} {
		if mustExist(t, sdPath(h, name)) {
			t.Fatalf("%s must not exist below the bound", name)
		}
	}
	if s, _ := out["summary"].(string); strings.Contains(s, "rotated") {
		t.Fatalf("an unrotated tick does not say rotated: %v", out["summary"])
	}
}

// TestProdWatch_LedgerExactlyAtTheBoundStays: the bound judges the file AFTER
// the append (this tick's line counts), and only a size STRICTLY past the
// bound rotates — one landing exactly on it stays.
func TestProdWatch_LedgerExactlyAtTheBoundStays(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	at := bigLines(2)
	delta := `{"at":"x","fp":"probe:api"}` + "\n"
	out := ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": len(at) + len(delta), "keep": 8},
		delta, `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": at})
	if rots, _ := out["rotations"].([]any); len(rots) != 0 {
		t.Fatalf("a file landing exactly ON the bound must not rotate: %v", rots)
	}
	if got, _ := os.ReadFile(sdPath(h, "alertlog.jsonl")); len(got) != len(at)+len(delta) {
		t.Fatalf("the current file keeps its past and this tick's line: %d bytes", len(got))
	}
}

// TestProdWatch_LedgerZeroNeverRotates: max_bytes 0 is the operator's explicit
// "no bound" — a mutant reading it as "rotate always" or as the default reddens.
func TestProdWatch_LedgerZeroNeverRotates(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 0, "keep": 8},
		`{"at":"x","fp":"probe:api"}`+"\n", `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": bigLines(50)})
	if rots, _ := out["rotations"].([]any); len(rots) != 0 {
		t.Fatalf("max_bytes 0 is no bound: %v", rots)
	}
	if mustExist(t, sdPath(h, "alertlog-5.jsonl")) {
		t.Fatalf("max_bytes 0 must not rotate a boundless file")
	}
}

// TestProdWatch_LedgerDefaultsBindWithoutAKey: an input carrying no ledger map
// (null, as an unwired field arrives) takes the DEFAULT bound — a deployment
// with no opinion still gets a bounded tree.
func TestProdWatch_LedgerDefaultsBindWithoutAKey(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 5, nil, "", `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": bigLines(35000)})
	if rots, _ := out["rotations"].([]any); len(rots) != 1 {
		t.Fatalf("the default 2 MB binds a 3.5 MB ledger with no config at all: %v", out["rotations"])
	}
}

// TestProdWatch_LedgerNameCollisionTakesASuffix: a resumed tick re-rotating at
// the same generation must not overwrite the rotation already on disk.
func TestProdWatch_LedgerNameCollisionTakesASuffix(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 7, map[string]any{"max_bytes": 100, "keep": 8},
		`{"at":"x","fp":"probe:api"}`+"\n", `{"at":"x","generation":8}`,
		map[string]string{"alertlog.jsonl": bigLines(3), "alertlog-7.jsonl": "prior rotation\n"})
	if r, _ := out["rotations"].([]any); len(r) != 1 || r[0].(map[string]any)["rotated_to"] != "alertlog-7-2.jsonl" {
		t.Fatalf("the name taken by the aborted tick's rotation is kept, the new one suffixes: %v", out["rotations"])
	}
	if got, _ := os.ReadFile(sdPath(h, "alertlog-7.jsonl")); string(got) != "prior rotation\n" {
		t.Fatalf("the prior rotation must not be overwritten: %q", got)
	}
	if !mustExist(t, sdPath(h, "alertlog-7-2.jsonl")) {
		t.Fatalf("the suffix rotation must exist")
	}
}

// TestProdWatch_LedgerFreshFileTakesTheNextAppend: the rotation froze the old
// file — the next tick's lines land in the fresh one, the closed rotation is
// never appended again.
func TestProdWatch_LedgerFreshFileTakesTheNextAppend(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 100, "keep": 8},
		`{"at":"x","fp":"one"}`+"\n", `{"at":"x","generation":6}`, map[string]string{"alertlog.jsonl": bigLines(3)})
	frozen, err := os.ReadFile(sdPath(h, "alertlog-5.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ledgerRun(t, wf, h, 6, map[string]any{"max_bytes": 100, "keep": 8},
		`{"at":"y","fp":"two"}`+"\n", `{"at":"y","generation":7}`, nil)
	if got, _ := os.ReadFile(sdPath(h, "alertlog-5.jsonl")); string(got) != string(frozen) {
		t.Fatalf("a closed rotation is frozen: %q", got)
	}
	if got, _ := os.ReadFile(sdPath(h, "alertlog.jsonl")); string(got) != `{"at":"y","fp":"two"}`+"\n" {
		t.Fatalf("the fresh file takes the next append: %q", got)
	}
}

// TestProdWatch_LedgerAttributesCoverTheRotations: the merge=union pattern
// covers the rotation names, and topping the attributes file up is idempotent
// — a second tick must not grow it.
func TestProdWatch_LedgerAttributesCoverTheRotations(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 0, "keep": 8}, "", `{"at":"x","generation":6}`, nil)
	ga, err := os.ReadFile(sdPath(h, ".gitattributes"))
	if err != nil || !strings.Contains(string(ga), "alertlog*.jsonl merge=union") || !strings.Contains(string(ga), "ticks*.jsonl merge=union") {
		t.Fatalf("the union pattern must cover the rotations: %q %v", ga, err)
	}
	ledgerRun(t, wf, h, 6, map[string]any{"max_bytes": 0, "keep": 8}, "", `{"at":"y","generation":7}`, nil)
	ga2, _ := os.ReadFile(sdPath(h, ".gitattributes"))
	if strings.Count(string(ga2), "alertlog*.jsonl") != 1 || string(ga) != string(ga2) {
		t.Fatalf("the attributes file is written once: %q", ga2)
	}
}

// TestProdWatch_LedgerTicksLedgerRotatesToo: the tick ledger is under the same
// bound — its generation-to-time map survives as whole files, not as a trim.
func TestProdWatch_LedgerTicksLedgerRotatesToo(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tick := `{"at":"x","generation":6,"lines":1}` + "\n"
	out := ledgerRun(t, wf, h, 5, map[string]any{"max_bytes": 100, "keep": 8},
		"", tick, map[string]string{"ticks.jsonl": bigLines(3)})
	rots, _ := out["rotations"].([]any)
	if len(rots) != 1 || rots[0].(map[string]any)["ledger"] != "ticks.jsonl" || rots[0].(map[string]any)["rotated_to"] != "ticks-5.jsonl" {
		t.Fatalf("the tick ledger rotates under the same bound: %v", rots)
	}
	if got, _ := os.ReadFile(sdPath(h, "ticks-5.jsonl")); !strings.Contains(string(got), `"generation":6`) || !strings.Contains(string(got), bigLines(1)[:30]) {
		t.Fatalf("the rotated tick ledger holds the past and this tick's line: %q", got)
	}
	if fresh, _ := os.ReadFile(sdPath(h, "ticks.jsonl")); len(fresh) != 0 {
		t.Fatalf("the fresh tick ledger starts empty: %q", fresh)
	}
}

// TestProdWatch_LedgerKeepPrunesTheOldestRotations: with keep=2 and a third
// rotation arriving, the OLDEST leaves the tree AND the git index (a pruned
// file is untracked, its versions stay in history), the rotation said in the
// commit message that carries it.
func TestProdWatch_LedgerKeepPrunesTheOldestRotations(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string { return gittest.Run(t, h.ws, args...) }
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	sd := filepath.Join(h.ws, ".prod-watch")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(sdPath(h, "alertlog-1.jsonl"), []byte("rot one\n"), 0o644)
	_ = os.WriteFile(sdPath(h, "alertlog-2.jsonl"), []byte("rot two\n"), 0o644)
	git("add", ".prod-watch/alertlog-1.jsonl", ".prod-watch/alertlog-2.jsonl")
	git("commit", "-q", "-m", "seed rotations")
	// The tick: its delta pushes the current file past the bound → the third
	// rotation arrives, keep=2 prunes alertlog-1.jsonl.
	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":4,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, []byte(`{"at":"x","fp":"probe:api"}`+"\n"), 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","generation":4}`), 0o644)
	_ = os.WriteFile(sdPath(h, "alertlog.jsonl"), []byte(bigLines(3)), 0o644)
	out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": 3, "state_commit": true,
		"workspace": h.ws, "state_dir": ".prod-watch", "ledger": map[string]any{"max_bytes": 100, "keep": 2}},
		nil, nil), gittest.Env())
	if err != nil || out["committed"] != true {
		t.Fatalf("the pruning tick must commit: %v %s %v", out, stderr, err)
	}
	if mustExist(t, sdPath(h, "alertlog-1.jsonl")) {
		t.Fatalf("the oldest rotation must leave the working tree")
	}
	for _, name := range []string{"alertlog-2.jsonl", "alertlog-3.jsonl", "alertlog.jsonl"} {
		if !mustExist(t, sdPath(h, name)) {
			t.Fatalf("%s must stay", name)
		}
	}
	tracked := git("ls-files", ".prod-watch")
	if strings.Contains(tracked, "alertlog-1.jsonl") {
		t.Fatalf("the pruned rotation must be untracked: %q", tracked)
	}
	for _, name := range []string{"alertlog-2.jsonl", "alertlog-3.jsonl"} {
		if !strings.Contains(tracked, name) {
			t.Fatalf("%s must stay tracked: %q", name, tracked)
		}
	}
	if rots, _ := out["rotations"].([]any); len(rots) != 1 {
		t.Fatalf("one rotation: %v", out["rotations"])
	}
	// The push reached the remote and SAID the rotation.
	log := git("log", "--oneline", "origin/main")
	if !strings.Contains(log, "rotated") {
		t.Fatalf("the commit message carries the rotation: %s", log)
	}
}

// TestProdWatch_LedgerKeepZeroKeepsEveryRotation: keep 0 is "no pruning" —
// every rotation stays in the tree, the growth is the operator's.
func TestProdWatch_LedgerKeepZeroKeepsEveryRotation(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 3, map[string]any{"max_bytes": 100, "keep": 0}, "",
		`{"at":"x","generation":4}`,
		map[string]string{"alertlog.jsonl": bigLines(3), "alertlog-1.jsonl": "one\n", "alertlog-2.jsonl": "two\n"})
	rots, _ := out["rotations"].([]any)
	if len(rots) != 1 || len(rots[0].(map[string]any)["removed"].([]any)) != 0 {
		t.Fatalf("keep 0 removes nothing: %v", rots)
	}
	for _, name := range []string{"alertlog-1.jsonl", "alertlog-2.jsonl", "alertlog-3.jsonl"} {
		if !mustExist(t, sdPath(h, name)) {
			t.Fatalf("%s must stay with keep 0", name)
		}
	}
}

// TestProdWatch_LedgerRotationThroughAHostileIgnoreRuleRefuses: an ignore
// rule matching ONLY the rotation names (the base files stay clean) would
// drop every rotation out of git while the summary still said "pushed" —
// the tick refuses by name instead, and the rotation stays on disk (the
// tick replays; nothing is lost).
func TestProdWatch_LedgerRotationThroughAHostileIgnoreRuleRefuses(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string { return gittest.Run(t, h.ws, args...) }
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	// The asymmetric rule: it matches the rotation family only — exactly
	// the shape that slipped past the base-name-only preflight.
	_ = os.WriteFile(filepath.Join(h.ws, ".gitignore"), []byte("alertlog-*.jsonl\n"), 0o644)
	sd := filepath.Join(h.ws, ".prod-watch")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(sdPath(h, "alertlog.jsonl"), []byte(bigLines(3)), 0o644)
	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":6,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, []byte(`{"at":"x","fp":"probe:api"}`+"\n"), 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","generation":6}`), 0o644)
	out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": 5, "state_commit": true,
		"workspace": h.ws, "state_dir": ".prod-watch", "ledger": map[string]any{"max_bytes": 100, "keep": 2}},
		nil, nil), gittest.Env())
	if err == nil || !strings.Contains(stderr, "did not take") || !strings.Contains(stderr, "alertlog-5.jsonl") {
		t.Fatalf("a rotation an ignore rule refuses must fail the tick by name: %v %s", out, stderr)
	}
	if !mustExist(t, sdPath(h, "alertlog-5.jsonl")) {
		t.Fatalf("the refused rotation stays on disk — the tick replays, nothing is lost")
	}
	// The staged state landed locally before the push was refused (the
	// existing discipline: "committed locally and NOT pushed") — the next
	// tick replays from it and fails just as loudly until the rule is fixed.
	if b, _ := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "state.json")); !strings.Contains(string(b), `"generation":6`) {
		t.Fatalf("the staged state is the one on disk after the refusal: %s", b)
	}
}

// TestProdWatch_LedgerKeepReductionPrunesWithoutARotation: `keep` is the
// tree's steady state — lowering it shrinks the tree at the NEXT tick even
// if no ledger rotates for weeks.
func TestProdWatch_LedgerKeepReductionPrunesWithoutARotation(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 9, map[string]any{"max_bytes": 100000, "keep": 1}, "",
		`{"at":"x","generation":10}`,
		map[string]string{"alertlog-1.jsonl": "one\n", "alertlog-2.jsonl": "two\n", "alertlog-3.jsonl": "three\n"})
	rots, _ := out["rotations"].([]any)
	if len(rots) != 1 {
		t.Fatalf("the prune-only tick says what it removed: %v", rots)
	}
	r := rots[0].(map[string]any)
	if r["rotated_to"] != nil || fmt.Sprint(r["removed"]) != "[alertlog-1.jsonl alertlog-2.jsonl]" {
		t.Fatalf("no rotation, the two oldest removed: %v", r)
	}
	if mustExist(t, sdPath(h, "alertlog-1.jsonl")) || mustExist(t, sdPath(h, "alertlog-2.jsonl")) {
		t.Fatalf("the pruned rotations must leave the tree")
	}
	if !mustExist(t, sdPath(h, "alertlog-3.jsonl")) {
		t.Fatalf("the newest rotation stays")
	}
	if s, _ := out["summary"].(string); !strings.Contains(s, "pruned alertlog.jsonl") {
		t.Fatalf("a prune-only tick says pruned, not rotated: %v", out["summary"])
	}
}

// TestProdWatch_LedgerPruneKeepsTheNewestWithinAGeneration: a replay suffix
// (-2) is NEWER than the plain rotation of the same generation — sorting by
// the bare name ('-' before '.') would prune the replay and keep the stale.
func TestProdWatch_LedgerPruneKeepsTheNewestWithinAGeneration(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 9, map[string]any{"max_bytes": 100000, "keep": 2}, "",
		`{"at":"x","generation":10}`,
		map[string]string{"alertlog-7.jsonl": "plain\n", "alertlog-7-2.jsonl": "replay\n", "alertlog-8.jsonl": "next\n"})
	rots, _ := out["rotations"].([]any)
	if len(rots) != 1 || fmt.Sprint(rots[0].(map[string]any)["removed"]) != "[alertlog-7.jsonl]" {
		t.Fatalf("the OLDER of the generation-7 pair is pruned: %v", rots)
	}
	if !mustExist(t, sdPath(h, "alertlog-7-2.jsonl")) || !mustExist(t, sdPath(h, "alertlog-8.jsonl")) {
		t.Fatalf("the replay rotation and the next generation stay")
	}
}

// TestProdWatch_LedgerPlanValidatesTheKnobs: the bound is validated where
// every config knob is — plan refuses a malformed one by name, and a valid
// one travels normalized into plan's output (the wiring feeds commit_state).
func TestProdWatch_LedgerPlanValidatesTheKnobs(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	plan := func() (map[string]any, string, error) {
		return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, map[string]string{"grafana_token": h.tokenFile}))
	}
	for _, c := range []struct {
		name, field, want string
		ledger            map[string]any
	}{
		{"max_bytes that is a boolean", "ledger.max_bytes", "must be an integer", map[string]any{"max_bytes": true}},
		{"a negative max_bytes", "ledger.max_bytes", "must be an integer", map[string]any{"max_bytes": -1}},
		{"max_bytes as text", "ledger.max_bytes", "must be an integer", map[string]any{"max_bytes": "2MB"}},
		{"keep above its ceiling", "ledger.keep", "must be an integer", map[string]any{"keep": 1001}},
		{"keep that is a boolean", "ledger.keep", "must be an integer", map[string]any{"keep": true}},
	} {
		h.writeConfig(t, func(cfg map[string]any) { cfg["ledger"] = c.ledger })
		if _, stderr, err := plan(); err == nil || strings.Contains(stderr, "Traceback") || !strings.Contains(stderr, c.field) || !strings.Contains(stderr, c.want) {
			t.Fatalf("%s: plan refuses it by name (%q): %v %s", c.name, c.want, err, stderr)
		}
	}
	h.writeConfig(t, func(cfg map[string]any) { cfg["ledger"] = map[string]any{"max_bytes": 5000, "keep": 2} })
	out, stderr, err := plan()
	if err != nil {
		t.Fatalf("a valid ledger passes: %v %s", err, stderr)
	}
	led, _ := out["ledger"].(map[string]any)
	if led["max_bytes"] != float64(5000) || led["keep"] != float64(2) {
		t.Fatalf("the normalized ledger travels in plan's output: %v", led)
	}
	// With no opinion in the config, the DEFAULT bound travels — a
	// deployment without a ledger section still gets a bounded tree.
	h.writeConfig(t, func(cfg map[string]any) { delete(cfg, "ledger") })
	out, stderr, err = plan()
	if err != nil {
		t.Fatalf("the default ledger passes: %v %s", err, stderr)
	}
	led, _ = out["ledger"].(map[string]any)
	if led["max_bytes"] != float64(2000000) || led["keep"] != float64(8) {
		t.Fatalf("the defaults travel when the config is silent: %v", led)
	}
}
