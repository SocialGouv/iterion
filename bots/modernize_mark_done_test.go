package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// modernizeMarkDoneOut is the subset of mark_done's output the tests read.
type modernizeMarkDoneOut struct {
	Marked  bool   `json:"marked"`
	Commit  string `json:"commit"`
	Notice  string `json:"notice"`
	Refused bool   `json:"refused"`
}

func modernizeAutoMaintenanceEnv() (map[string]string, error) {
	slot := 0
	if raw, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || strings.TrimLeft(raw, "0123456789") != "" {
			return nil, fmt.Errorf("GIT_CONFIG_COUNT=%q is not a non-negative integer", raw)
		}
		if n > int(^uint(0)>>1)-2 {
			return nil, fmt.Errorf("GIT_CONFIG_COUNT=%q leaves no room for two appended entries", raw)
		}
		slot = n
	}
	maintenanceSlot := strconv.Itoa(slot)
	gcSlot := strconv.Itoa(slot + 1)
	return map[string]string{
		"GIT_CONFIG_COUNT":                    strconv.Itoa(slot + 2),
		"GIT_CONFIG_KEY_" + maintenanceSlot:   "maintenance.auto",
		"GIT_CONFIG_VALUE_" + maintenanceSlot: "false",
		"GIT_CONFIG_KEY_" + gcSlot:            "gc.auto",
		"GIT_CONFIG_VALUE_" + gcSlot:          "0",
	}, nil
}

// modernizeRepo builds a throwaway git repository carrying one committed
// contract and returns its path, the base commit, and a git runner bound to
// it. Shared by the mark_done and lot_verify tests.
func modernizeRepo(t *testing.T, planYAML string) (string, string, func(args ...string) string) {
	t.Helper()
	env, err := modernizeAutoMaintenanceEnv()
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "maintenance.auto", "false")
	git("config", "gc.auto", "0")
	if err := os.MkdirAll(filepath.Join(ws, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(planYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".modernize/plan.yaml")
	git("commit", "-qm", "contract")
	return ws, git("rev-parse", "HEAD"), git
}

func TestModernizeRepoPythonInheritsAutoMaintenanceOff(t *testing.T) {
	requireModernizeTools(t)
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "iterion.canary")
	t.Setenv("GIT_CONFIG_VALUE_0", "alive")
	t.Setenv("GIT_CONFIG_KEY_1", "maintenance.auto")
	t.Setenv("GIT_CONFIG_VALUE_1", "true")
	t.Setenv("GIT_CONFIG_KEY_2", "gc.auto")
	t.Setenv("GIT_CONFIG_VALUE_2", "1")
	ws, _, _ := modernizeRepo(t, "version: 1\nlots: []\n")
	trace := filepath.Join(t.TempDir(), "git.trace")
	script := `import os
import subprocess
import sys

ws, trace = sys.argv[1:3]
expected_env = {
    "GIT_CONFIG_COUNT": "5",
    "GIT_CONFIG_KEY_0": "iterion.canary",
    "GIT_CONFIG_VALUE_0": "alive",
    "GIT_CONFIG_KEY_1": "maintenance.auto",
    "GIT_CONFIG_VALUE_1": "true",
    "GIT_CONFIG_KEY_2": "gc.auto",
    "GIT_CONFIG_VALUE_2": "1",
    "GIT_CONFIG_KEY_3": "maintenance.auto",
    "GIT_CONFIG_VALUE_3": "false",
    "GIT_CONFIG_KEY_4": "gc.auto",
    "GIT_CONFIG_VALUE_4": "0",
}
for key, want in expected_env.items():
    if os.environ.get(key) != want:
        raise SystemExit(f"{key}: got {os.environ.get(key)!r}, want {want!r}")

def config(key, *scope):
    return subprocess.check_output(
        ["git", "-C", ws, "config", *scope, "--get", key], text=True
    ).strip()

for key, want in (("maintenance.auto", "false"), ("gc.auto", "0")):
    if config(key) != want:
        raise SystemExit(f"effective {key}: got {config(key)!r}, want {want!r}")
    if config(key, "--local") != want:
        raise SystemExit(f"local {key}: got {config(key, '--local')!r}, want {want!r}")
if config("iterion.canary") != "alive":
    raise SystemExit("the pre-existing config entry was overwritten")

with open(os.path.join(ws, "python.txt"), "w", encoding="utf-8") as f:
    f.write("python\n")
subprocess.run(["git", "-C", ws, "add", "python.txt"], check=True)
env = os.environ.copy()
env["GIT_TRACE"] = trace
subprocess.run(["git", "-C", ws, "commit", "-qm", "python"], check=True, env=env)
with open(trace, encoding="utf-8") as f:
    traced = f.read()
for command in ("maintenance run --auto", "gc --auto"):
    if command in traced:
        raise SystemExit(f"auto-maintenance escaped the fixture: {command}\n{traced}")
`
	cmd := exec.Command("python3", "-c", script, ws, trace)
	cmd.Env = gittest.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python did not inherit fixture git config: %v\n%s", err, out)
	}
}

