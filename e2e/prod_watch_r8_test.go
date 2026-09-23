package e2e

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// R8-A: a first walk that fails after its band was CUT (more than BAND_CAP
// lines seen) leaves a bound ABOVE where it opened; the bootstrap retry
// reopens at min(boot_from, bound) = boot_from, below the bound: the cut
// lines are written a second time.
func TestProdWatch_BootstrapRetryAfterCutRewrites(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	base := nsAgo(9 * time.Minute)
	var lines []pwLine
	for i := 0; i < 6000; i++ {
		lines = append(lines, pwLine{TS: base + int64(i)*int64(80*time.Millisecond), Line: fmt.Sprintf("ERROR boot %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	// errors-q is walked first: pages 1..5 answer (5000 lines), page 6 and its retry fail.
	h.failLokiFrom.Store(int64(len(h.calls()) + 6))
	h.failLokiCount.Store(2)
	written := map[string]int{}
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 6000))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	t.Logf("tick 1: err=%q lines=%v covered=%v frontier=%v from=%v bound=%v band=%d", pq["error"], pq["lines"], pq["covered_to_ns"], pq["frontier_ns"], pq["from_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)))
	from1, _ := strconv.ParseInt(pq["from_ns"].(string), 10, 64)
	bound1, _ := strconv.ParseInt(pq["overlap_from_ns"].(string), 10, 64)
	h.failLokiFrom.Store(0)
	time.Sleep(2 * time.Second)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 6000))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	from2, _ := strconv.ParseInt(pq["from_ns"].(string), 10, 64)
	t.Logf("tick 2: err=%q lines=%v from=%v (from1+%.1fs, bound-from2=%.1fs) bootstrap=%v", pq["error"], pq["lines"], pq["from_ns"], float64(from2-from1)/1e9, float64(bound1-from2)/1e9, pq["bootstrap"])
	dups := 0
	first := ""
	for l, n := range written {
		if n > 1 {
			dups++
			if first == "" {
				first = fmt.Sprintf("%q x%d", l, n)
			}
		}
	}
	if dups > 0 {
		t.Fatalf("%d lines written twice (e.g. %s); distinct=%d", dups, first, len(written))
	}
}

// R8-A2: the same double count with the DEFAULT page size and max_lines
// (1000 / 5000): two failed bootstrap walks in a row — the retry re-reads
// the known band uncharged, sees > 4000 lines, fails again, cuts.
func TestProdWatch_BootstrapRetryAfterCutRewritesDefaults(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	base := nsAgo(9 * time.Minute)
	var lines []pwLine
	for i := 0; i < 7000; i++ {
		lines = append(lines, pwLine{TS: base + int64(i)*int64(70*time.Millisecond), Line: fmt.Sprintf("ERROR boot %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	written := map[string]int{}
	// tick 1: pages 1..3 answer, page 4 (and its retry) fail.
	h.failLokiFrom.Store(int64(len(h.calls()) + 4))
	h.failLokiCount.Store(2)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	t.Logf("tick 1: err=%q lines=%v from=%v bound=%v band=%d", pq["error"], pq["lines"], pq["from_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)))
	// tick 2: pages 1..3 re-read the known band (uncharged), 4..5 are new, page 6 fails.
	h.failLokiFrom.Store(int64(len(h.calls()) + 6))
	h.failLokiCount.Store(2)
	time.Sleep(1 * time.Second)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	t.Logf("tick 2: err=%q lines=%v from=%v bound=%v band=%d", pq["error"], pq["lines"], pq["from_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)))
	h.failLokiFrom.Store(0)
	time.Sleep(1 * time.Second)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	t.Logf("tick 3: err=%q lines=%v from=%v bound=%v band=%d trunc=%v", pq["error"], pq["lines"], pq["from_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)), pq["truncated"])
	dups := 0
	first := ""
	for l, n := range written {
		if n > 1 {
			dups++
			if first == "" {
				first = fmt.Sprintf("%q x%d", l, n)
			}
		}
	}
	if dups > 0 {
		t.Fatalf("%d lines written twice with the default page/max_lines (e.g. %s); distinct=%d", dups, first, len(written))
	}
}

// R8-B: the documented config (errors + a broad leak_sweep). leak_sweep
// reads more than max_lines every tick (a busy namespace): loki_complete is
// false on EVERY tick, so no template incident is ever quieted or forgotten
// — although leak_sweep cannot produce a template at all.
func TestProdWatch_TruncatedLeakSweepFreezesTemplateLifecycle(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, sweepPerTick := range []int{3, 40} { // 3: leak_sweep complete (control); 40: truncated at max_lines 20
		sweepPerTick := sweepPerTick
		t.Run(fmt.Sprintf("sweep%d", sweepPerTick), func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) {
				lokiOnly(1000, 60)(cfg)
				cfg["loki"].(map[string]any)["queries"] = map[string]any{"errors": "errors-q", "leak_sweep": "sweep-q"}
			})
			// An established install: incidents from earlier ticks.
			st := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
				"incidents": map[string]any{"loki:quiet": incident("loki", "medium", true, 100, 100), "loki:old": incident("loki", "medium", true, 400, 400)}}
			sb, _ := json.Marshal(st)
			_ = os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755)
			_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), sb, 0o644)
			var lines []pwLine
			quiet, forgotten := 0, false
			for k := 0; k < 6; k++ {
				for i := 0; i < sweepPerTick; i++ {
					lines = append(lines, pwLine{TS: nsAgo(time.Duration(sweepPerTick-i) * 10 * time.Millisecond), Line: fmt.Sprintf("INFO GET /x %d-%d", k, i), Container: "api", Q: "sweep-q"})
				}
				h.lines.Store(append([]pwLine(nil), lines...))
				outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 20))
				for _, a := range outs["decide"]["alerts"].([]any) {
					if a.(map[string]any)["state"] == "quiet" {
						quiet++
					}
				}
				pq := outs["poll_loki"]["per_query"].(map[string]any)
				inc := h.state(t)["incidents"].(map[string]any)
				_, kept := inc["loki:old"]
				if !kept {
					forgotten = true
				}
				t.Logf("tick %d: errors trunc=%v | leak_sweep trunc=%v gap=%v | quiet notes so far=%d, loki:old kept=%v", k,
					pq["errors"].(map[string]any)["truncated"], pq["leak_sweep"].(map[string]any)["truncated"], pq["leak_sweep"].(map[string]any)["gap"], quiet, kept)
				time.Sleep(200 * time.Millisecond)
			}
			if quiet == 0 || !forgotten {
				t.Fatalf("sweep %d/tick: the errors query observed everything on every tick, yet no quiet note (%d) and loki:old never forgotten (%v)", sweepPerTick, quiet, forgotten)
			}
		})
	}
}

func r8Word(i int) string {
	s := ""
	for k := 0; k < 4; k++ {
		s += string(rune('a' + i%26))
		i /= 26
	}
	return s
}

// R8-C: signals keep the top 200 templates by TOTAL count — history
// included. A new query's first window with 200 history templates pushes a
// live template out of the list: the live alert is never posted, and an
// alerted incident observed this tick gets a "not observed any more" note.
func TestProdWatch_HistoryCrowdsLiveTemplatesOutOfTheList(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	lines := []pwLine{{TS: nsAgo(2 * time.Minute), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // bootstrap tick: cursors established
	// An alerted incident from long ago whose template recurs this tick.
	tpl := "ERROR payment gateway refused the card"
	sum := sha1.Sum([]byte(tpl))
	fp := "loki:" + hex.EncodeToString(sum[:])[:12]
	st := h.state(t)
	st["incidents"] = map[string]any{fp: incident("loki", "medium", true, 100, 100)}
	sb, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), sb, 0o644)
	h.writeConfig(t, func(cfg map[string]any) {
		lokiTwoQueries(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["queries"].(map[string]any)["zzz_new"] = "new-q"
	})
	for i := 0; i < 200; i++ { // the new query's first window: 200 history templates, 2 lines each
		for r := 0; r < 2; r++ {
			lines = append(lines, pwLine{TS: nsAgo(4*time.Minute) + int64(i*2+r)*int64(time.Millisecond), Line: "ERROR legacy " + r8Word(i) + " failed", Container: "api", Q: "new-q"})
		}
	}
	lines = append(lines, pwLine{TS: nsAgo(5 * time.Second), Line: tpl, Container: "api", Q: "errors-q"})                      // live, recurring
	lines = append(lines, pwLine{TS: nsAgo(4 * time.Second), Line: "ERROR brand new outage", Container: "api", Q: "errors-q"}) // live, new
	h.lines.Store(lines)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	t.Logf("leak_scan: %v", outs["leak_scan"]["summary"])
	t.Logf("decide: %v", outs["decide"]["summary"])
	for _, a := range outs["decide"]["alerts"].([]any) {
		m := a.(map[string]any)
		t.Logf("alert: %v %v %v", m["state"], m["kind"], m["title_arg"])
	}
	got := strings.Join(alertsOf(t, outs), ",")
	if !strings.Contains(got, "loki:new:medium") || strings.Contains(got, "quiet") {
		t.Fatalf("the live new template must post and nothing observed this tick may be called quiet, got %v", got)
	}
}

// R8-D: a query whose walk failed after writing lines, then dropped from the
// config and re-added: the dropped cursor keeps its mark but not the lines
// the failed walk wrote (above the mark / in the first window).
func TestProdWatch_FailedWalkThenDropAndReaddRewrites(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, established := range []bool{false, true} {
		established := established
		t.Run(fmt.Sprintf("established=%v", established), func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, lokiTwoQueries(2, 60))
			written := map[string]int{}
			var lines []pwLine
			if established {
				h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // cursor established at `now`
				time.Sleep(1 * time.Second)
			}
			for i := 0; i < 6; i++ {
				lines = append(lines, pwLine{TS: nsAgo(900*time.Millisecond) + int64(i)*int64(100*time.Millisecond), Line: fmt.Sprintf("ERROR line %d", i), Container: "api", Q: "errors-q"})
			}
			h.lines.Store(lines)
			time.Sleep(200 * time.Millisecond)
			h.failLokiFrom.Store(int64(len(h.calls()) + 2)) // second page of errors-q and its retry
			h.failLokiCount.Store(2)
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
			pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
			t.Logf("tick A (fails): err=%q lines=%v covered=%v frontier=%v bound=%v", pq["error"], pq["lines"], pq["covered_to_ns"], pq["frontier_ns"], pq["overlap_from_ns"])
			h.failLokiFrom.Store(0)
			h.writeConfig(t, func(cfg map[string]any) { // errors dropped
				lokiTwoQueries(2, 60)(cfg)
				delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
			})
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			t.Logf("dropped cursor: %v", h.state(t)["cursors"].(map[string]any)["loki"].(map[string]any)["errors"])
			h.writeConfig(t, lokiTwoQueries(2, 60)) // errors re-added
			outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
			pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
			t.Logf("tick C (re-added): lines=%v from=%v bootstrap=%v", pq["lines"], pq["from_ns"], pq["bootstrap"])
			for l, n := range written {
				if n > 1 {
					t.Errorf("%q written %d times", l, n)
				}
			}
		})
	}
}

// R8-E: the live alert of a template fed by history lines too renders
// count = live lines only, but `first`, the sample, the streams and the
// evidence query come from the history lines.
func TestProdWatch_LiveAlertCarriesHistoryFields(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	lines := []pwLine{{TS: nsAgo(2 * time.Minute), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	h.writeConfig(t, func(cfg map[string]any) {
		lokiTwoQueries(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["queries"].(map[string]any)["aaa_new"] = "new-q"
	})
	for i := 0; i < 60; i++ {
		lines = append(lines, pwLine{TS: nsAgo(9*time.Minute) + int64(i)*int64(time.Second), Line: fmt.Sprintf("ERROR db timeout %d ms", i), Container: "legacy-batch", Q: "new-q"})
	}
	lines = append(lines, pwLine{TS: nsAgo(5 * time.Second), Line: "ERROR db timeout 7 ms", Container: "api", Q: "errors-q"})
	h.lines.Store(lines)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	alerts := outs["decide"]["alerts"].([]any)
	if len(alerts) != 1 {
		t.Fatalf("one alert, for the live line: %v", alerts)
	}
	m := alerts[0].(map[string]any)
	fields, ev := m["fields"].(map[string]any), m["evidence"].(map[string]any)
	if fields["count"].(float64) != 1 || strings.Contains(fmt.Sprint(fields["streams"]), "legacy-batch") || ev["query"] != "errors" || !strings.Contains(fmt.Sprint(ev["sample"]), "7 ms") {
		t.Fatalf("a live alert renders its live lines only — count, streams, query, sample: %v %v", fields, ev)
	}
	first := fmt.Sprint(fields["first"])
	var parsed time.Time
	var perr error
	for _, layout := range []string{"2006-01-02T15:04:05-07:00", "2006-01-02T15:04-07:00", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04Z07:00", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if parsed, perr = time.Parse(layout, first); perr == nil {
			break
		}
	}
	if perr != nil || time.Since(parsed) > 2*time.Minute {
		t.Fatalf("first-seen must be the live line's time, not the history's: %q (%v)", first, perr)
	}
}

// R8-F: two truncated walks in a row while the overlap narrows (300 s ->
// 5 s): the second one stops at a frontier far below `covered - overlap`.
// The band's lower edge must follow the frontier (hand_over's
// min(frontier, covered - overlap)), or the next window reopens above the
// unread late lines.
func TestProdWatch_TruncatedTwiceWhileOverlapNarrows(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 300))
	lines := []pwLine{{TS: nsAgo(250 * time.Second), Line: "ERROR anchor", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	written := map[string]int{}
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	for i := 1; i <= 6; i++ { // late lines below the mark, inside the 300 s overlap
		lines = append(lines, pwLine{TS: nsAgo(240*time.Second) + int64(i)*int64(10*time.Second), Line: fmt.Sprintf("ERROR late %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 2)) // truncated at late 2
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	h.writeConfig(t, lokiOnly(1000, 5))
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 2)) // truncated again at late 4, overlap now 5 s
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	t.Logf("tick 3: trunc=%v frontier=%v covered=%v bound=%v", pq["truncated"], pq["frontier_ns"], pq["covered_to_ns"], pq["overlap_from_ns"])
	for k := 0; k < 2; k++ {
		outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	}
	if len(written) != 7 {
		t.Fatalf("every late line inside a window must be written once: %d of 7 (%v)", len(written), written)
	}
	for l, n := range written {
		if n != 1 {
			t.Fatalf("%q written %d times", l, n)
		}
	}
}

// R8-G: a deterministic witness for round 7's failed-walk frontier: a walk
// that fails among late lines below the mark, then the overlap narrows —
// the retry must reopen where the walk stopped.
func TestProdWatch_FailedWalkThenOverlapNarrowsReadsTheRest(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(2, 300))
	lines := []pwLine{{TS: nsAgo(250 * time.Second), Line: "ERROR anchor", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	written := map[string]int{}
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	for i := 1; i <= 6; i++ {
		lines = append(lines, pwLine{TS: nsAgo(240*time.Second) + int64(i)*int64(10*time.Second), Line: fmt.Sprintf("ERROR late %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	h.failLokiFrom.Store(int64(len(h.calls()) + 2)) // errors-q page 2 and its retry
	h.failLokiCount.Store(2)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	h.failLokiFrom.Store(0)
	h.writeConfig(t, lokiTwoQueries(2, 5))
	for k := 0; k < 2; k++ {
		outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	}
	if len(written) != 7 {
		t.Fatalf("the late lines after the failure point must be read on the retry: %d of 7 (%v)", len(written), written)
	}
	for l, n := range written {
		if n != 1 {
			t.Fatalf("%q written %d times", l, n)
		}
	}
}

// R8-H: the SAME line read by a query still in its first window and by an
// established query (two LogQL selectors over one namespace overlap): the
// scan keeps the first record by (ts, line) — the first QUERY in config
// order — so the order of the queries decides whether a live line is news.
func TestProdWatch_SharedLineGateFollowsQueryOrder(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, newq := range []string{"aaa_new", "zzz_new"} {
		newq := newq
		t.Run(newq, func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, lokiTwoQueries(1000, 60))
			lines := []pwLine{{TS: nsAgo(2 * time.Minute), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
			h.lines.Store(lines)
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			h.writeConfig(t, func(cfg map[string]any) {
				lokiTwoQueries(1000, 60)(cfg)
				cfg["loki"].(map[string]any)["queries"].(map[string]any)[newq] = "new-q"
			})
			ts := nsAgo(5 * time.Second)
			lines = append(lines,
				pwLine{TS: ts, Line: "ERROR payment provider unreachable", Container: "api", Q: "errors-q"}, // live on the established query
				pwLine{TS: ts, Line: "ERROR payment provider unreachable", Container: "api", Q: "new-q"})    // the same line, in the new query's first window
			h.lines.Store(lines)
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			t.Logf("%s: decide: %v | alerts=%v", newq, outs["decide"]["summary"], alertsOf(t, outs))
			if got := alertsOf(t, outs); len(got) != 1 || got[0] != "loki:new:medium" {
				t.Fatalf("%s: a line the established query reads live is news whatever the order of the queries: %v", newq, got)
			}
		})
	}
}

// R8-I: leak_sweep listed BEFORE errors in the operator's config (JSON key
// order is kept): every error line leak_sweep also returns is deduplicated
// under leak_sweep's record, which produces no template.
func TestProdWatch_LeakSweepFirstSwallowsTemplates(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, order := range []string{"errors-first", "sweep-first"} {
		order := order
		t.Run(order, func(t *testing.T) {
			h := newPWHarness(t)
			q := `"errors": "errors-q", "leak_sweep": "sweep-q"`
			if order == "sweep-first" {
				q = `"leak_sweep": "sweep-q", "errors": "errors-q"`
			}
			cfg := fmt.Sprintf(`{"app": {"name": "demo"}, "grafana": {"base_url": %q, "loki_uid": "loki"},
 "loki": {"queries": {%s}, "overlap_seconds": 60, "bootstrap_window_minutes": 10, "page_size": 1000},
 "sinks": [{"webhook": "w1", "channel": "#ops", "min_severity": "low"}]}`, h.srv.URL, q)
			_ = os.WriteFile(filepath.Join(h.ws, "prod-watch.json"), []byte(cfg), 0o644)
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // bootstrap
			ts := nsAgo(5 * time.Second)
			h.lines.Store([]pwLine{
				{TS: ts, Line: "ERROR database connection refused", Container: "api", Q: "errors-q"},
				{TS: ts, Line: "ERROR database connection refused", Container: "api", Q: "sweep-q"}, // the sweep returns every line
			})
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			t.Logf("%s: leak_scan: %v | decide: %v | alerts=%v", order, outs["leak_scan"]["summary"], outs["decide"]["summary"], alertsOf(t, outs))
			if got := alertsOf(t, outs); len(got) != 1 {
				t.Fatalf("%s: the error line must post its template: %v", order, got)
			}
		})
	}
}

// R8-J: the engine re-injects plan's output through a Go map, so poll_loki
// walks the queries in ALPHABETICAL order of their names. A template query
// named after "leak_sweep" (warnings, panics, timeouts, worker_errors, …)
// has every line the broad sweep also returns deduplicated under the
// sweep's record — which produces no template.
func TestProdWatch_QueryNamedAfterLeakSweepIsBlind(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, name := range []string{"errors", "warnings"} {
		name := name
		t.Run(name, func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) {
				lokiOnly(1000, 60)(cfg)
				cfg["loki"].(map[string]any)["queries"] = map[string]any{name: "tpl-q", "leak_sweep": "sweep-q"}
			})
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // bootstrap
			ts := nsAgo(5 * time.Second)
			h.lines.Store([]pwLine{
				{TS: ts, Line: "ERROR database connection refused", Container: "api", Q: "tpl-q"},
				{TS: ts, Line: "ERROR database connection refused", Container: "api", Q: "sweep-q"}, // the sweep returns every line of the namespace
			})
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			t.Logf("query %q: leak_scan: %v | alerts=%v", name, outs["leak_scan"]["summary"], alertsOf(t, outs))
			if got := alertsOf(t, outs); len(got) != 1 {
				t.Fatalf("query %q: the error line must post its template: %v", name, got)
			}
		})
	}
}

// R8-K: a truncated walk leaves an unread tail [frontier, mark); the query
// is dropped then re-added: the dropped cursor reopens at the mark (its
// bound), so the unread tail is skipped — no gap declared, coverage full.
func TestProdWatch_TruncatedTailLostOnDropAndReadd(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 300))
	lines := []pwLine{{TS: nsAgo(250 * time.Second), Line: "ERROR anchor", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	written := map[string]int{}
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	for i := 1; i <= 5; i++ { // late lines below the mark, inside the overlap
		lines = append(lines, pwLine{TS: nsAgo(240*time.Second) + int64(i)*int64(10*time.Second), Line: fmt.Sprintf("ERROR late %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 2)) // truncated at late 2: late 3..5 unread
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	t.Logf("tick 2: truncated=%v frontier=%v covered=%v", pq["truncated"], pq["frontier_ns"], pq["covered_to_ns"])
	h.writeConfig(t, func(cfg map[string]any) {
		lokiTwoQueries(1000, 300)(cfg)
		delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
	})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	h.writeConfig(t, lokiTwoQueries(1000, 300))
	for k := 0; k < 2; k++ {
		outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
		pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
		t.Logf("re-added tick %d: from=%v gap=%v lines=%v coverage=%v", k, pq["from_ns"], pq["gap"], pq["lines"], outs["leak_scan"]["coverage"])
	}
	if len(written) != 6 {
		t.Fatalf("the unread tail must be read after the re-add (or a gap declared): %d of 6 written (%v)", len(written), written)
	}
}

// R8-L: the cursor OBJECT itself (not a field) is foreign: plan must refuse by name, not trace back.
func TestProdWatch_ForeignCursorObjectTracesBack(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, cur := range map[string]string{"string-cursor": `{"errors":"x"}`, "list-cursor": `{"errors":[1]}`, "list-lane": `[1]`} {
		name, cur := name, cur
		t.Run(name, func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, lokiOnly(1000, 60))
			_ = os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755)
			_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), []byte(`{"version":1,"generation":1,"cursors":{"loki":`+cur+`},"incidents":{},"health":{}}`), 0o644)
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), map[string]string{"grafana_token": h.tokenFile}))
			last := stderr
			if i := strings.LastIndex(strings.TrimSpace(stderr), "\n"); i >= 0 {
				last = strings.TrimSpace(stderr)[i+1:]
			}
			t.Logf("%s: err=%v last stderr line: %s", name, err, last)
			if err == nil || strings.Contains(stderr, "Traceback") {
				t.Fatalf("%s: a foreign cursor must refuse by name: %v", name, err)
			}
		})
	}
}

// R8-X: an extended randomized drive — the dimensions the shipped generator
// does not sample: max_window changes (declared gaps), failures on FIRST
// walks with a burst, the query dropped and re-added, bursts under churn,
// zero-width ticks (lag 400 s). Oracle: no line written twice; a line
// inside a window is written unless a gap was DECLARED over it afterwards.
func TestProdWatch_ExtendedRandomDrive(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	seeds := 4
	if v, err := strconv.Atoi(os.Getenv("R8_SEEDS")); err == nil && v > 0 {
		seeds = v
	}
	first := 0
	if v, err := strconv.Atoi(os.Getenv("R8_FIRST")); err == nil && v > 0 {
		first = v
	}
	for seed := first; seed < first+seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rnd := rand.New(rand.NewSource(int64(seed)*104729 + 17))
			h := newPWHarness(t)
			overlap := []int{0, 1, 5, 60, 300}[rnd.Intn(5)]
			page := []int{1, 2, 3, 7, 1000}[rnd.Intn(5)]
			present := true
			configure := func() {
				h.writeConfig(t, func(cfg map[string]any) {
					lokiTwoQueries(page, overlap)(cfg)
					if !present {
						delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
					}
				})
			}
			configure()
			now := time.Now().UnixNano()
			var lines []pwLine
			type win struct {
				from, to int64
				gap      bool
			}
			var wins []win
			firstVisible := map[string]int{}
			written := map[string]int{}
			var log []string
			inject := func(n, tick int) {
				for i := 0; i < n; i++ {
					var ts int64
					switch rnd.Intn(3) {
					case 0:
						ts = time.Now().UnixNano() - int64(rnd.Intn(120))*int64(time.Second) - int64(rnd.Intn(1000))*int64(time.Millisecond)
					case 1:
						ts = time.Now().UnixNano() - int64(300+rnd.Intn(200))*int64(time.Second)
					default:
						if len(lines) > 0 {
							ts = lines[rnd.Intn(len(lines))].TS
						} else {
							ts = time.Now().UnixNano() - int64(rnd.Intn(60))*int64(time.Second)
						}
					}
					lines = append(lines, pwLine{TS: ts, Line: fmt.Sprintf("ERROR t%d-%d", tick, i), Container: "api", Q: "errors-q"})
				}
			}
			tick := func(k, maxLines, lag, maxWin int, failing bool) {
				t.Helper()
				h.lines.Store(append([]pwLine(nil), lines...))
				if failing {
					h.failLokiFrom.Store(int64(len(h.calls()) + 1 + rnd.Intn(8)))
					h.failLokiCount.Store(2)
				} else {
					h.failLokiFrom.Store(0)
				}
				outs := h.cursorTick(t, wf, cursorVars(h, maxWin, lag, maxLines))
				countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
				pqs := outs["poll_loki"]["per_query"].(map[string]any)
				pqa, ok := pqs["errors"]
				if !ok {
					log = append(log, fmt.Sprintf("tick %d: errors DROPPED", k))
					return
				}
				pq := pqa.(map[string]any)
				from, _ := strconv.ParseInt(pq["from_ns"].(string), 10, 64)
				to, _ := strconv.ParseInt(pq["to_ns"].(string), 10, 64)
				wins = append(wins, win{from, to, pq["gap"] == true})
				for _, l := range lines {
					if _, seen := firstVisible[l.Line]; !seen && l.TS >= from && l.TS < to {
						firstVisible[l.Line] = len(wins) - 1
					}
				}
				log = append(log, fmt.Sprintf("tick %d max_lines=%d lag=%d win=%dm overlap=%d page=%d failing=%v | from=%v to=%v lines=%v trunc=%v gap=%v err=%.40q covered=%v frontier=%v ofrom=%v band=%d boot=%v",
					k, maxLines, lag, maxWin, overlap, page, failing, pq["from_ns"], pq["to_ns"], pq["lines"], pq["truncated"], pq["gap"], pq["error"], pq["covered_to_ns"], pq["frontier_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)), pq["bootstrap"]))
			}
			if rnd.Intn(2) == 0 { // a burst inside the FIRST window, and the first walk fails deep
				base := now - 250*int64(time.Second)
				for i := 0; i < 4500+rnd.Intn(2000); i++ {
					lines = append(lines, pwLine{TS: base + int64(i)*int64(40*time.Millisecond), Line: fmt.Sprintf("ERROR boot-burst-%d", i), Container: "api", Q: "errors-q"})
				}
			}
			for k := 0; k < 12; k++ {
				overlap = []int{0, 1, 5, 60, 300}[rnd.Intn(5)]
				page = []int{1, 2, 3, 7, 1000}[rnd.Intn(5)]
				present = k == 0 || rnd.Intn(8) != 0
				configure()
				if k > 0 && rnd.Intn(6) == 0 {
					base := time.Now().UnixNano() - int64(rnd.Intn(200))*int64(time.Second)
					for i := 0; i < 4500; i++ {
						lines = append(lines, pwLine{TS: base + int64(i)*int64(30*time.Millisecond), Line: fmt.Sprintf("ERROR burst%d-%d", k, i), Container: "api", Q: "errors-q"})
					}
				}
				inject(rnd.Intn(9), k)
				lag := []int{0, 1, 2, 0, 400}[rnd.Intn(5)]
				maxWin := []int{60, 60, 60, 5, 1}[rnd.Intn(5)]
				if lag > 2 {
					maxWin = 60 // a cursor more than a full window ahead is an error by design
				}
				tick(k, []int{3, 7, 20, 5000, 6000}[rnd.Intn(5)], lag, maxWin, present && rnd.Intn(3) == 0)
			}
			present = true
			configure()
			for k := 12; k < 17; k++ {
				tick(k, 6000, 0, 60, false)
			}
			dump := func() {
				for _, l := range log {
					t.Log(l)
				}
			}
			var drops, gaps, fails, truncs, zero, bootErr int
			for _, l := range log {
				switch {
				case strings.Contains(l, "DROPPED"):
					drops++
				default:
					if strings.Contains(l, "gap=true") {
						gaps++
					}
					if !strings.Contains(l, `err=""`) {
						fails++
						if strings.Contains(l, "boot=true") {
							bootErr++
						}
					}
					if strings.Contains(l, "trunc=true") {
						truncs++
					}
					if i := strings.Index(l, "| from="); i >= 0 {
						var f, tt string
						fmt.Sscanf(strings.ReplaceAll(l[i+2:], " to=", " "), "from=%s %s", &f, &tt)
						if f == tt {
							zero++
						}
					}
				}
			}
			t.Logf("seed %d exercised: drops=%d gaps=%d failed-walks=%d (first-window=%d) truncated=%d zero-width=%d lines=%d", seed, drops, gaps, fails, bootErr, truncs, zero, len(lines))
			for l, n := range written {
				if n > 1 {
					dump()
					t.Fatalf("seed %d: %q written %d times", seed, l, n)
				}
			}
			for _, l := range lines {
				k, ok := firstVisible[l.Line]
				if !ok || written[l.Line] == 1 {
					continue
				}
				declared := false
				for j := k; j < len(wins); j++ {
					if wins[j].gap && wins[j].from > l.TS {
						declared = true
					}
				}
				if !declared {
					dump()
					t.Fatalf("seed %d: %q (ts %d) was inside window %d [%d, %d) and was written %d times, no gap declared over it", seed, l.Line, l.TS, k, wins[k].from, wins[k].to, written[l.Line])
				}
			}
		})
	}
}

// R8-M: the simplest drop/re-add: a truncated walk's mark is its LAST LINE
// (covered = last_ts, read), the dropped cursor reopens there inclusive with
// no band — that line is written twice.
func TestProdWatch_TruncatedThenDropAndReaddRewritesTheMarkLine(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	var lines []pwLine
	for i := 0; i < 5; i++ {
		lines = append(lines, pwLine{TS: nsAgo(30*time.Second) + int64(i)*int64(time.Second), Line: fmt.Sprintf("ERROR L%d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	written := map[string]int{}
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 2)) // truncated at L1: covered = frontier = L1's ts
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	h.writeConfig(t, func(cfg map[string]any) {
		lokiTwoQueries(1000, 60)(cfg)
		delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
	})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	t.Logf("written: %v", written)
	for l, n := range written {
		if n != 1 {
			t.Fatalf("%q written %d times", l, n)
		}
	}
}

// R8-N: a TRUNCATED first window (more lines than max_lines in the
// bootstrap window): the next tick is no longer a bootstrap (covered > 0),
// so the unread rest of the FIRST window is read as live lines and posted
// as news.
func TestProdWatch_TruncatedFirstWindowRestPostedAsNews(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, established := range []bool{false, true} {
		established := established
		t.Run(fmt.Sprintf("established=%v", established), func(t *testing.T) {
			h := newPWHarness(t)
			h.writeConfig(t, lokiTwoQueries(1000, 60))
			var lines []pwLine
			if established { // an install running for a while; the query is added now
				h.writeConfig(t, func(cfg map[string]any) {
					lokiTwoQueries(1000, 60)(cfg)
					delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
				})
				h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
				h.writeConfig(t, lokiTwoQueries(1000, 60))
			}
			for i := 0; i < 100; i++ { // history: 9 to 4 minutes ago
				lines = append(lines, pwLine{TS: nsAgo(9*time.Minute) + int64(i)*int64(3*time.Second), Line: fmt.Sprintf("ERROR legacy job %d failed", i), Container: "batch", Q: "errors-q"})
			}
			h.lines.Store(lines)
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 20)) // the first window, truncated at 20 lines
			pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
			t.Logf("tick 1: bootstrap=%v truncated=%v lines=%v alerts=%v", pq["bootstrap"], pq["truncated"], pq["lines"], alertsOf(t, outs))
			outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // no new line at all
			pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
			for _, a := range outs["decide"]["alerts"].([]any) {
				m := a.(map[string]any)
				t.Logf("tick 2: bootstrap=%v lines=%v ALERT %v %v %v fields=%v", pq["bootstrap"], pq["lines"], m["state"], m["severity"], m["title_arg"], m["fields"])
			}
			if got := alertsOf(t, outs); len(got) != 0 {
				t.Fatalf("no line was written after the install/add: the rest of the first window is history, not news: %v", got)
			}
		})
	}
}

// R8-O: a quieted incident (quiet_noted) whose template reappears in
// history lines only (tick_count 0): observed, no alert — but quiet_noted
// is reset and fields.count set to 0, so a SECOND "not observed any more"
// note follows later with no alert in between, rendering "0 line(s)".
func TestProdWatch_HistoryOnlyRecurrenceRearmsTheQuietNote(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	inc := incident("loki", "medium", true, 100, 100)
	inc["quiet_noted"] = true
	inc["fields"] = map[string]any{"count": 3, "first": "2026-09-19T10:00+00:00", "streams": "container=api"}
	st := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
		"incidents": map[string]any{"loki:t1": inc}}
	sig := map[string]any{"templates": []map[string]any{{"template_id": "t1", "query": "errors", "template": "ERROR job # failed", "count": 4, "count_live": 0,
		"first_ts": strconv.FormatInt(nsAgo(8*time.Minute), 10), "last_ts": strconv.FormatInt(nsAgo(7*time.Minute), 10), "sample": "ERROR job <num> failed", "streams": []string{"container=w"}}}, "leak": []any{}}
	out, stderr, err := pwDecide(t, wf, h, sig, st, nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	rec := pwStateNext(t, out)["incidents"].(map[string]any)["loki:t1"].(map[string]any)
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 0 || rec["quiet_noted"] != true || rec["fields"].(map[string]any)["count"].(float64) != 3 {
		t.Fatalf("a template seen only as history is not a recurrence: the incident keeps its quiet note and its fields: alerts=%v rec=%v", got, rec)
	}
	// 49 h later, nothing seen at all: a second quiet note fires.
	rec["last_seen"] = hoursAgo(49)
	next := pwStateNext(t, out)
	next["incidents"].(map[string]any)["loki:t1"] = rec
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out["alerts"].([]any)); n != 0 {
		t.Fatalf("the quiet note was already posted; a history-only sighting must not re-arm it: %v", out["alerts"])
	}
}

// R8-P (verification): a failed walk whose last page ends INSIDE a
// same-nanosecond group, with a band over the cap: the cut is clamped at
// the frontier group, so the retry writes the unseen members once and the
// seen ones never again.
func TestProdWatch_FailedWalkInsideAGroupWithFullBand(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 300))
	written := map[string]int{}
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 6000)) // empty: the mark at now
	base := nsAgo(250 * time.Second)
	var lines []pwLine
	var groupTS int64
	for i := 0; i < 5200; i++ {
		ts := base + int64(i)*int64(40*time.Millisecond)
		if i >= 4990 && i < 5010 {
			if groupTS == 0 {
				groupTS = ts
			}
			ts = groupTS
		}
		lines = append(lines, pwLine{TS: ts, Line: fmt.Sprintf("ERROR g %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	h.failLokiFrom.Store(int64(len(h.calls()) + 6)) // page 6 resumes AT the group's timestamp: it and its retry fail
	h.failLokiCount.Store(2)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 6000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	t.Logf("tick 1: err=%.30q lines=%v frontier=%v (group ts %d) bound=%v band=%d", pq["error"], pq["lines"], pq["frontier_ns"], groupTS, pq["overlap_from_ns"], len(pq["band"].([]any)))
	h.failLokiFrom.Store(0)
	for k := 0; k < 2; k++ {
		outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 6000))
		countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	}
	dups, missing := 0, 0
	for _, l := range lines {
		switch written[l.Line] {
		case 0:
			missing++
		case 1:
		default:
			dups++
		}
	}
	t.Logf("distinct=%d dups=%d missing=%d", len(written), dups, missing)
	if dups != 0 || missing != 0 {
		t.Fatalf("exactly once: dups=%d missing=%d", dups, missing)
	}
}

// R8-Q: an empty (zero-width) window: decide concludes nothing, but the
// coverage stays "full" — the comment's "the bot's own coverage note says so"
// does not hold for this case.
func TestProdWatch_ZeroWidthCoverageStaysFull(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	raw := filepath.Join(h.scratch, "empty.jsonl")
	_ = os.WriteFile(raw, nil, 0o644)
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": raw, "per_query": map[string]any{"errors": map[string]any{"lines": 0, "error": "", "truncated": false, "gap": false, "from_ns": "900", "to_ns": "900"}},
		"app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	t.Logf("zero-width window: coverage=%v", out["coverage"])
}

// R8-R: a template without `count_live` (a signals file from another
// version of leak_scan — a resumed run across an upgrade) is read as ALL
// history: the alert is swallowed silently instead of a named refusal.
func TestProdWatch_MissingCountLiveSwallowsTheAlert(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	st := map[string]any{"version": 1, "generation": 7, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	sig := map[string]any{"templates": []map[string]any{{"template_id": "t1", "query": "errors", "template": "ERROR job # failed", "count": 3,
		"first_ts": strconv.FormatInt(nsAgo(time.Minute), 10), "last_ts": strconv.FormatInt(nsAgo(time.Minute), 10), "sample": "ERROR job <num> failed", "streams": []string{"container=w"}}}, "leak": []any{}}
	_, stderr, err := pwDecide(t, wf, h, sig, st, nil)
	if err == nil || !strings.Contains(stderr, "count_live") || strings.Contains(stderr, "Traceback") {
		t.Fatalf("a template without count_live is refused by name (a scan from another version): %v %s", err, stderr)
	}
}

// R8-S: a falsy foreign band ({} / false / 0) is swapped for an empty band
// by `c.get('band') or []` — the rule the fields follow ("a falsy foreign
// value is never swapped for a default") does not hold for the band.
func TestProdWatch_FalsyForeignBandIsSwappedForEmpty(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, band := range []string{`{}`, `false`, `0`, `""`} {
		h := newPWHarness(t)
		h.writeConfig(t, lokiOnly(1000, 60))
		_ = os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755)
		_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), []byte(fmt.Sprintf(
			`{"version":1,"generation":1,"cursors":{"loki":{"errors":{"covered_to_ns":"%d","frontier_ns":"%d","band":%s,"band_base_ns":"0","overlap_from_ns":"0"}}},"incidents":{},"health":{}}`,
			nsAgo(20*time.Second), nsAgo(20*time.Second), band)), 0o644)
		_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), map[string]string{"grafana_token": h.tokenFile}))
		if err == nil || !strings.Contains(stderr, "band") || !strings.Contains(stderr, "errors") || strings.Contains(stderr, "Traceback") {
			t.Fatalf("band=%s: a falsy foreign band is refused by name, never read as empty: %v %q", band, err, strings.TrimSpace(stderr))
		}
	}
}
