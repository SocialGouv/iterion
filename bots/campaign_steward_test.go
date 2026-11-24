package bots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stewardOn runs the REAL steward on a committed tree with one pending
// rebaseline request, for lot L1 — `after`, when given, first shapes the
// working tree the way a child run could leave it — and returns the
// escalation items it wrote and the workspace. The run has no wall.
func stewardOn(t *testing.T, plan, verify string, after func(ws string, git func(...string) string)) ([]map[string]any, string, func(args ...string) string) {
	t.Helper()
	return stewardUnder(t, campaignClock{}, plan, verify, after)
}

// stewardUnder is stewardOn under a given clock.
func stewardUnder(t *testing.T, clock campaignClock, plan, verify string, after func(ws string, git func(...string) string)) ([]map[string]any, string, func(args ...string) string) {
	t.Helper()
	requireModernizeTools(t)
	script := toolScript(t, "campaign/main.bot", "steward")
	ws, _, git := modernizeRepo(t, plan)
	writeContract(t, ws, ".golden-master/refs/001.txt", "STATUS 200\n")
	writeContract(t, ws, ".golden-master/ledger.md", "# ledger\n")
	writeContract(t, ws, "verify.sh", verify)
	git("add", "-A")
	git("commit", "-qm", "the tree a child run left")
	if after != nil {
		after(ws, git)
	}
	for k, v := range map[string]string{
		"{{vars.workspace_dir}}": strconv.Quote(ws),
		"{{vars.governance}}":    strconv.Quote("bot"),
		"{{vars.escalation}}":    strconv.Quote("handoff"),
		"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
		"{{input.ledger_path}}":  strconv.Quote(".golden-master/ledger.md"),
		"{{input.verify_path}}":  strconv.Quote("sh verify.sh"),
		"{{input.before}}":       strconv.Quote(git("rev-parse", "HEAD")),
		"{{input.moved}}":        "true",
		"{{input.extensions}}":   "[]",
		"{{input.pending}}":      `[{"id": "R1", "lot": "L1", "expected_paths": [".golden-master/refs/001.txt"]}]`,
	} {
		script = strings.ReplaceAll(script, k, v)
	}
	script = clock.render(script)
	if i := strings.Index(script, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in steward near %q", script[i:min(i+40, len(script))])
	}
	p := filepath.Join(t.TempDir(), "steward.py")
	if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := modernizeChild(t, "python3", p)
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("steward: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	var items []map[string]any
	if raw, err := os.ReadFile(filepath.Join(ws, ".modernize/campaign/escalations.jsonl")); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var item map[string]any
			if err := json.Unmarshal([]byte(l), &item); err != nil {
				t.Fatalf("escalation line is not JSON: %q", l)
			}
			items = append(items, item)
		}
	}
	return items, ws, git
}

// campaignSteward is stewardOn on the committed tree as it stands, returning
// the items and the ledger.
func campaignSteward(t *testing.T, plan, verify string) ([]map[string]any, string) {
	t.Helper()
	items, ws, _ := stewardOn(t, plan, verify, nil)
	return items, readContract(t, ws, ".golden-master/ledger.md")
}