func TestModernizeAutoMaintenanceEnvAcceptsEmptyCount(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "")
	env, err := modernizeAutoMaintenanceEnv()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "maintenance.auto",
		"GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1":   "gc.auto",
		"GIT_CONFIG_VALUE_1": "0",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestModernizeAutoMaintenanceEnvRefusesMalformedCount(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, raw := range []string{"-1", "one", strconv.Itoa(maxInt), strconv.Itoa(maxInt - 1)} {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			t.Setenv("GIT_CONFIG_COUNT", raw)
			if _, err := modernizeAutoMaintenanceEnv(); err == nil {
				t.Fatalf("GIT_CONFIG_COUNT=%q was accepted", raw)
			}
		})
	}
}

// modernizeMarkDone runs mark_done as if lot_verify had just judged the tree
// as it stands: the verdict it answers for is this HEAD and these files.
func modernizeMarkDone(t *testing.T, script, ws, lotID, base string, wantExit int) modernizeMarkDoneOut {
	t.Helper()
	head, tree := judgedNow(t, ws)
	return modernizeMarkDoneJudged(t, script, ws, lotID, base, head, tree, wantExit)
}

// contractPaths are the contract's files beside the default plan, in the
// order lot_verify's table lists them.
var contractPaths = []string{
	".modernize/plan.yaml",
	".modernize/outcomes.json",
	".modernize/brief.yaml",
	".modernize/ARBITRAGE.md",
	".modernize/defects-ledger.json",
}

