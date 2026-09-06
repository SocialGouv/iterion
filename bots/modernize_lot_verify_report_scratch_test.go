package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The wrapper the net materialises, reduced to what these tests pin: it
// records where lot_verify told it to write (RVA marker), writes the report
// there, prints it, removes it. A redirect into a directory that is not
// there is the death being pinned.
const scratchWrapperHead = "#!/bin/sh\nset -e\n" +
	"[ -n \"$GM_REPORT_TMP\" ] || { echo 'GM_REPORT_TMP unset' >&2; exit 3; }\n" +
	"[ -z \"$REPORT_PATH_MARKER\" ] || printf '%s' \"$GM_REPORT_TMP\" > \"$REPORT_PATH_MARKER\"\n"

func scratchWorkspace(t *testing.T, wrapper string) (ws, base string) {
	t.Helper()
	const plan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: todo
    exit_gate:
      - "true"
`
	ws, _, git := modernizeRepo(t, plan)
	modernizeNet(t, ws)
	if err := os.WriteFile(filepath.Join(ws, ".golden-master", "verify-oracle.sh"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	git("add", "-A", ".golden-master")
	git("commit", "-qm", "net")
	return ws, git("rev-parse", "HEAD")
}

func scratchVerify(t *testing.T, script, ws, base string, extraEnv ...string) modernizeLotVerifyOut {
	t.Helper()
	env := append(os.Environ(), extraEnv...)
	res, exit := modernizeLotVerifyEnv(t, script, ws, "L1", base, "true", env)
	if exit != 0 {
		t.Fatalf("lot_verify exited %d, want a verdict (out %+v)", exit, res)
	}
	return res
}

func strayReports(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), "gm-last-report") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestModernizeLotVerifyOracleReportNeverDependsOnAMount pins where the gate
// wrapper's report goes, by reading the path back out of the wrapper.
// Measured on a pod backend: the runtime exported the run-files scratch
// variable for a bind the driver then dropped, lot_verify pointed
// GM_REPORT_TMP into it, the wrapper died on the redirect before the oracle
// ran, and four lots read "oracle RED" out of a missing directory.
func TestModernizeLotVerifyOracleReportNeverDependsOnAMount(t *testing.T) {
	requireModernizeTools(t)
	script := toolScript(t, "modernize/main.bot", "lot_verify")
	green := scratchWrapperHead +
		"echo '{\"mode\":\"gate\",\"ok\":true,\"invalid\":[]}' > \"$GM_REPORT_TMP\"\n" +
		"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\n"
	reportName := regexp.MustCompile(`^gm-last-report-[^/]+\.json$`)

	t.Run("the report is a temp file of the run, whatever scratch the environment announces", func(t *testing.T) {
		ws, base := scratchWorkspace(t, green)
		tmp := t.TempDir()
		marker := filepath.Join(t.TempDir(), "path")
		announced := filepath.Join(t.TempDir(), "announced-but-absent")
		res := scratchVerify(t, script, ws, base,
			"TMPDIR="+tmp, "ITERION_ARTIFACT_FILES_DIR="+announced, "REPORT_PATH_MARKER="+marker)
		if !res.OraclePassed || res.OracleNotRun {
			t.Fatalf("the oracle must run and pass: %+v", res)
		}
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("the wrapper never ran: %v", err)
		}
		path := string(got)
		if filepath.Dir(path) != tmp {
			t.Fatalf("report written to %s, want the run's temp dir %s", path, tmp)
		}
		if !reportName.MatchString(filepath.Base(path)) {
			t.Fatalf("report name %q is not a per-process temp name", filepath.Base(path))
		}
		if strings.HasPrefix(path, ws+string(os.PathSeparator)) {
			t.Fatalf("report inside the judged tree: %s", path)
		}
		if _, err := os.Stat(announced); err == nil {
			t.Fatalf("lot_verify created the announced scratch %s — the directory is the runtime's to provide, never the lot's to conjure", announced)
		}
		if res.OracleReport == nil || res.OracleReport["mode"] != "gate" {
			t.Fatalf("the report the wrapper printed is not in the verdict: %+v", res)
		}
	})

	t.Run("an inherited GM_REPORT_TMP is not a knob: the path is this call's", func(t *testing.T) {
		ws, base := scratchWorkspace(t, green)
		marker := filepath.Join(t.TempDir(), "path")
		inherited := filepath.Join(t.TempDir(), "absent", "r.json")
		res := scratchVerify(t, script, ws, base, "GM_REPORT_TMP="+inherited, "REPORT_PATH_MARKER="+marker)
		if !res.OraclePassed || res.OracleNotRun {
			t.Fatalf("an inherited path into an absent directory decided the verdict: %+v", res)
		}
		if got, _ := os.ReadFile(marker); string(got) == inherited {
			t.Fatalf("the wrapper was handed the inherited path %s", inherited)
		}
	})

	t.Run("a wrapper that dies before printing a report is an environment failure, not a RED", func(t *testing.T) {
		dead := scratchWrapperHead + "echo 'cannot create report: Directory nonexistent' >&2\nexit 2\n"
		ws, base := scratchWorkspace(t, dead)
		res := scratchVerify(t, script, ws, base)
		if res.OraclePassed {
			t.Fatalf("no report and a non-zero exit read as a pass: %+v", res)
		}
		if !res.OracleNotRun {
			t.Fatalf("a wrapper death read as an oracle verdict: %+v", res)
		}
		if !strings.HasPrefix(res.BlockReason, "ORACLE_NOT_RUN: ") {
			t.Fatalf("block_reason does not carry the code: %q", res.BlockReason)
		}
		if !strings.Contains(res.BlockReason, "Directory nonexistent") {
			t.Fatalf("block_reason — what the fail node reports — must carry the wrapper's own words, not stop before them: %q", res.BlockReason)
		}
		if !strings.Contains(res.LogTail, "never delivered one") || strings.Contains(res.LogTail, "went RED") {
			t.Fatalf("the log must say the oracle never delivered a verdict, not that it went RED: %s", res.LogTail)
		}
	})

	t.Run("a chatty wrapper's death keeps its LAST words in the typed message", func(t *testing.T) {
		chatty := scratchWrapperHead +
			"i=0; while [ $i -lt 120 ]; do echo \"replaying mutant $i of the visible set against the references\" >&2; i=$((i+1)); done\n" +
			"echo 'cannot create report: Directory nonexistent' >&2\nexit 2\n"
		ws, base := scratchWorkspace(t, chatty)
		res := scratchVerify(t, script, ws, base)
		if !res.OracleNotRun {
			t.Fatalf("a wrapper death read as a verdict: %+v", res)
		}
		if !strings.Contains(res.BlockReason, "Directory nonexistent") {
			t.Fatalf("the typed message kept the oldest output instead of the cause: %q", res.BlockReason)
		}
	})

	t.Run("a chatty stdout with no report is typed in seconds, not minutes", func(t *testing.T) {
		noisy := scratchWrapperHead +
			"i=0; while [ $i -lt 6000 ]; do echo \"{'line': $i, 'note': 'a python dict repr the harness logged, brace first, no JSON anywhere'}\"; i=$((i+1)); done\n" +
			"exit 2\n"
		ws, base := scratchWorkspace(t, noisy)
		started := time.Now()
		res := scratchVerify(t, script, ws, base)
		if d := time.Since(started); d > 8*time.Second {
			t.Fatalf("typing a no-report stdout of 6000 brace-first lines took %s — the report search is not bounded", d)
		}
		if !res.OracleNotRun || res.OraclePassed {
			t.Fatalf("a chatty wrapper death read as something else: %+v", res)
		}
	})

	t.Run("a report buried under chatter and followed by one noise line is still read", func(t *testing.T) {
		// The budget bounds the SEARCH; it must never discard a verdict that
		// was there. A bounded scan that gave up on the first brace-terminated
		// noise line after the report typed a green oracle ORACLE_NOT_RUN.
		buried := scratchWrapperHead +
			"i=0; while [ $i -lt 40 ]; do echo \"{'line': $i, 'note': 'a dict repr the harness logged before its report'}\"; i=$((i+1)); done\n" +
			"echo '{\"mode\":\"gate\",\"ok\":true,\"invalid\":[]}'\n" +
			"echo \"{'teardown': 'one more brace-first line after the report'}\"\n" +
			"exit 0\n"
		ws, base := scratchWorkspace(t, buried)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || !res.OraclePassed {
			t.Fatalf("a report followed by one noise line was discarded: %+v", res)
		}
		if res.OracleReport == nil || res.OracleReport["mode"] != "gate" {
			t.Fatalf("the buried report is not in the verdict: %+v", res)
		}
	})

	t.Run("a pretty-printed report buried under chatter and followed by noise is still read", func(t *testing.T) {
		// The block starts at the nearest column-0 "{" above its closing
		// line: forty brace-first lines above and two below must not push it
		// out of reach, whatever the search spends on the noise.
		buried := scratchWrapperHead +
			"i=0; while [ $i -lt 40 ]; do echo \"{'line': $i, 'note': 'a dict repr the harness logged before its report'}\"; i=$((i+1)); done\n" +
			"printf '{\\n  \"mode\": \"gate\",\\n  \"ok\": true,\\n  \"invalid\": []\\n}\\n'\n" +
			"echo \"{'teardown': 'one more brace-first line after the report'}\"\n" +
			"echo \"{'teardown': 'and another'}\"\n" +
			"exit 0\n"
		ws, base := scratchWorkspace(t, buried)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || !res.OraclePassed {
			t.Fatalf("a pretty-printed report followed by noise was discarded: %+v", res)
		}
		if res.OracleReport == nil || res.OracleReport["mode"] != "gate" {
			t.Fatalf("the buried pretty-printed report is not in the verdict: %+v", res)
		}
	})

	t.Run("an indent=0 report whose list objects sit at column 0 is read whole, not as a fragment", func(t *testing.T) {
		// json.dumps(indent=0) puts every object of a list at column 0; the
		// nearest opening line above the closing brace then starts an inner
		// object, and a fragment without a mode was read as a green gate.
		zero := scratchWrapperHead +
			"printf '{\\n\"mode\": \"gate\",\\n\"invalid\": [\\n{\\n\"name\": \"m1\"\\n},\\n{\\n\"name\": \"m2\"\\n}\\n],\\n\"stable\": true\\n}\\n'\n" +
			"exit 0\n"
		ws, base := scratchWorkspace(t, zero)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || !res.OraclePassed {
			t.Fatalf("an indent=0 report was not read as a gate verdict: %+v", res)
		}
		if len(res.OracleInvalid) != 2 {
			t.Fatalf("the report's invalid list was lost — a fragment was read instead of the report: %+v", res)
		}
	})

	t.Run("an indented one-line report is read wherever it sits", func(t *testing.T) {
		indented := scratchWrapperHead +
			"echo '   {\"mode\":\"gate\",\"ok\":true,\"invalid\":[]}'\n" +
			"exit 0\n"
		ws, base := scratchWorkspace(t, indented)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || !res.OraclePassed || res.OracleReport["mode"] != "gate" {
			t.Fatalf("an indented one-line report was lost: %+v", res)
		}
	})

	t.Run("a JSON object carrying no verdict field is not a report — the report above it is read", func(t *testing.T) {
		// A wrapper that prints `{}` (or a fragment) after its report: the
		// object is skipped, the report is the verdict. Read as a mode-less
		// gate report, `{}` was a green verdict with an empty `invalid` list.
		bare := scratchWrapperHead +
			"echo '{\"mode\":\"gate\",\"ok\":false,\"invalid\":[\"m1\"]}'\n" +
			"echo '{}'\n" +
			"echo '{\"name\": \"m2\"}'\n" +
			"exit 1\n"
		ws, base := scratchWorkspace(t, bare)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || res.OraclePassed {
			t.Fatalf("a red report followed by non-report objects was not read as the RED it is: %+v", res)
		}
		if len(res.OracleInvalid) != 1 {
			t.Fatalf("the report's invalid list was lost — a later object was read instead: %+v", res)
		}
	})

	t.Run("a wrapper that exits 0 without printing a report is not a pass", func(t *testing.T) {
		silent := scratchWrapperHead + "exit 0\n"
		ws, base := scratchWorkspace(t, silent)
		res := scratchVerify(t, script, ws, base)
		if res.OraclePassed || !res.OracleNotRun {
			t.Fatalf("a wrapper that said nothing was read as a pass: %+v", res)
		}
		if !strings.Contains(res.LogTail, "exited 0 without printing a report") {
			t.Fatalf("the cause is not named: %s", res.LogTail)
		}
	})

	t.Run("a pretty-printed report reads the same as a one-line one", func(t *testing.T) {
		pretty := scratchWrapperHead +
			"printf '{\\n  \"mode\": \"gate\",\\n  \"ok\": false,\\n  \"invalid\": [\\n    {\"id\": \"m03\", \"reason\": \"anchor gone\"}\\n  ]\\n}\\n' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 1\n"
		ws, base := scratchWorkspace(t, pretty)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || res.OraclePassed {
			t.Fatalf("an indented report was not read as the oracle's RED: %+v", res)
		}
		if len(res.OracleInvalid) != 1 || res.OracleReport == nil || res.OracleReport["mode"] != "gate" {
			t.Fatalf("the indented report's fields were not read: %+v", res)
		}
	})

	t.Run("an oversized report is bounded in the verdict, its fields still read", func(t *testing.T) {
		huge := scratchWrapperHead +
			"python3 -c 'import json; print(json.dumps({\"mode\": \"gate\", \"ok\": False, \"invalid\": [{\"id\": \"m09\", \"reason\": \"anchor gone\"}], \"blind_lanes\": [\"lane-%d\" % i for i in range(20000)]}))' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 1\n"
		ws, base := scratchWorkspace(t, huge)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || res.OraclePassed {
			t.Fatalf("an oversized report was not read as the oracle's RED: %+v", res)
		}
		if len(res.OracleInvalid) != 1 {
			t.Fatalf("oracle_invalid must still come from the whole report: %+v", res.OracleInvalid)
		}
		if res.OracleReport == nil || res.OracleReport["truncated"] != true || res.OracleReport["mode"] != "gate" {
			t.Fatalf("an oversized report must be stored as a bounded stub naming its mode: %+v", res.OracleReport)
		}
	})

	t.Run("an oversized invalid list is bounded in the verdict, and the cut is written", func(t *testing.T) {
		many := scratchWrapperHead +
			"python3 -c 'import json; print(json.dumps({\"mode\": \"gate\", \"ok\": False, \"invalid\": [{\"id\": \"m%03d\" % i, \"reason\": \"anchor gone\"} for i in range(300)]}))' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 1\n"
		ws, base := scratchWorkspace(t, many)
		res := scratchVerify(t, script, ws, base)
		if len(res.OracleInvalid) != 200 {
			t.Fatalf("oracle_invalid must be bounded at 200 entries, got %d", len(res.OracleInvalid))
		}
		if !strings.Contains(res.LogTail, "carries the first 200") {
			t.Fatalf("the cut must be written in the log: %s", res.LogTail)
		}
	})

	t.Run("a report without a mode is a gate report: its RED stays a RED", func(t *testing.T) {
		modeless := scratchWrapperHead +
			"echo '{\"ok\":false,\"invalid\":[]}' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 1\n"
		ws, base := scratchWorkspace(t, modeless)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || res.OraclePassed {
			t.Fatalf("an older harness's report, without a mode, was not read as a gate verdict: %+v", res)
		}
		if !strings.Contains(res.LogTail, "went RED") {
			t.Fatalf("the log must carry the RED: %s", res.LogTail)
		}
	})

	t.Run("a report and a non-zero exit is the oracle's RED, with its fields", func(t *testing.T) {
		red := scratchWrapperHead +
			"echo '{\"mode\":\"gate\",\"ok\":false,\"invalid\":[{\"id\":\"m01\",\"reason\":\"anchor gone\"}]}' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\necho 'GATE RED' >&2\nexit 1\n"
		ws, base := scratchWorkspace(t, red)
		res := scratchVerify(t, script, ws, base)
		if res.OraclePassed || res.OracleNotRun {
			t.Fatalf("a printed report with exit 1 is the oracle's own RED: %+v", res)
		}
		if !strings.Contains(res.LogTail, "went RED") {
			t.Fatalf("the log must carry the RED: %s", res.LogTail)
		}
		if len(res.OracleInvalid) != 1 || res.OracleReport == nil || res.OracleReport["ok"] != false {
			t.Fatalf("the report's fields were not read: %+v", res)
		}
	})

	t.Run("the report survives a harness that logs more than the tail keeps", func(t *testing.T) {
		noisy := scratchWrapperHead +
			"i=0; while [ $i -lt 300 ]; do echo \"progress line $i of the harness, replaying one more mutant against the references\" >&2; i=$((i+1)); done\n" +
			"echo '{\"mode\":\"gate\",\"ok\":false,\"invalid\":[{\"id\":\"m07\",\"reason\":\"anchor gone\"}]}' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 1\n"
		ws, base := scratchWorkspace(t, noisy)
		res := scratchVerify(t, script, ws, base)
		if res.OracleNotRun || res.OraclePassed {
			t.Fatalf("a RED with a report read as something else: %+v", res)
		}
		if len(res.OracleInvalid) != 1 || res.OracleReport == nil {
			t.Fatalf("the report was lost to the log tail: %+v", res)
		}
	})

	t.Run("a temp dir inside the workspace is refused, and leaves nothing behind", func(t *testing.T) {
		ws, base := scratchWorkspace(t, green)
		res := scratchVerify(t, script, ws, base, "TMPDIR="+filepath.Join(ws, ".golden-master"))
		if res.OraclePassed || !res.OracleNotRun {
			t.Fatalf("a report inside the judged tree was accepted: %+v", res)
		}
		if !strings.Contains(res.LogTail, "inside the workspace") {
			t.Fatalf("the cause is not named: %s", res.LogTail)
		}
		if stray := strayReports(t, ws); len(stray) != 0 {
			t.Fatalf("report files left in the workspace: %v", stray)
		}
	})

	t.Run("a temp dir that merely shares the workspace's prefix is accepted", func(t *testing.T) {
		ws, base := scratchWorkspace(t, green)
		sibling := ws + "-scratch"
		if err := os.MkdirAll(sibling, 0o755); err != nil {
			t.Fatal(err)
		}
		res := scratchVerify(t, script, ws, base, "TMPDIR="+sibling)
		if !res.OraclePassed || res.OracleNotRun {
			t.Fatalf("a sibling scratch dir was refused as inside the workspace: %+v", res)
		}
	})

	t.Run("a report that says it is not a gate is no verdict, whatever the exit code", func(t *testing.T) {
		subset := scratchWrapperHead +
			"echo '{\"mode\":\"gate-subset\",\"ok\":true,\"invalid\":[]}' > \"$GM_REPORT_TMP\"\n" +
			"cat \"$GM_REPORT_TMP\"\nrm -f \"$GM_REPORT_TMP\"\nexit 0\n"
		ws, base := scratchWorkspace(t, subset)
		res := scratchVerify(t, script, ws, base)
		if res.OraclePassed || !res.OracleNotRun {
			t.Fatalf("a subset run exiting 0 was read as a pass: %+v", res)
		}
		if !strings.Contains(res.LogTail, "mode=gate-subset") {
			t.Fatalf("the mode is not named: %s", res.LogTail)
		}
	})

	t.Run("a lot the campaign declared blocked keeps its own stop when the oracle cannot run", func(t *testing.T) {
		const blockedPlan = `version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "raise the build tool"
    status: blocked
    exit_gate:
      - "true"
`
		ws, _, git := modernizeRepo(t, blockedPlan)
		modernizeNet(t, ws)
		dead := scratchWrapperHead + "echo 'cannot create report: Directory nonexistent' >&2\nexit 2\n"
		if err := os.WriteFile(filepath.Join(ws, ".golden-master", "verify-oracle.sh"), []byte(dead), 0o755); err != nil {
			t.Fatal(err)
		}
		git("add", "-A", ".golden-master")
		git("commit", "-qm", "net")
		base := git("rev-parse", "HEAD")
		res := scratchVerify(t, script, ws, base)
		if !res.LotBlocked || res.OracleNotRun || res.OraclePassed {
			t.Fatalf("the campaign's block must stay the stop: %+v", res)
		}
		if !strings.HasPrefix(res.BlockReason, "raise the build tool") {
			t.Fatalf("the campaign's reason was typed over: %q", res.BlockReason)
		}
		if !strings.Contains(res.LogTail, "ORACLE_NOT_RUN") {
			t.Fatalf("the environment failure must still be logged: %s", res.LogTail)
		}
	})
}