// TestCampaignStewardActsUnderTheLotsWall: a rebaseline act records the net and
// judges it again under the wall lot_verify gives the lot — its
// gate_timeout_s, else the top level's — never a fixed hour: a net slower than
// that is not an act that failed. A wall lot_verify would refuse is named, and
// nothing runs under one guessed.
func TestCampaignStewardActsUnderTheLotsWall(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nsleep 2\nif [ \"$1\" = --record ]; then echo 'STATUS 201' > .golden-master/refs/001.txt; fi\nexit 0\n"
	plan := func(top, lotWall string) string {
		return "version: 1\n" + top + "oracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n" +
			"    status: done\n    rebaseline_allowed: true\n" + lotWall + "    exit_gate:\n      - \"true\"\n"
	}
	t.Run("within the lot's wall, whatever the top level says, the act lands", func(t *testing.T) {
		items, ledger := campaignSteward(t, plan("gate_timeout_s: 1\n", "    gate_timeout_s: 10\n"), verify)
		if len(items) != 0 || !strings.Contains(ledger, "iterion:rebaseline-verdict") {
			t.Fatalf("items %v, ledger:\n%s", items, ledger)
		}
	})
	t.Run("past the lot's wall, the record failed and says it timed out", func(t *testing.T) {
		pastWall := "#!/bin/sh\nif [ \"$1\" = --record ]; then exec sleep infinity; fi\nexit 0\n"
		items, ledger := campaignSteward(t, plan("", "    gate_timeout_s: 1\n"), pastWall)
		if len(items) != 1 || items[0]["type"] != "record_failed" ||
			!strings.Contains(items[0]["detail"].(string), "timed out after 1s") {
			t.Fatalf("items %v", items)
		}
		if strings.Contains(ledger, "iterion:rebaseline-act") {
			t.Fatalf("an act was recorded past the wall:\n%s", ledger)
		}
	})
	t.Run("a re-judgement past the lot's wall unwinds the act", func(t *testing.T) {
		slowGate := "#!/bin/sh\nif [ \"$1\" = --record ]; then echo 'STATUS 201' > .golden-master/refs/001.txt; else sleep 4; fi\nexit 0\n"
		items, ledger := campaignSteward(t, plan("", "    gate_timeout_s: 2\n"), slowGate)
		if len(items) != 1 || items[0]["type"] != "gate_red_after_record" ||
			!strings.Contains(items[0]["detail"].(string), "timed out after 2s") {
			t.Fatalf("items %v", items)
		}
		if strings.Contains(ledger, "iterion:rebaseline-act") {
			t.Fatalf("the act was not unwound:\n%s", ledger)
		}
	})
	t.Run("output that is not UTF-8 is read, not raised on", func(t *testing.T) {
		latin1 := "#!/bin/sh\nprintf 'R\\351sultat\\n'\nif [ \"$1\" = --record ]; then echo 'STATUS 201' > .golden-master/refs/001.txt; fi\nexit 0\n"
		items, ledger := campaignSteward(t, plan("", ""), latin1)
		if len(items) != 0 || !strings.Contains(ledger, "iterion:rebaseline-verdict") {
			t.Fatalf("items %v, ledger:\n%s", items, ledger)
		}
	})
	t.Run("a plan that is not a mapping is an escalation, not a crash", func(t *testing.T) {
		items, _ := campaignSteward(t, "- id: L1\n", verify)
		if len(items) != 1 || items[0]["type"] != "plan_unreadable" ||
			!strings.Contains(items[0]["detail"].(string), "not a mapping") {
			t.Fatalf("items %v", items)
		}
	})
	t.Run("an unreadable wall is named, and nothing runs", func(t *testing.T) {
		items, _ := campaignSteward(t, plan("", "    gate_timeout_s: soon\n"), verify)
		if len(items) != 1 || items[0]["type"] != "refused_unreadable_wall" ||
			!strings.Contains(items[0]["detail"].(string), "lot L1") {
			t.Fatalf("items %v", items)
		}
	})
}

// TestCampaignStewardStopsWhatARecordStarted: a record past its wall is killed
// with everything it started, so nothing writes a reference after the steward
// has reported the record failed and handed the tree back.
func TestCampaignStewardStopsWhatARecordStarted(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nif [ \"$1\" = --record ]; then\n" +
		"  (sleep 3; echo 'STATUS 201' > .golden-master/refs/001.txt) &\n  wait\nfi\nexit 0\n"
	plan := "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n" +
		"    status: done\n    rebaseline_allowed: true\n    gate_timeout_s: 1\n    exit_gate:\n      - \"true\"\n"
	items, ws, git := stewardOn(t, plan, verify, nil)
	if len(items) != 1 || items[0]["type"] != "record_failed" {
		t.Fatalf("items %v", items)
	}
	time.Sleep(4 * time.Second)
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("a process the record started wrote after the steward returned: %q (ref now %q)",
			st, readContract(t, ws, ".golden-master/refs/001.txt"))
	}
}

// TestCampaignStewardJournalTakesOnlyItsOwnFiles: what a child run left in the
// index is not the supervisor's to land under "campaign: run N".
func TestCampaignStewardJournalTakesOnlyItsOwnFiles(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nexit 0\n"
	plan := "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n" +
		"    status: done\n    rebaseline_allowed: true\n    exit_gate:\n      - \"true\"\n"
	items, _, git := stewardOn(t, plan, verify, func(ws string, git func(...string) string) {
		writeContract(t, ws, "src/worker.txt", "work in flight\n")
		git("add", "src/worker.txt")
	})
	if len(items) != 1 || items[0]["type"] != "dirty_tree" {
		t.Fatalf("items %v", items)
	}
	if took := git("show", "--name-only", "--format=", "HEAD"); strings.Contains(took, "src/worker.txt") {
		t.Fatalf("the journal commit took the worker's staged file: %q", took)
	}
}

// TestCampaignStewardHoldsLotsToBeLists: a plan whose `lots` is not a list of
// lots is plan_unreadable — an escalation — where a rebaseline request was
// pending, not a crash that leaves no journal and no commit.
func TestCampaignStewardHoldsLotsToBeLists(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nexit 0\n"
	for name, plan := range map[string]string{
		"lots as a mapping": "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  L1:\n    status: done\n",
		"a scalar lots":     "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots: L1\n",
		"a lot not a map":   "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - L1\n",
	} {
		t.Run(name, func(t *testing.T) {
			items, _, git := stewardOn(t, plan, verify, nil)
			if len(items) != 1 || items[0]["type"] != "plan_unreadable" ||
				!strings.Contains(items[0]["detail"].(string), "lots") {
				t.Fatalf("items %v", items)
			}
			// The escalation is recorded and the steward goes on: the loop
			// survives a plan it cannot read.
			if j := git("log", "-1", "--format=%s"); !strings.Contains(j, "escalations=1") {
				t.Fatalf("the journal commit reads %q, want the escalation recorded", j)
			}
		})
	}
}

// TestCampaignStewardRestoresWhatItsCounterTestWrote: green or red, the
// counter-test's writes do not stay in the tree — the next request and the
// next child run see the committed tree.
func TestCampaignStewardRestoresWhatItsCounterTestWrote(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nif [ \"$1\" = --record ]; then echo 'STATUS 201' > .golden-master/refs/001.txt; else echo probe > probe.txt; exit 1; fi\nexit 0\n"
	plan := "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n" +
		"    status: done\n    rebaseline_allowed: true\n    exit_gate:\n      - \"true\"\n"
	items, ws, git := stewardOn(t, plan, verify, nil)
	if len(items) != 1 || items[0]["type"] != "gate_red_after_record" {
		t.Fatalf("items %v", items)
	}
	if st := git("status", "--porcelain"); st != "" {
		t.Fatalf("the counter-test's writes stayed: %q", st)
	}
	if _, err := os.Lstat(filepath.Join(ws, "probe.txt")); !os.IsNotExist(err) {
		t.Fatalf("probe.txt survived: %v", err)
	}
}

// TestCampaignStewardDefersAnActTheWallCannotHold: past 90% of the campaign's
// wall no tool node starts, finalize's included, so an act — the record, then the
// counter-test, each up to the lot's wall — starts only where it ends, at
// those walls, the steward's reserve (RESERVE_S) before that point. One that
// does not is deferred and nothing is recorded; the REAL observe of the next
// campaign then finds the request still pending (a logged request_id is a
// refusal it never acts on again). With one more second of room, the same
// request is acted on.
func TestCampaignStewardDefersAnActTheWallCannotHold(t *testing.T) {
	t.Parallel()
	verify := "#!/bin/sh\nif [ \"$1\" = --record ]; then echo 'STATUS 201' > .golden-master/refs/001.txt; fi\nexit 0\n"
	plan := "version: 1\noracle:\n  refs_dir: .golden-master/refs\nlots:\n  - id: L1\n    title: t\n" +
		"    status: done\n    rebaseline_allowed: true\n    gate_timeout_s: 600\n    exit_gate:\n      - \"true\"\n"
	request := "\n<!-- iterion:rebaseline-request\n" +
		`{"id": "R1", "lot": "L1", "expected_paths": [".golden-master/refs/001.txt"]}` + "\n-->\n"
	withRequest := func(ws string, git func(...string) string) {
		writeContract(t, ws, ".golden-master/ledger.md", "# ledger\n"+request)
		git("add", ".golden-master/ledger.md")
		git("commit", "-qm", "the request")
	}
	// The act takes up to 2 × 600 s. 90% of the wall less the reserve leaves
	// `left` seconds when the steward starts.
	const wall = 100000.0
	under := func(left float64) campaignClock {
		return campaignClock{wall: wall, elapsed: wall*0.9 - stewardReserve(t).Seconds() - left}
	}
	t.Run("an act that would end inside the reserve", func(t *testing.T) {
		items, ws, git := stewardUnder(t, under(1199), plan, verify, withRequest)
		if len(items) != 1 || items[0]["type"] != "deferred_by_wall" || items[0]["request"] != "R1" {
			t.Fatalf("items %v: an act of up to 1200 s with 1199 s left before the handoff's reserve must be deferred, by name", items)
		}
		if _, refused := items[0]["request_id"]; refused {
			t.Fatalf("the deferral logs a request_id (%v): observe reads that as a refusal, and the request is never acted on", items[0])
		}
		if note, _ := items[0]["note"].(string); !strings.Contains(note, "1200s") || !strings.Contains(note, "stays pending") {
			t.Fatalf("the deferral note %q does not name the act's time and that the request stays pending", note)
		}
		if ledger := readContract(t, ws, ".golden-master/ledger.md"); strings.Contains(ledger, "iterion:rebaseline-act") {
			t.Fatalf("an act was recorded past the room the wall left:\n%s", ledger)
		}
		if ref := readContract(t, ws, ".golden-master/refs/001.txt"); ref != "STATUS 200\n" {
			t.Fatalf("the reference was re-recorded under a deferral: %q", ref)
		}
		if pending := campaignObservePending(t, ws, git); len(pending) != 1 || pending[0] != "R1" {
			t.Fatalf("observe's pending requests after the deferral = %v, want [R1]: a deferred request is acted on by a later campaign", pending)
		}
	})
	t.Run("an act that ends before it", func(t *testing.T) {
		items, ws, _ := stewardUnder(t, under(1300), plan, verify, withRequest)
		if len(items) != 0 || !strings.Contains(readContract(t, ws, ".golden-master/ledger.md"), "iterion:rebaseline-verdict") {
			t.Fatalf("items %v: an act of up to 1200 s with 1300 s left must run", items)
		}
	})
}

// campaignObservePending runs the REAL observe on the workspace as the steward
// left it and returns the ids of the requests it hands the next steward.
func campaignObservePending(t *testing.T, ws string, git func(...string) string) []string {
	t.Helper()
	script := toolScript(t, "campaign/main.bot", "observe")
	for k, v := range map[string]string{
		"{{vars.workspace_dir}}": strconv.Quote(ws),
		"{{vars.plan_path}}":     strconv.Quote(".modernize/plan.yaml"),
		"{{input.base}}":         strconv.Quote(git("rev-parse", "HEAD")),
		"{{input.ledger_path}}":  strconv.Quote(".golden-master/ledger.md"),
	} {
		script = strings.ReplaceAll(script, k, v)
	}
	if i := strings.Index(script, "{{"); i >= 0 {
		t.Fatalf("unresolved template ref in observe near %q", script[i:min(i+40, len(script))])
	}
	p := filepath.Join(t.TempDir(), "observe.py")
	if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := modernizeChild(t, "python3", p)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("observe: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	var state struct {
		Pending []struct {
			ID string `json:"id"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatalf("observe output is not JSON: %v (out %q)", err, out)
	}
	ids := make([]string, 0, len(state.Pending))
	for _, r := range state.Pending {
		ids = append(ids, r.ID)
	}
	return ids
}