// judgedNow is the verdict lot_verify would hand mark_done for this tree:
// HEAD, and per path of the contract's directory — the table's files, and
// every path the landing carries there: HEAD's tree, the index, the files
// `git add` would take — what a commit of the working tree would take: the
// working tree as git would store it (w), the index entries (i), the index
// flags (t), the `filter` attribute (f). In lot_verify's own format: the
// table's files by name, the rest as a count and the digest of its states,
// computed by the same canonical JSON as the producer's (python's json with
// sorted keys, compact separators, ASCII escapes) — names here are UTF-8.
func judgedNow(t *testing.T, ws string) (string, string) {
	t.Helper()
	state := map[string]map[string]string{}
	paths := map[string]bool{}
	for _, rel := range contractPaths {
		paths[rel] = true
	}
	for _, listing := range []string{
		gittest.Run(t, ws, "ls-tree", "-r", "-z", "--name-only", "HEAD", "--", ".modernize/"),
		gittest.Run(t, ws, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".modernize/"),
	} {
		for _, rel := range strings.Split(listing, "\x00") {
			if rel != "" {
				paths[rel] = true
			}
		}
	}
	idx, tags := map[string][]string{}, map[string][]string{}
	for _, e := range strings.Split(gittest.Run(t, ws, "ls-files", "-s", "-z", "--", ".modernize/"), "\x00") {
		meta, path, ok := strings.Cut(e, "\t")
		if ok {
			idx[path] = append(idx[path], strings.Join(strings.Fields(meta), ":"))
		}
	}
	for _, e := range strings.Split(gittest.Run(t, ws, "ls-files", "-v", "-z", "--", ".modernize/"), "\x00") {
		if len(e) > 2 {
			tags[e[2:]] = append(tags[e[2:]], e[:1])
		}
	}
	for rel := range paths {
		st := map[string]string{"w": "absent", "i": "absent", "t": "-", "f": "unspecified"}
		if e := idx[rel]; len(e) > 0 {
			sort.Strings(e)
			st["i"] = strings.Join(e, ",")
		}
		if tg := tags[rel]; len(tg) > 0 {
			sort.Strings(tg)
			st["t"] = strings.Join(tg, ",")
		}
		attr := strings.Split(gittest.Run(t, ws, "check-attr", "-z", "filter", "--", rel), "\x00")
		if len(attr) >= 3 {
			st["f"] = attr[2]
		}
		full := filepath.Join(ws, rel)
		fi, err := os.Lstat(full)
		switch {
		case err != nil:
		case fi.Mode()&os.ModeSymlink != 0:
			target, rerr := os.Readlink(full)
			if rerr != nil {
				t.Fatal(rerr)
			}
			st["w"] = "symlink:" + target
		case !fi.Mode().IsRegular():
			st["w"] = "other"
		default:
			st["w"] = "file:" + gittest.Run(t, ws, "hash-object", "-w", "--path="+rel, "--", full)
		}
		state[rel] = st
	}
	table, rest := map[string]map[string]string{}, map[string]map[string]string{}
	for rel, st := range state {
		if slices.Contains(contractPaths, rel) {
			table[rel] = st
		} else {
			rest[rel] = st
		}
	}
	raw, err := json.Marshal(rest)
	if err != nil {
		t.Fatal(err)
	}
	digest := exec.Command("python3", "-c", "import hashlib,json,sys\n"+
		"d=json.load(sys.stdin)\n"+
		"sys.stdout.write(hashlib.sha256(json.dumps(d,sort_keys=True,separators=(',',':')).encode()).hexdigest())")
	digest.Stdin = strings.NewReader(string(raw))
	sum, err := digest.Output()
	if err != nil {
		t.Fatalf("digest of the rest of the directory: %v", err)
	}
	tree, err := json.Marshal(map[string]any{"table": table, "rest": map[string]any{"count": len(rest), "digest": string(sum)}})
	if err != nil {
		t.Fatal(err)
	}
	return gittest.Run(t, ws, "rev-parse", "HEAD"), string(tree)
}

// modernizeMarkDoneJudged runs mark_done fed the verdict the lot_gate edge
// maps: the HEAD lot_verify judged and the fingerprints of the files it
// judged in the working tree.
func modernizeMarkDoneJudged(t *testing.T, script, ws, lotID, base, judgedHead, judgedTree string, wantExit int) modernizeMarkDoneOut {
	t.Helper()
	body := strings.ReplaceAll(script, "{{vars.workspace_dir}}", strconv.Quote(ws))
	body = strings.ReplaceAll(body, "{{input.plan_path}}", strconv.Quote(".modernize/plan.yaml"))
	body = strings.ReplaceAll(body, "{{input.lot_id}}", strconv.Quote(lotID))
	body = strings.ReplaceAll(body, "{{input.base_sha}}", strconv.Quote(base))
	body = strings.ReplaceAll(body, "{{input.judged_head}}", strconv.Quote(judgedHead))
	body = strings.ReplaceAll(body, "{{input.judged_tree}}", strconv.Quote(judgedTree))
	if i := strings.Index(body, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in mark_done near %q", body[i:min(i+40, len(body))])
	}
	scriptPath := filepath.Join(t.TempDir(), "mark_done.py")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	// stderr quoted in every failure message, never merged into the parsed
	// stream — the blind spot #2232 names, closed for the whole family.
	var sbuf strings.Builder
	cmd.Stderr = &sbuf
	out, err := cmd.Output()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("mark_done failed to execute: %v (out %q, stderr %q)", err, out, sbuf.String())
		}
		exit = ee.ExitCode()
	}
	if exit != wantExit {
		t.Fatalf("mark_done exited %d, want %d (out %q, stderr %q)", exit, wantExit, out, sbuf.String())
	}
	var res modernizeMarkDoneOut
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("mark_done output is not JSON: %v (out %q, stderr %q)", uerr, out, sbuf.String())
	}
	return res
}

// TestModernizeMarkDone pins the one write the gate makes to the contract:
// the converged lot's status, one line, one commit, nothing else. `done` is
// the gate's word — a worker that writes it is refused by lot_verify — so
// this node is the only place the programme's "accepted" ever gets written,
// and a landing has exactly one commit to check for it.
func TestModernizeMarkDone(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "mark_done")
	const plan = `version: 1
# the programme, as a human wrote it
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo   # a bookmark, never evidence
    rebaseline_allowed: false
    intent: |
      what may change, and what may not
    exit_gate:
      - "true"
  - id: L2
    title: "raise the runtime"
    status: todo
    depends_on: [L1]
    exit_gate:
      - "true"
`

	t.Run("converged lot: one line flipped, one commit, comments intact", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked || res.Commit == "" {
			t.Fatalf("marked=%v commit=%q, want the status written and committed (%s)", res.Marked, res.Commit, res.Notice)
		}
		got, err := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done   # a bookmark, never evidence", 1)
		if string(got) != want {
			t.Fatalf("contract after mark_done:\n%s\nwant exactly one status line changed:\n%s", got, want)
		}
		if subj := git("log", "-1", "--format=%s"); subj != "L1: done — gate, oracle and references green at "+base[:12] {
			t.Fatalf("commit subject = %q", subj)
		}
		if files := git("show", "--stat", "--format=", "HEAD"); !strings.Contains(files, "1 file changed") {
			t.Fatalf("the done commit must carry the contract alone, got:\n%s", files)
		}
		if git("rev-parse", "HEAD") != res.Commit {
			t.Fatalf("reported commit %s is not HEAD", res.Commit)
		}
	})

	t.Run("idempotent: a contract already done is left alone, nothing committed", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done", 1))
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if res.Marked || res.Commit != "" {
			t.Fatalf("marked=%v commit=%q on an already-done lot, want no write", res.Marked, res.Commit)
		}
		if head := git("rev-parse", "HEAD"); head != base {
			t.Fatalf("HEAD moved to %s on the idempotent path", head)
		}
	})

	t.Run("an interrupted attempt — done in the tree, not at HEAD — is committed, not skipped", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		written := strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done   # a bookmark, never evidence", 1)
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(written), 0o644); err != nil {
			t.Fatal(err)
		}
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked || res.Commit == "" {
			t.Fatalf("marked=%v commit=%q — idempotence judged in the working tree skipped the commit the gate owes (%s)", res.Marked, res.Commit, res.Notice)
		}
		if head := git("rev-parse", "HEAD"); head == base || head != res.Commit {
			t.Fatalf("HEAD=%s base=%s reported=%s, want the done commit at HEAD", head, base, res.Commit)
		}
		if dirty := git("status", "--porcelain"); dirty != "" {
			t.Fatalf("the tree must be clean after the commit, got %q", dirty)
		}
	})

	t.Run("an uncommitted worker edit to the contract does not ride along under the gate's subject", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		// The worker added a lot (a proposal the verdict accepts) and left it
		// uncommitted: the gate's commit must carry HEAD's contract + its one
		// flipped line, nothing else — the proposal stays the worker's.
		added := plan + "  - id: L3\n    title: proposed by the worker\n    status: todo\n    exit_gate:\n      - \"true\"\n"
		if err := os.WriteFile(filepath.Join(ws, ".modernize", "plan.yaml"), []byte(added), 0o644); err != nil {
			t.Fatal(err)
		}
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked || res.Commit == "" {
			t.Fatalf("marked=%v commit=%q (%s)", res.Marked, res.Commit, res.Notice)
		}
		committed := git("show", "HEAD:.modernize/plan.yaml")
		want := strings.TrimSpace(strings.Replace(plan, "status: todo   # a bookmark, never evidence", "status: done   # a bookmark, never evidence", 1))
		if committed != want {
			t.Fatalf("HEAD's contract must be the base + one flipped line, got:\n%s", committed)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
		if !strings.Contains(string(got), "L3") || !strings.Contains(string(got), "status: done") {
			t.Fatalf("the working tree must keep the worker's proposal and the flipped line:\n%s", got)
		}
		if dirty := git("status", "--porcelain", "--untracked-files=no"); dirty != "M .modernize/plan.yaml" {
			t.Fatalf("status = %q, want the contract modified in the tree (the proposal, unstaged)", dirty)
		}
	})

	t.Run("a rejecting pre-commit hook does not stop the gate's commit", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		hooks := filepath.Join(ws, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\necho rejected >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Marked || git("rev-parse", "HEAD") != res.Commit {
			t.Fatalf("the gate's bookkeeping commit must bypass the target's hooks: %+v", res)
		}
	})

	t.Run("undeclared lot is refused", func(t *testing.T) {
		ws, base, git := modernizeRepo(t, plan)
		res := modernizeMarkDone(t, script, ws, "L9", base, 0)
		if !res.Refused || !strings.Contains(res.Notice, "not in the contract") {
			t.Fatalf("notice = %q", res.Notice)
		}
		if head := git("rev-parse", "HEAD"); head != base {
			t.Fatalf("HEAD moved to %s on a refusal", head)
		}
	})

	t.Run("a block without a status line is refused, contract untouched", func(t *testing.T) {
		noStatus := strings.Replace(plan, "    status: todo   # a bookmark, never evidence\n", "", 1)
		ws, base, _ := modernizeRepo(t, noStatus)
		res := modernizeMarkDone(t, script, ws, "L1", base, 0)
		if !res.Refused || !strings.Contains(res.Notice, "no `status:` line") {
			t.Fatalf("notice = %q", res.Notice)
		}
		got, _ := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
		if string(got) != noStatus {
			t.Fatalf("a refused edit must leave the contract byte-identical")
		}
	})
}

// TestModernizeMarkDoneAnchorsOnIndentation pins the editor against the two
// look-alikes an intent can carry: a `status:` line and a `- id:` line written
// INSIDE an `intent: |` block scalar. Both sit deeper than the item's key
// column, so neither is the key the gate flips nor the boundary of the block.
func TestModernizeMarkDoneAnchorsOnIndentation(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "mark_done")
	const plan = `version: 1
lots:
  -   id: L1
      title: "wide dash"
      intent: |
        the previous line said
        status: done
        - id: L9
        but that is prose, not a key
      status: todo
      exit_gate:
        - "true"
  -   id: L2
      title: "next"
      status: todo
      exit_gate:
        - "true"
`
	ws, base, git := modernizeRepo(t, plan)
	res := modernizeMarkDone(t, script, ws, "L1", base, 0)
	if !res.Marked {
		t.Fatalf("not marked: %s", res.Notice)
	}
	got, _ := os.ReadFile(filepath.Join(ws, ".modernize", "plan.yaml"))
	want := strings.Replace(plan, "      status: todo\n      exit_gate:\n        - \"true\"\n  -   id: L2", "      status: done\n      exit_gate:\n        - \"true\"\n  -   id: L2", 1)
	if string(got) != want {
		t.Fatalf("the wrong line was edited:\n%s", got)
	}
	if subj := git("log", "-1", "--format=%s"); !strings.HasPrefix(subj, "L1: done") {
		t.Fatalf("commit subject = %q", subj)
	}
}
