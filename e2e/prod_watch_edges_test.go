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

// TestProdWatch_BootstrapRetryAfterACutOpensAtTheBound: a first walk that fails after its band was cut (more lines seen than the
// band keeps) raised the band's bound above where its window opened; the
// retry opens at that bound — never at a first window recomputed from the
// clock below it — so the cut lines are not written again.
func TestProdWatch_BootstrapRetryAfterACutOpensAtTheBound(t *testing.T) {
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

// TestProdWatch_BootstrapRetryAfterACutOpensAtTheBoundWithDefaults: the same with the default page size and max_lines (1000 / 5000): two
// failed first walks in a row — the retry re-reads the known band
// uncharged, sees more lines than the band keeps, fails again and cuts.
func TestProdWatch_BootstrapRetryAfterACutOpensAtTheBoundWithDefaults(t *testing.T) {
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

// TestProdWatch_TruncatedSweepDoesNotFreezeTemplateIncidents: the documented config — an error query and a broad sweep that reads more
// than max_lines every tick: template incidents are judged by the template
// queries, so a sweep running behind does not keep them from being quieted
// or forgotten.
func TestProdWatch_TruncatedSweepDoesNotFreezeTemplateIncidents(t *testing.T) {
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

// TestProdWatch_LiveTemplatesRankFirstInTheList: the template list keeps the 200 templates with the most LIVE lines: a
// new query's first window with 200 history templates never pushes a live
// template out, and the live alert is posted.
func TestProdWatch_LiveTemplatesRankFirstInTheList(t *testing.T) {
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

// TestProdWatch_FailedWalkThenDropAndReaddWritesOnce: a query whose walk failed after writing lines, then dropped and
// re-added: the dropped cursor keeps the band entries a re-add re-reads, so
// those lines are not written again.
func TestProdWatch_FailedWalkThenDropAndReaddWritesOnce(t *testing.T) {
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

// TestProdWatch_LiveAlertRendersItsLiveLinesOnly: a template fed by live and history lines renders its live lines only —
// count, first-seen, streams, sample and query.
func TestProdWatch_LiveAlertRendersItsLiveLinesOnly(t *testing.T) {
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

// TestProdWatch_TruncatedTwiceWhileOverlapNarrowsLosesNothing: two truncated walks in a row while the overlap narrows (300 s → 5 s):
// the second stops at a frontier far below the mark minus the overlap; the
// band's lower edge follows the frontier, so the next window reopens at the
// unread late lines.
func TestProdWatch_TruncatedTwiceWhileOverlapNarrowsLosesNothing(t *testing.T) {
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

// TestProdWatch_FailedWalkThenOverlapNarrowsReadsTheRest: a walk that fails among late lines below the mark, then the overlap
// narrows: the retry reopens where the walk stopped.
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

// TestProdWatch_SharedLineLivenessIgnoresQueryOrder: the same line read by a query still in its first window and by an
// established query: it is live for the established one, whichever query
// sorts first.
func TestProdWatch_SharedLineLivenessIgnoresQueryOrder(t *testing.T) {
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

// TestProdWatch_SweepListedFirstStillLeavesTemplates: the sweep listed before the error query in the config: a line both
// return still makes its template.
func TestProdWatch_SweepListedFirstStillLeavesTemplates(t *testing.T) {
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

// TestProdWatch_QueryNamedAfterTheSweepStillMakesTemplates: the engine walks the queries in the sorted order of their names; a
// template query named after the sweep (warnings, workers…) still makes its
// templates from the lines the sweep also returns.
func TestProdWatch_QueryNamedAfterTheSweepStillMakesTemplates(t *testing.T) {
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

// TestProdWatch_TruncatedTailSurvivesDropAndReadd: a truncated walk leaves an unread tail between its frontier and the
// mark; the query dropped then re-added reopens at the frontier and reads
// it.
func TestProdWatch_TruncatedTailSurvivesDropAndReadd(t *testing.T) {
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

// TestProdWatch_ForeignCursorObjectIsRefusedByName: a cursor that is not an object (a string, a list) is refused by name by
// plan, never a traceback.
func TestProdWatch_ForeignCursorObjectIsRefusedByName(t *testing.T) {
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

// TestProdWatch_ExtendedRandomDrive: drives the cursor chain over seventeen ticks per seed across what
// the regime drive does not combine: max-window changes (declared gaps),
// failures on FIRST walks inside a burst, the query dropped and re-added,
// bursts under churn, zero-width ticks (lag 400 s), a lag raised after a
// walk truncated below the mark. Each seed forces one of the five rare
// dimensions (seed % 5: first-window failure, gap, drop and re-add,
// zero-width, lag raised below the mark) and asserts it was exercised; the
// drain ticks must read cleanly. Oracle: the read-point oracle (no hole;
// every owed line written exactly once unless a legitimately declared gap
// excused it) and no line written twice.
func TestProdWatch_ExtendedRandomDrive(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	seeds := 5
	if v, err := strconv.Atoi(os.Getenv("R8_SEEDS")); err == nil && v > 0 {
		seeds = v
	}
	first := 0
	if v, err := strconv.Atoi(os.Getenv("R8_FIRST")); err == nil && v > 0 {
		first = v
	}
	dimensions := []string{"first-window failure", "gap", "drop/re-add", "zero-width", "lag raised below the mark"}
	for seed := first; seed < first+seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rnd := rand.New(rand.NewSource(int64(seed)*104729 + 17))
			force := seed % 5
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
			now := time.Now().UnixNano()
			var lines []pwLine
			written := map[string]int{}
			var log []string
			dump := func() {
				for _, l := range log {
					t.Log(l)
				}
			}
			oracle := newPWReadOracle("errors-q")
			oracle.onFail = dump
			var drops, gaps, fails, bootErr, truncs, zero, lagBelowMark int
			var lastCovered int64
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
			// failAt: 0 no failure; -1 a random call among the tick's first
			// eight; n > 0 the tick's n-th Loki call (and its retry).
			tick := func(k, maxLines, lag, maxWin, failAt int) map[string]any {
				t.Helper()
				h.lines.Store(append([]pwLine(nil), lines...))
				switch {
				case failAt < 0:
					h.failLokiFrom.Store(int64(len(h.calls()) + 1 + rnd.Intn(8)))
					h.failLokiCount.Store(2)
				case failAt > 0:
					h.failLokiFrom.Store(int64(len(h.calls()) + failAt))
					h.failLokiCount.Store(2)
				default:
					h.failLokiFrom.Store(0)
				}
				servedFrom := h.servedLen()
				outs := h.cursorTick(t, wf, cursorVars(h, maxWin, lag, maxLines))
				countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
				pqa, ok := outs["poll_loki"]["per_query"].(map[string]any)["errors"]
				if !ok {
					drops++
					log = append(log, fmt.Sprintf("tick %d: errors DROPPED", k))
					return nil
				}
				pq := pqa.(map[string]any)
				from, _ := strconv.ParseInt(fmt.Sprint(pq["from_ns"]), 10, 64)
				to, _ := strconv.ParseInt(fmt.Sprint(pq["to_ns"]), 10, 64)
				covered, _ := strconv.ParseInt(fmt.Sprint(pq["covered_to_ns"]), 10, 64)
				if lastCovered != 0 && from < to && to < lastCovered && pq["truncated"] != true && pq["error"] == "" {
					lagBelowMark++ // a complete walk whose window ends below the mark
				}
				lastCovered = covered
				if pq["gap"] == true {
					gaps++
				}
				if pq["error"] != "" {
					fails++
					if pq["bootstrap"] == true {
						bootErr++
					}
				}
				if pq["truncated"] == true {
					truncs++
				}
				if pq["from_ns"] == pq["to_ns"] {
					zero++
				}
				log = append(log, fmt.Sprintf("tick %d max_lines=%d lag=%d win=%dm overlap=%d page=%d failAt=%d | from=%v to=%v lines=%v trunc=%v gap=%v err=%.40q covered=%v frontier=%v ofrom=%v band=%d boot=%v",
					k, maxLines, lag, maxWin, overlap, page, failAt, pq["from_ns"], pq["to_ns"], pq["lines"], pq["truncated"], pq["gap"], pq["error"], pq["covered_to_ns"], pq["frontier_ns"], pq["overlap_from_ns"], len(pq["band"].([]any)), pq["bootstrap"]))
				oracle.observe(t, k, pq, h.servedSince(servedFrom), lines, maxWin)
				return pq
			}
			if force <= 1 || rnd.Intn(2) == 0 { // a burst inside the FIRST window
				base := now - 250*int64(time.Second)
				for i := 0; i < 4500+rnd.Intn(2000); i++ {
					lines = append(lines, pwLine{TS: base + int64(i)*int64(40*time.Millisecond), Line: fmt.Sprintf("ERROR boot-burst-%d", i), Container: "api", Q: "errors-q"})
				}
			}
			for k := 0; k < 12; k++ {
				overlap = []int{0, 1, 5, 60, 300}[rnd.Intn(5)]
				page = []int{1, 2, 3, 7, 1000}[rnd.Intn(5)]
				present = k == 0 || rnd.Intn(8) != 0
				if k > 0 && rnd.Intn(6) == 0 {
					base := time.Now().UnixNano() - int64(rnd.Intn(200))*int64(time.Second)
					for i := 0; i < 4500; i++ {
						lines = append(lines, pwLine{TS: base + int64(i)*int64(30*time.Millisecond), Line: fmt.Sprintf("ERROR burst%d-%d", k, i), Container: "api", Q: "errors-q"})
					}
				}
				inject(rnd.Intn(9), k)
				maxLines := []int{3, 7, 20, 5000, 6000}[rnd.Intn(5)]
				lag := []int{0, 1, 2, 0, 400}[rnd.Intn(5)]
				maxWin := []int{60, 60, 60, 5, 1}[rnd.Intn(5)]
				failAt := 0
				if rnd.Intn(3) == 0 {
					failAt = -1
				}
				// The seed's forced dimension overrides the draw.
				switch {
				case force == 0 && k == 0: // the first walk fails on its second page, inside the burst
					page, maxLines, lag, maxWin, failAt = 1000, 6000, 0, 60, 2
				case force == 1 && k == 0: // truncated inside the burst …
					page, maxLines, lag, maxWin, failAt = 7, 20, 0, 60, 0
				case force == 1 && k == 1: // … then a max window the frontier falls out of
					present, maxLines, lag, maxWin, failAt = true, 5000, 0, 1, 0
				case force == 2 && k == 3:
					present = false
				case force == 2 && k == 4:
					present = true
				case force == 3 && k == 1: // a complete walk …
					present, maxLines, lag, maxWin, failAt = true, 6000, 0, 60, 0
				case force == 3 && k == 2: // … then the lag raised past the mark
					present, lag, maxWin, failAt = true, 400, 60, 0
				case force == 4 && k == 0: // a complete walk sets the mark …
					present, maxLines, lag, maxWin, failAt, overlap = true, 6000, 0, 60, 0, 60
				case force == 4 && k == 1: // … late lines below it truncate the next walk …
					present, maxLines, lag, maxWin, failAt, overlap = true, 3, 0, 60, 0, 60
					for i := 0; i < 10; i++ {
						lines = append(lines, pwLine{TS: lastCovered - int64(40-3*i)*int64(time.Second), Line: fmt.Sprintf("ERROR late-below-mark-%d", i), Container: "api", Q: "errors-q"})
					}
				case force == 4 && k == 2: // … the lag raised so the window ends between the frontier and the mark …
					present, maxLines, maxWin, failAt, overlap = true, 6000, 60, 0, 0
					lag = int((time.Now().UnixNano()-lastCovered)/int64(time.Second)) + 25
				case force == 4 && k == 3: // … and the next window opens where that walk stopped
					present, maxLines, lag, maxWin, failAt, overlap = true, 6000, 0, 60, 0, 0
				}
				if lag > 2 && maxWin < 60 {
					maxWin = 60 // a cursor more than a full window ahead is an error by design
				}
				if !present {
					failAt = 0
				}
				configure()
				tick(k, maxLines, lag, maxWin, failAt)
			}
			present = true
			configure()
			for k := 12; k < 17; k++ {
				if pq := tick(k, 6000, 0, 60, 0); pq == nil || pq["error"] != "" {
					dump()
					t.Fatalf("seed %d: drain tick %d must read cleanly: %v", seed, k, pq)
				}
			}
			t.Logf("seed %d (forced: %s) exercised: drops=%d gaps=%d failed-walks=%d (first-window=%d) truncated=%d zero-width=%d lag-below-mark=%d lines=%d",
				seed, dimensions[force], drops, gaps, fails, bootErr, truncs, zero, lagBelowMark, len(lines))
			if []int{bootErr, gaps, drops, zero, lagBelowMark}[force] == 0 {
				dump()
				t.Fatalf("seed %d did not exercise its forced dimension (%s)", seed, dimensions[force])
			}
			for l, n := range written {
				if n > 1 {
					dump()
					t.Fatalf("seed %d: %q written %d times", seed, l, n)
				}
			}
			oracle.settle(t, written)
		})
	}
}

// TestProdWatch_TruncatedThenDropAndReaddWritesTheMarkLineOnce: a truncated walk's mark is its last line read; the dropped cursor keeps
// it in its band, so the re-added query does not write it again.
func TestProdWatch_TruncatedThenDropAndReaddWritesTheMarkLineOnce(t *testing.T) {
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

// TestProdWatch_TruncatedFirstWindowRestStaysHistory: a first window with more lines than max_lines: the rest, read on later
// ticks, is still history — never posted as news.
func TestProdWatch_TruncatedFirstWindowRestStaysHistory(t *testing.T) {
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

// TestProdWatch_HistoryOnlySightingKeepsTheQuietNote: a quieted incident whose template reappears in history lines only keeps
// its quiet note and its fields: no second "not observed any more" note
// follows.
func TestProdWatch_HistoryOnlySightingKeepsTheQuietNote(t *testing.T) {
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

// TestProdWatch_FailedWalkInsideAGroupWithFullBand: a failed walk whose last page ends inside a same-nanosecond group, with
// a band over the cap: the cut is clamped at the frontier group, so the
// retry writes the unseen members once and the seen ones never again.
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

// TestProdWatch_ZeroWidthCoverageStaysFull: an empty (zero-width) window is fully covered — nothing was there to
// read, the next window reads what comes — so the coverage stays full;
// decide concludes nothing about it either.
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
	if out["coverage"] != "full" {
		t.Fatalf("an empty window is fully covered: coverage=%v", out["coverage"])
	}
}

// TestProdWatch_ScanWithoutCountLiveIsRefused: a signals file whose templates lack count_live (a scan from another
// version of the bot, a resumed run across an upgrade) is refused by name,
// never read as all history.
func TestProdWatch_ScanWithoutCountLiveIsRefused(t *testing.T) {
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

// TestProdWatch_FalsyForeignBandIsRefused: a falsy foreign band ({}, false, 0, "") is refused by name, never read
// as empty.
func TestProdWatch_FalsyForeignBandIsRefused(t *testing.T) {
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

// TestProdWatch_LokiLagRaisedAfterATruncatedWalkLosesNothing: a walk
// truncated among late lines below the mark leaves its frontier below the
// mark; the ingest lag is then raised so that the next window ends between
// the frontier and the mark. That walk is complete, and its frontier moves
// to its own end — never up to the mark — so the late lines between the two
// are read on the next tick, not skipped with coverage reported full.
func TestProdWatch_LokiLagRaisedAfterATruncatedWalkLosesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 50))
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000)) // the mark at now, nothing to read
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	mark, _ := strconv.ParseInt(pq["covered_to_ns"].(string), 10, 64)
	var lines []pwLine
	for _, d := range []int{40, 35, 30, 20, 15, 10} {
		lines = append(lines, pwLine{TS: mark - int64(d)*int64(time.Second), Line: fmt.Sprintf("ERROR late %ds before the mark", d), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	written := map[string]int{}
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 3))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	frontier, _ := strconv.ParseInt(pq["frontier_ns"].(string), 10, 64)
	if pq["truncated"] != true || frontier != mark-30*int64(time.Second) {
		t.Fatalf("tick 2 is truncated at the third late line: %v", pq)
	}
	h.writeConfig(t, lokiOnly(1000, 0))
	lag := int((time.Now().UnixNano()-mark)/int64(time.Second)) + 25 // the window ends ~25 s below the mark
	outs = h.cursorTick(t, wf, cursorVars(h, 60, lag, 5000))
	countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	from, _ := strconv.ParseInt(pq["from_ns"].(string), 10, 64)
	to, _ := strconv.ParseInt(pq["to_ns"].(string), 10, 64)
	if from >= to || to <= frontier || to >= mark || pq["truncated"] == true || pq["error"] != "" {
		t.Fatalf("tick 3 is a complete walk whose window ends between the frontier and the mark: from=%d to=%d frontier=%d mark=%d %v", from, to, frontier, mark, pq)
	}
	for k := 4; k <= 5; k++ {
		outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		countRawLines(t, written, outs["poll_loki"]["raw_file"].(string))
	}
	for _, l := range lines {
		if written[l.Line] != 1 {
			t.Fatalf("%q written %d times (want exactly once): %v", l.Line, written[l.Line], written)
		}
	}
}

// TestProdWatch_BootstrapRetryKeepsTheFirstWindowBoundary: a first walk
// that fails keeps the end of its first window as the history boundary; the
// retry reads the first window as history and a line stamped after that end
// as news — the boundary does not slide to the retry's own window end.
func TestProdWatch_BootstrapRetryKeepsTheFirstWindowBoundary(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	lines := []pwLine{{TS: nsAgo(300 * time.Second), Line: "ERROR legacy batch failed", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.failLokiFrom.Store(int64(len(h.calls()) + 1)) // the first page of errors-q, and its retry
	h.failLokiCount.Store(2)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	if pq["error"] == "" {
		t.Fatalf("tick 1: the first walk fails: %v", pq)
	}
	boundary, _ := strconv.ParseInt(pq["to_ns"].(string), 10, 64)
	h.failLokiFrom.Store(0)
	time.Sleep(2 * time.Second)
	lines = append(lines, pwLine{TS: boundary + int64(time.Second), Line: "ERROR payment gateway down", Container: "api", Q: "errors-q"})
	h.lines.Store(lines)
	time.Sleep(time.Second)
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	pq = outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
	if pq["history_to_ns"] != strconv.FormatInt(boundary, 10) || pq["lines"].(float64) != 2 {
		t.Fatalf("the retry keeps the first window's end as the boundary and reads both lines: %v (boundary %d)", pq, boundary)
	}
	alerts := outs["decide"]["alerts"].([]any)
	if len(alerts) != 1 || alerts[0].(map[string]any)["state"] != "new" || !strings.Contains(fmt.Sprint(alerts[0].(map[string]any)["title_arg"]), "payment gateway down") {
		t.Fatalf("only the line past the first window is news: %v", alerts)
	}
}

// TestProdWatch_CutTemplateListConcludesNothing: more live templates than
// the list keeps: the cut is declared, the tick is partial coverage with the
// cut named in the coverage note, and no template incident is concluded on —
// the incident whose template was cut out of the list was seen this tick.
func TestProdWatch_CutTemplateListConcludesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 60))
	lines := []pwLine{{TS: nsAgo(100 * time.Second), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	const known = "ERROR payment gateway refused the card"
	st := h.state(t)
	inc := incident("loki", "medium", true, 49, 49)
	inc["title_arg"] = known
	st["incidents"] = map[string]any{pwTemplateFP(known): inc}
	h.setState(t, st)
	word := func(i int) string {
		w := ""
		for j := 0; j < 4; j++ {
			w += string(rune('a' + i%26))
			i /= 26
		}
		return w
	}
	base := nsAgo(20 * time.Second)
	for i := 0; i < 205; i++ { // 205 live templates, two lines each, outrank the known one
		for r := 0; r < 2; r++ {
			lines = append(lines, pwLine{TS: base + int64(i*2+r)*int64(time.Millisecond), Line: "ERROR worker " + word(i) + " crashed", Container: "api", Q: "errors-q"})
		}
	}
	lines = append(lines, pwLine{TS: nsAgo(5 * time.Second), Line: known, Container: "api", Q: "errors-q"})
	h.lines.Store(lines)
	outs := h.cursorTickWith(t, wf, cursorVars(h, 60, 0, 5000), map[string]any{"max_alerts": 1000})
	b, err := os.ReadFile(outs["leak_scan"]["signals_file"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var sig map[string]any
	if err := json.Unmarshal(b, &sig); err != nil {
		t.Fatal(err)
	}
	if sig["templates_cut"] != true || sig["coverage"] != "partial" {
		t.Fatalf("a cut list is declared and makes the tick partial: cut=%v coverage=%v", sig["templates_cut"], sig["coverage"])
	}
	if q := pwQuietFPs(outs); len(q) != 0 {
		t.Fatalf("no template incident is concluded on a cut list: %v", q)
	}
	var reasons string
	for _, s := range outs["decide"]["stale_sources"].([]any) {
		if m := s.(map[string]any); m["source"] == "coverage" {
			reasons = fmt.Sprint(m["reasons"])
		}
	}
	if !strings.Contains(reasons, "template list cut: 206 templates") {
		t.Fatalf("the coverage note names the cut and its size (205 worker templates and the known one): %q", reasons)
	}
}

// TestProdWatch_DroppedFirstWindowStaysHistoryWhenReadded: a query whose
// first window was truncated, dropped for a tick and re-added, reads the
// rest of its first window as history — the dropped cursor keeps the
// boundary.
func TestProdWatch_DroppedFirstWindowStaysHistoryWhenReadded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	base := nsAgo(540 * time.Second)
	var lines []pwLine
	for i := 0; i < 100; i++ {
		lines = append(lines, pwLine{TS: base + int64(i)*int64(3*time.Second), Line: fmt.Sprintf("ERROR legacy job %d failed", i), Container: "batch", Q: "errors-q"})
	}
	h.lines.Store(lines)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 20))
	if pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any); pq["truncated"] != true || len(outs["decide"]["alerts"].([]any)) != 0 {
		t.Fatalf("tick 1 reads 20 lines of the first window, as history: %v %v", pq, outs["decide"]["alerts"])
	}
	h.writeConfig(t, func(cfg map[string]any) {
		lokiTwoQueries(1000, 60)(cfg)
		delete(cfg["loki"].(map[string]any)["queries"].(map[string]any), "errors")
	})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	if n := outs["poll_loki"]["lines"].(float64); n != 80 || len(outs["decide"]["alerts"].([]any)) != 0 {
		t.Fatalf("the re-added query reads the 80 remaining first-window lines as history: lines=%v alerts=%v", n, outs["decide"]["alerts"])
	}
}

// TestProdWatch_HistoryOnlySightingKeepsTheIncidentSeen: a known template
// seen only in a new query's first window is not a recurrence (no alert, no
// count), but it was SEEN: no "not observed any more" note that tick, and
// its clock follows the newest history line, so none the next tick either.
func TestProdWatch_HistoryOnlySightingKeepsTheIncidentSeen(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 60))
	lines := []pwLine{{TS: nsAgo(120 * time.Second), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	const known = "ERROR payment gateway refused the card"
	fp := pwTemplateFP(known)
	st := h.state(t)
	inc := incident("loki", "medium", true, 49, 49)
	inc["title_arg"] = known
	st["incidents"] = map[string]any{fp: inc}
	h.setState(t, st)
	h.writeConfig(t, func(cfg map[string]any) {
		lokiOnly(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["queries"].(map[string]any)["zzz_new"] = "new-q"
	})
	for i := 0; i < 5; i++ {
		lines = append(lines, pwLine{TS: nsAgo(time.Duration(300-i) * time.Second), Line: known, Container: "api", Q: "new-q"})
	}
	h.lines.Store(lines)
	outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	if q := pwQuietFPs(outs); len(q) != 0 || len(outs["decide"]["alerts"].([]any)) != 0 {
		t.Fatalf("a history-only sighting posts nothing, no quiet note included: %v", outs["decide"]["alerts"])
	}
	seen := h.state(t)["incidents"].(map[string]any)[fp].(map[string]any)["last_seen"]
	if at, err := time.Parse(time.RFC3339, fmt.Sprint(seen)); err != nil || time.Since(at) > 10*time.Minute {
		t.Fatalf("the incident's clock follows the newest history line: last_seen=%v (%v)", seen, err)
	}
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	if q := pwQuietFPs(outs); len(q) != 0 {
		t.Fatalf("nor the next tick: %v", q)
	}
}

// TestProdWatch_SweepOnlyConfigConcludesNothingAboutTemplates: with only
// the sweep configured, no query can observe a template: template incidents
// are never concluded on — retention alone ends them — whether the sweep
// is truncated or complete.
func TestProdWatch_SweepOnlyConfigConcludesNothingAboutTemplates(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, truncated := range []bool{true, false} {
		truncated := truncated
		t.Run(fmt.Sprintf("truncated=%v", truncated), func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			withQueries := func(queries map[string]any) func(cfg map[string]any) {
				return func(cfg map[string]any) {
					lokiOnly(1000, 60)(cfg)
					cfg["loki"].(map[string]any)["queries"] = queries
				}
			}
			h.writeConfig(t, withQueries(map[string]any{"errors": "errors-q", "leak_sweep": "sweep-q"}))
			lines := []pwLine{{TS: nsAgo(100 * time.Second), Line: "INFO warmup", Container: "api", Q: "sweep-q"}}
			h.lines.Store(lines)
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 20))
			st := h.state(t)
			inc := incident("loki", "medium", true, 49, 49)
			inc["title_arg"] = "ERROR payment gateway refused the card"
			st["incidents"] = map[string]any{"loki:aaaaaaaaaaaa": inc}
			h.setState(t, st)
			h.writeConfig(t, withQueries(map[string]any{"leak_sweep": "sweep-q"}))
			base := nsAgo(20 * time.Second)
			for i := 0; i < 60; i++ {
				lines = append(lines, pwLine{TS: base + int64(i)*int64(10*time.Millisecond), Line: fmt.Sprintf("INFO GET /x %d", i), Container: "api", Q: "sweep-q"})
			}
			h.lines.Store(lines)
			maxLines := 5000
			if truncated {
				maxLines = 20
			}
			outs := h.cursorTick(t, wf, cursorVars(h, 60, 0, maxLines))
			if pq := outs["poll_loki"]["per_query"].(map[string]any)["leak_sweep"].(map[string]any); pq["truncated"] != truncated {
				t.Fatalf("the sweep's walk: %v", pq)
			}
			if q := pwQuietFPs(outs); len(q) != 0 {
				t.Fatalf("no template query is configured: nothing is concluded about template incidents: %v", q)
			}
		})
	}
}

// TestProdWatch_ForeignStateContainersAreRefusedByName: plan validates the
// whole state it reads — its top-level containers and EVERY cursor of the
// Loki map, configured or not — and refuses a foreign value by name before
// any lane runs; a falsy foreign value is never read as empty.
func TestProdWatch_ForeignStateContainersAreRefusedByName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	cursor := func(v string) string {
		return `{"version":1,"generation":1,"cursors":{"loki":{"gone":` + v + `}},"incidents":{},"health":{}}`
	}
	cases := []struct{ name, state, want string }{
		{"the state is a list", `[]`, "state"},
		{"cursors is a list", `{"version":1,"generation":1,"cursors":[],"incidents":{},"health":{}}`, "cursors"},
		{"cursors.loki is a list", `{"version":1,"generation":1,"cursors":{"loki":[]},"incidents":{},"health":{}}`, "cursors.loki"},
		{"cursors.loki is zero", `{"version":1,"generation":1,"cursors":{"loki":0},"incidents":{},"health":{}}`, "cursors.loki"},
		{"cursors.loki is an empty string", `{"version":1,"generation":1,"cursors":{"loki":""},"incidents":{},"health":{}}`, "cursors.loki"},
		{"incidents is a list", `{"version":1,"generation":1,"cursors":{},"incidents":[],"health":{}}`, "incidents"},
		{"health is a string", `{"version":1,"generation":1,"cursors":{},"incidents":{},"health":"x"}`, "health"},
		{"halt is a string", `{"version":1,"generation":1,"cursors":{},"incidents":{},"health":{},"halt":"x"}`, "halt"},
		{"a dropped cursor's band is a number", cursor(`{"covered_to_ns":"5","frontier_ns":"5","band":5,"band_base_ns":"0","overlap_from_ns":"0"}`), "'gone'"},
		{"a dropped cursor's mark is not a number", cursor(`{"covered_to_ns":"x"}`), "'gone'"},
		{"a dropped cursor's band entry has no numeric offset", cursor(`{"covered_to_ns":"5","band":["x:abcdabcdabcdabcd"]}`), "'gone'"},
		{"incidents is null", `{"version":1,"generation":1,"cursors":{},"incidents":null,"health":{}}`, "incidents"},
		{"health is null", `{"version":1,"generation":1,"cursors":{},"incidents":{},"health":null}`, "health"},
		{"cursors is null", `{"version":1,"generation":1,"cursors":null,"incidents":{},"health":{}}`, "cursors"},
		{"cursors.loki is null", `{"version":1,"generation":1,"cursors":{"loki":null},"incidents":{},"health":{}}`, "cursors.loki"},
		{"generation is not a number", `{"version":1,"generation":"x","cursors":{},"incidents":{},"health":{}}`, "generation"},
		{"an incident is a string", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":"x"},"health":{}}`, "incidents["},
		{"an incident's count is not a number", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"count":"x"}},"health":{}}`, ".count"},
		{"an incident's severity is a list", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"severity":["high"]}},"health":{}}`, ".severity"},
		{"an incident's last sighting is not a date", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"last_seen":"yesterday"}},"health":{}}`, ".last_seen"},
		{"a health record is a number", `{"version":1,"generation":1,"cursors":{},"incidents":{},"health":{"loki":5}}`, "health["},
		{"an incident's fields are a list", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"fields":[1]}},"health":{}}`, ".fields"},
		{"an incident's kind is a list", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"kind":["x"]}},"health":{}}`, ".kind"},
		{"an incident's title key is a list", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"title_key":["x"]}},"health":{}}`, ".title_key"},
		{"an incident's detail key is a number", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"detail_key":5}},"health":{}}`, ".detail_key"},
		{"an incident's first sighting is a number", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"first_seen":5}},"health":{}}`, ".first_seen"},
		{"an incident's sources are text", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"sources":"errors"}},"health":{}}`, ".sources"},
		{"an incident's sources hold a list", `{"version":1,"generation":1,"cursors":{},"incidents":{"loki:a":{"sources":[["errors"]]}},"health":{}}`, ".sources"},
		{"the posted coverage kinds are a list", `{"version":1,"generation":1,"cursors":{},"incidents":{},"health":{},"coverage_posted":[]}`, "coverage_posted"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, lokiOnly(1000, 60))
			_ = os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755)
			_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), []byte(c.state), 0o644)
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), map[string]string{"grafana_token": h.tokenFile}))
			if err == nil || strings.Contains(stderr, "Traceback") || !strings.Contains(stderr, "prod-watch:") || !strings.Contains(stderr, c.want) {
				t.Fatalf("%s: plan must refuse by name (%q): %v %s", c.name, c.want, err, stderr)
			}
		})
	}
}

// TestProdWatch_LeakScanCountsALineOnceAcrossQueries: a line two queries
// return (the sweep and a template query, or two template queries) is one
// record: it makes one template line and one leak finding, never two.
func TestProdWatch_LeakScanCountsALineOnceAcrossQueries(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	raw := filepath.Join(h.scratch, "raw.jsonl")
	var buf strings.Builder
	for _, q := range []string{"errors", "leak_sweep", "warnings"} {
		rec, _ := json.Marshal(map[string]any{"q": q, "ts": "1790000000000000001", "line": "ERROR payment for carte 4111 1111 1111 1111 declined", "stream": map[string]string{"container": "api"}})
		buf.Write(rec)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(raw, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	one := map[string]any{"lines": 1, "error": "", "truncated": false, "gap": false, "history_to_ns": "0"}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": raw, "per_query": map[string]any{"errors": one, "leak_sweep": one, "warnings": one},
		"app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil {
		t.Fatalf("leak_scan: %v\n%s", err, stderr)
	}
	b, _ := os.ReadFile(out["signals_file"].(string))
	var sig map[string]any
	if err := json.Unmarshal(b, &sig); err != nil {
		t.Fatal(err)
	}
	tpls := sig["templates"].([]any)
	leaks := sig["leak"].([]any)
	if out["lines_scanned"].(float64) != 1 || len(tpls) != 1 || tpls[0].(map[string]any)["count"].(float64) != 1 || len(leaks) != 1 || leaks[0].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("one line, whatever the queries that returned it: scanned=%v templates=%v leaks=%v", out["lines_scanned"], tpls, leaks)
	}
}

// TestProdWatch_FloodGapIsAnnouncedWithItsReasons: a sustained flood
// truncates every walk, then the frontier falls out of the max window and
// the lane skips lines (a declared gap). The coverage note announces the
// truncation, then the gap — lines skipped, with its reason — and stays
// quiet while nothing changes in kind.
func TestProdWatch_FloodGapIsAnnouncedWithItsReasons(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 0))
	var lines []pwLine
	start := time.Now().Add(-90 * time.Second).UnixNano()
	for i := 0; i < 3000; i++ { // 20 lines a second for 150 s
		lines = append(lines, pwLine{TS: start + int64(i)*int64(50*time.Millisecond), Line: fmt.Sprintf("ERROR flood %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	var notes []string
	for k := 0; k < 4; k++ {
		outs := h.cursorTick(t, wf, cursorVars(h, 1, 0, 20))
		pq := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)
		note := ""
		for _, s := range outs["decide"]["stale_sources"].([]any) {
			if m := s.(map[string]any); m["source"] == "coverage" {
				note = fmt.Sprint(m["reasons"])
			}
		}
		notes = append(notes, note)
		if k == 0 && (pq["truncated"] != true || pq["gap"] == true) {
			t.Fatalf("tick 0 is truncated, without a gap: %v", pq)
		}
		if k > 0 && pq["gap"] != true {
			t.Fatalf("tick %d: the frontier fell out of the max window: %v", k, pq)
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(notes[0], "errors: truncated") {
		t.Fatalf("tick 0 announces the truncation: %q", notes[0])
	}
	if !strings.Contains(notes[1], "errors: gap") || !strings.Contains(notes[1], "skipped") {
		t.Fatalf("tick 1 announces the gap and that lines were skipped: %q", notes[1])
	}
	if notes[2] != "" || notes[3] != "" {
		t.Fatalf("nothing changed in kind: no further note: %q", notes[2:])
	}
}

// TestProdWatch_CoverageReasonsPutTheGapFirst: long lane errors never push
// a gap — lines lost — out of the note's budget.
func TestProdWatch_CoverageReasonsPutTheGapFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var perr []any
	for i := 0; i < 4; i++ {
		perr = append(perr, map[string]any{"probe": fmt.Sprintf("probe%d", i), "error": "ValueError: Prometheus answered status 'error': " + strings.Repeat("x", 160)})
	}
	state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	signals := map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}
	out, stderr, err := pwDecide(t, wf, h, signals, state, map[string]any{
		"loki_per_query": map[string]any{"errors": map[string]any{"lines": 20, "error": "", "truncated": true, "gap": true, "from_ns": "100", "to_ns": "900"}},
		"prom_ok":        false, "prom_errors": perr, "lanes": map[string]any{"loki": true, "prometheus": true, "probes": false}})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	for _, s := range out["stale_sources"].([]any) {
		if m := s.(map[string]any); m["source"] == "coverage" {
			if r := fmt.Sprint(m["reasons"]); !strings.HasPrefix(r, "errors: gap") {
				t.Fatalf("the gap leads the reasons: %q", r)
			}
			return
		}
	}
	t.Fatalf("a coverage note is posted: %v", out["stale_sources"])
}

// TestProdWatch_CoverageNoteKeysOnTheKindOfPartiality: two queries truncated
// in turn do not re-post the note every tick (truncation loses nothing); a
// query entering a gap does (lines are lost); under a standing gap, a cut
// appearing is said, a truncation ending or coming back within the interval
// is not.
func TestProdWatch_CoverageNoteKeysOnTheKindOfPartiality(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	window := func(truncated, gap bool) map[string]any {
		return map[string]any{"lines": 20, "error": "", "truncated": truncated, "gap": gap, "from_ns": "100", "to_ns": "900"}
	}
	state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	signals := map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}
	lanes := map[string]any{"loki": true, "prometheus": false, "probes": false}
	note := func(out map[string]any) string {
		for _, s := range out["stale_sources"].([]any) {
			if m := s.(map[string]any); m["source"] == "coverage" {
				return fmt.Sprint(m["reasons"])
			}
		}
		return ""
	}
	cut := map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial", "templates_cut": true, "templates_total": 230}
	var notes []string
	for _, tick := range []struct {
		pq  map[string]any
		sig map[string]any
	}{
		{map[string]any{"errors": window(true, false), "other": window(false, false)}, signals},
		{map[string]any{"errors": window(false, false), "other": window(true, false)}, signals},
		{map[string]any{"errors": window(true, false), "other": window(false, false)}, signals},
		{map[string]any{"errors": window(true, true), "other": window(false, false)}, signals},
		{map[string]any{"errors": window(false, true), "other": window(false, false)}, signals}, // the truncation ends, the gap stays
		{map[string]any{"errors": window(false, true), "other": window(false, false)}, cut},     // a cut appears under the gap
		{map[string]any{"errors": window(false, true), "other": window(true, false)}, cut},      // a truncation appears under the gap and the cut
	} {
		out, stderr, err := pwDecide(t, wf, h, tick.sig, state, map[string]any{"loki_per_query": tick.pq, "lanes": lanes})
		if err != nil {
			t.Fatalf("decide: %v %s", err, stderr)
		}
		notes = append(notes, note(out))
		state = pwStateNext(t, out)
	}
	if notes[0] == "" || notes[1] != "" || notes[2] != "" || !strings.Contains(notes[3], "errors: gap") {
		t.Fatalf("a note for the first truncation, none while queries take turns, one for the gap: %q", notes)
	}
	if notes[4] != "" || !strings.Contains(notes[5], "template list cut") || notes[6] != "" {
		t.Fatalf("under a standing gap: a truncation ending is nothing new, a cut appearing is said, a truncation already said within the interval is not: %q", notes[4:])
	}
}

// TestProdWatch_CutHistoryStillAdvancesTheClock: a template seen only in a
// new query's first window, in a tick whose template list is cut, is cut
// first (history ranks last) — and still moves its incident's clock, so no
// "not observed any more" note follows against a pattern seen minutes ago.
func TestProdWatch_CutHistoryStillAdvancesTheClock(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 60))
	lines := []pwLine{{TS: nsAgo(100 * time.Second), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
	h.lines.Store(lines)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	const known = "ERROR payment gateway refused the card"
	fp := pwTemplateFP(known)
	st := h.state(t)
	inc := incident("loki", "medium", true, 49, 49)
	inc["title_arg"] = known
	st["incidents"] = map[string]any{fp: inc}
	h.setState(t, st)
	h.writeConfig(t, func(cfg map[string]any) {
		lokiOnly(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["queries"].(map[string]any)["zzz_new"] = "new-q"
	})
	word := func(i int) string {
		w := ""
		for j := 0; j < 4; j++ {
			w += string(rune('a' + i%26))
			i /= 26
		}
		return w
	}
	base := nsAgo(20 * time.Second)
	for i := 0; i < 205; i++ {
		for r := 0; r < 2; r++ {
			lines = append(lines, pwLine{TS: base + int64(i*2+r)*int64(time.Millisecond), Line: "ERROR worker " + word(i) + " crashed", Container: "api", Q: "errors-q"})
		}
	}
	for i := 0; i < 3; i++ { // the known template, in the new query's first window only
		lines = append(lines, pwLine{TS: nsAgo(time.Duration(300-i) * time.Second), Line: known, Container: "api", Q: "new-q"})
	}
	h.lines.Store(lines)
	outs := h.cursorTickWith(t, wf, cursorVars(h, 60, 0, 5000), map[string]any{"max_alerts": 1000})
	if outs["leak_scan"]["coverage"] != "partial" {
		t.Fatalf("the list is cut: %v", outs["leak_scan"])
	}
	seen := h.state(t)["incidents"].(map[string]any)[fp].(map[string]any)["last_seen"]
	if at, err := time.Parse(time.RFC3339, fmt.Sprint(seen)); err != nil || time.Since(at) > 10*time.Minute {
		t.Fatalf("the cut history sighting moves the clock: last_seen=%v (%v)", seen, err)
	}
	outs = h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	for _, q := range pwQuietFPs(outs) {
		if q == fp {
			t.Fatalf("no \"not observed any more\" against a pattern seen minutes ago: %v", pwQuietFPs(outs))
		}
	}
}

// TestProdWatch_HistoryClockNeverMovesBack: a history line older than the
// incident's last sighting leaves the clock where it is.
func TestProdWatch_HistoryClockNeverMovesBack(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	inc := incident("loki", "medium", true, 0.05, 0.05)
	last := inc["last_seen"]
	state := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
		"incidents": map[string]any{"loki:t1": inc}}
	sig := map[string]any{"templates": []map[string]any{{"template_id": "t1", "query": "errors", "template": "ERROR job # failed", "count": 2, "count_live": 0,
		"first_ts": strconv.FormatInt(nsAgo(11*time.Minute), 10), "last_ts": strconv.FormatInt(nsAgo(10*time.Minute), 10), "sample": "ERROR job <num> failed", "streams": []string{"container=w"}}},
		"leak": []any{}}
	out, stderr, err := pwDecide(t, wf, h, sig, state, nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	if got := pwStateNext(t, out)["incidents"].(map[string]any)["loki:t1"].(map[string]any)["last_seen"]; got != last {
		t.Fatalf("an older history line must not move the clock back: %v -> %v", last, got)
	}
}

// pwCoverageReasons returns the reasons of the coverage note decide staged
// this tick ("" when there is none).
func pwCoverageReasons(decide map[string]any) string {
	for _, s := range decide["stale_sources"].([]any) {
		if m := s.(map[string]any); m["source"] == "coverage" {
			return fmt.Sprint(m["reasons"])
		}
	}
	return ""
}

// TestProdWatch_ConfigTitleIsTextOrRefusedByName: a Prometheus probe title
// written as a number is text — breached tick after breached tick, each
// tick's plan accepts the state the previous one wrote; a list, an object
// or a boolean is refused by name at the config.
func TestProdWatch_ConfigTitleIsTextOrRefusedByName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	probe := func(title any) func(cfg map[string]any) {
		return func(cfg map[string]any) {
			cfg["prometheus"] = map[string]any{"probes": []map[string]any{
				{"id": "restarts", "title": title, "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"}}}
		}
	}
	for _, title := range []any{5, 99.9, 0} {
		h := newPWHarness(t)
		h.writeConfig(t, probe(title))
		h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
		var posted []string
		for k := 0; k < 3; k++ {
			posted = append(posted, alertsOf(t, h.tick(t, wf, false))...) // a node failure (plan's refusal) fails the test
		}
		if strings.Join(posted, ",") != "prom:new:high" {
			t.Fatalf("title %v: the breach posts once over three breached ticks: %v", title, posted)
		}
		if got := h.state(t)["incidents"].(map[string]any)["prom:restarts"].(map[string]any)["title_arg"]; got != fmt.Sprint(title) {
			t.Fatalf("title %v is carried as text: %#v", title, got)
		}
	}
	for _, title := range []any{[]any{"p99"}, map[string]any{"text": "p99"}, true} {
		h := newPWHarness(t)
		h.writeConfig(t, probe(title))
		_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), map[string]string{"grafana_token": h.tokenFile}))
		if err == nil || strings.Contains(stderr, "Traceback") || !strings.Contains(stderr, "title") {
			t.Fatalf("title %v: plan refuses it by name: %v %s", title, err, stderr)
		}
	}
}

// TestProdWatch_EveryStateTheBotWritesIsAccepted: the whole chain over
// random configs (probe lists or maps, numeric ids and titles, severities
// in any case, a sink channel and username written as text, a number or not
// at all, an icon or none) and random production conditions — Prometheus
// failing in part or entirely, the health URL down, the Grafana token
// refused, blank or unbound, the Grafana host not resolving, lines with and
// without personal data — against a sink that refuses a non-text field like
// Mattermost does, fourteen ticks per seed (PW_CHAIN_SEEDS, default 4): no
// node fails, so each tick's plan accepted the state the previous tick
// wrote, no lane's failure killed the tick and every delivery was consumed.
func TestProdWatch_EveryStateTheBotWritesIsAccepted(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	seeds := 4
	if v, err := strconv.Atoi(os.Getenv("PW_CHAIN_SEEDS")); err == nil && v > 0 {
		seeds = v
	}
	for seed := 0; seed < seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rnd := rand.New(rand.NewSource(int64(seed)*7919 + 3))
			h := newPWHarness(t)
			titles := []any{"Pod restarts", 5, 99.9, nil}
			sevs := []string{"high", "MEDIUM", "Critical", "low"}
			restarts := map[string]any{"query": "restarts-q", "op": ">", "threshold": 0, "severity": sevs[rnd.Intn(4)]}
			if title := titles[rnd.Intn(4)]; title != nil {
				restarts["title"] = title
			}
			latency := map[string]any{"query": "lat-q", "op": ">=", "threshold": 1.5, "severity": sevs[rnd.Intn(4)], "title": titles[rnd.Intn(3)]}
			var probes any = map[string]any{"restarts": restarts, "latency": latency}
			if rnd.Intn(2) == 0 {
				restarts["id"], latency["id"] = "restarts", 7
				probes = []any{restarts, latency}
			}
			health := []map[string]any{{"id": 42, "url": h.srv.URL + "/health", "expect_status": 200, "severity": sevs[rnd.Intn(4)]}}
			sink := map[string]any{"webhook": "w1", "min_severity": sevs[rnd.Intn(4)]}
			for _, field := range []string{"channel", "username"} {
				if v := []any{"ops", 2024, nil}[rnd.Intn(3)]; v != nil {
					sink[field] = v
				}
			}
			if rnd.Intn(2) == 0 {
				sink["icon_emoji"] = ":eye:"
			}
			configure := func(unresolvable bool) {
				h.writeConfig(t, func(cfg map[string]any) {
					cfg["prometheus"] = map[string]any{"probes": probes}
					cfg["probes"] = health
					cfg["sinks"] = []map[string]any{sink}
					if unresolvable {
						cfg["grafana"].(map[string]any)["base_url"] = "http://grafana.invalid"
					}
				})
			}
			words := []string{"alpha", "beta", "gamma", "delta", "omega", "kappa", "sigma", "tau"}
			var lines []pwLine
			for k := 0; k < 14; k++ {
				pm := map[string]pwProm{}
				for _, q := range []string{"restarts-q", "lat-q"} {
					switch rnd.Intn(4) {
					case 0:
						pm[q] = pwProm{Value: strconv.Itoa(rnd.Intn(5))}
					case 1:
						pm[q] = pwProm{NoData: true}
					case 2:
						pm[q] = pwProm{Status: 500}
					default:
						pm[q] = pwProm{Value: "2.5", Warnings: []string{"w" + words[rnd.Intn(len(words))]}}
					}
				}
				h.prom.Store(pm)
				h.healthStatus.Store([]int64{200, 200, 503}[rnd.Intn(3)])
				switch rnd.Intn(8) {
				case 0:
					_ = os.WriteFile(h.tokenFile, []byte("glsa_refused\n"), 0o600)
				case 1:
					_ = os.WriteFile(h.tokenFile, []byte("\n"), 0o600)
				case 2:
					_ = os.Remove(h.tokenFile)
				default:
					if err := os.WriteFile(h.tokenFile, []byte(pwToken+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				configure(rnd.Intn(6) == 0)
				for i, n := 0, rnd.Intn(30); i < n; i++ {
					w := words[rnd.Intn(len(words))]
					line := "ERROR " + w + " failed for job " + strconv.Itoa(rnd.Intn(1000))
					if rnd.Intn(6) == 0 {
						line += " user " + w + "@example.org"
					}
					lines = append(lines, pwLine{TS: time.Now().UnixNano() - int64(rnd.Intn(200))*int64(time.Second), Line: line, Container: "api",
						Q: []string{"errors-q", "sweep-q"}[rnd.Intn(2)]})
				}
				h.lines.Store(append([]pwLine(nil), lines...))
				if outs := h.tick(t, wf, false); outs["notify"]["consume"] != true {
					t.Fatalf("tick %d: a delivered tick is consumed: %v", k, outs["notify"])
				}
			}
		})
	}
}

// TestProdWatch_CutLiveTemplateStillAdvancesTheClock: a template with LIVE
// lines cut from the list (one line, it ranks below 205 templates with two)
// moves its incident's clock like a cut history-only one: the next complete
// tick concludes no "not observed any more" against it, and a one-day
// retention does not forget it — its next sighting is a reminder, not NEW.
func TestProdWatch_CutLiveTemplateStillAdvancesTheClock(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, c := range []struct {
		name       string
		forgetDays int
		lastSeenH  float64
	}{{"quiet after 48 h", 14, 49}, {"forgotten after a day", 1, 25}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, lokiOnly(1000, 60))
			lines := []pwLine{{TS: nsAgo(100 * time.Second), Line: "ERROR warmup", Container: "api", Q: "errors-q"}}
			h.lines.Store(lines)
			h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
			const known = "ERROR payment gateway refused the card"
			fp := pwTemplateFP(known)
			st := h.state(t)
			inc := incident("loki", "medium", true, c.lastSeenH, c.lastSeenH)
			inc["title_arg"] = known
			st["incidents"] = map[string]any{fp: inc}
			h.setState(t, st)
			word := func(i int) string {
				w := ""
				for j := 0; j < 4; j++ {
					w += string(rune('a' + i%26))
					i /= 26
				}
				return w
			}
			base := nsAgo(20 * time.Second)
			for i := 0; i < 205; i++ {
				for r := 0; r < 2; r++ {
					lines = append(lines, pwLine{TS: base + int64(i*2+r)*int64(time.Millisecond), Line: "ERROR worker " + word(i) + " crashed", Container: "api", Q: "errors-q"})
				}
			}
			lines = append(lines, pwLine{TS: nsAgo(5 * time.Second), Line: known, Container: "api", Q: "errors-q"})
			h.lines.Store(lines)
			outs := h.cursorTickWith(t, wf, cursorVars(h, 60, 0, 5000), map[string]any{"max_alerts": 1000, "forget_after_days": c.forgetDays})
			if outs["leak_scan"]["coverage"] != "partial" {
				t.Fatalf("the list is cut: %v", outs["leak_scan"])
			}
			rec, ok := h.state(t)["incidents"].(map[string]any)[fp].(map[string]any)
			if !ok {
				t.Fatalf("a pattern seen live this tick is not forgotten")
			}
			if at, err := time.Parse(time.RFC3339, fmt.Sprint(rec["last_seen"])); err != nil || time.Since(at) > 10*time.Minute {
				t.Fatalf("the cut live sighting moves the clock: last_seen=%v (%v)", rec["last_seen"], err)
			}
			if c.forgetDays == 1 {
				lines = append(lines, pwLine{TS: nsAgo(time.Second), Line: known, Container: "api", Q: "errors-q"})
				h.lines.Store(lines)
			}
			outs = h.cursorTickWith(t, wf, cursorVars(h, 60, 0, 5000), map[string]any{"forget_after_days": c.forgetDays})
			for _, q := range pwQuietFPs(outs) {
				if q == fp {
					t.Fatalf("no \"not observed any more\" against a pattern seen live one tick ago: %v", pwQuietFPs(outs))
				}
			}
			for _, a := range outs["decide"]["alerts"].([]any) {
				if m := a.(map[string]any); m["fingerprint"] == fp && m["state"] == "new" {
					t.Fatalf("a known pattern is never re-posted as NEW: %v", m)
				}
			}
		})
	}
}

// TestProdWatch_CutClockIsScopedAndNeverMovesBack: a cut template's newest
// line moves ITS incident's clock only, and never back.
func TestProdWatch_CutClockIsScopedAndNeverMovesBack(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	fresh := incident("loki", "medium", true, 0.05, 0.05)
	stale := incident("loki", "medium", true, 49, 49)
	other := incident("loki", "medium", true, 49, 49)
	state := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
		"incidents": map[string]any{"loki:t1": fresh, "loki:t2": stale, "loki:t3": other}}
	sig := map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial", "templates_cut": true, "templates_total": 202,
		"cut_last_ts": map[string]any{"t1": strconv.FormatInt(nsAgo(10*time.Minute), 10), "t2": strconv.FormatInt(nsAgo(5*time.Minute), 10)}}
	out, stderr, err := pwDecide(t, wf, h, sig, state, nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	inc := pwStateNext(t, out)["incidents"].(map[string]any)
	if got := inc["loki:t1"].(map[string]any)["last_seen"]; got != fresh["last_seen"] {
		t.Fatalf("a cut line older than the last sighting leaves the clock where it is: %v -> %v", fresh["last_seen"], got)
	}
	if at, err := time.Parse(time.RFC3339, fmt.Sprint(inc["loki:t2"].(map[string]any)["last_seen"])); err != nil ||
		time.Since(at) > 6*time.Minute || time.Since(at) < 4*time.Minute {
		t.Fatalf("the cut line moves its incident's clock to it: %v (%v)", inc["loki:t2"].(map[string]any)["last_seen"], err)
	}
	if got := inc["loki:t3"].(map[string]any)["last_seen"]; got != other["last_seen"] {
		t.Fatalf("a cut line moves no other incident's clock: %v -> %v", other["last_seen"], got)
	}
}

// TestProdWatch_PrometheusOutageStillDeliversTheHealthProbe: production is
// down (the health URL answers 503) while every Prometheus probe fails: the
// tick goes on, posts the probe's critical alert, and its coverage note
// names the probe that failed.
func TestProdWatch_PrometheusOutageStillDeliversTheHealthProbe(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.healthStatus.Store(503)
	h.prom.Store(map[string]pwProm{"restarts-q": {Status: 500}})
	outs := h.tick(t, wf, false)
	if got := strings.Join(alertsOf(t, outs), ","); !strings.Contains(got, "probe:new:critical") {
		t.Fatalf("the health probe's alert is posted: %v", got)
	}
	if r := pwCoverageReasons(outs["decide"]); !strings.Contains(r, "restarts: ") {
		t.Fatalf("the coverage note names the failed probe: %q", r)
	}
	b, err := os.ReadFile(outs["decide"]["tick_file"].(string))
	if err != nil || !strings.Contains(string(b), `"coverage": "partial"`) {
		t.Fatalf("the tick record says the coverage was partial: %s %v", b, err)
	}
}

// TestProdWatch_PrometheusLaneReportsWhatAnswered: on a Prometheus-only
// config, one probe failing does not refuse the tick — the probe that
// answered posts its breach; every probe failing does, naming why.
func TestProdWatch_PrometheusLaneReportsWhatAnswered(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"].(map[string]any)["queries"] = map[string]any{}
		cfg["probes"] = []map[string]any{}
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{
			{"id": "restarts", "title": "restarts", "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"},
			{"id": "latency", "title": "latency", "query": "lat-q", "op": ">", "threshold": 1, "severity": "medium"}}}
	})
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}, "lat-q": {Status: 500}})
	if got := strings.Join(alertsOf(t, h.tick(t, wf, false)), ","); got != "prom:new:high" {
		t.Fatalf("the probe that answered posts its breach: %v", got)
	}
	// "no data" is an answer: every probe answering it reports the tick.
	h.prom.Store(map[string]pwProm{"restarts-q": {NoData: true}, "lat-q": {NoData: true}})
	if got := strings.Join(alertsOf(t, h.tick(t, wf, false)), ","); got != "prom_no_data:new:medium,prom_no_data:new:medium" {
		t.Fatalf("probes answering no data report the tick: %v", got)
	}
	failed := func(id string) map[string]any {
		return map[string]any{"id": id, "title": id, "state": "error", "value": nil, "error": "HTTPError: HTTP Error 500: Internal Server Error"}
	}
	fresh := newPWHarness(t)
	_, stderr, err := pwDecide(t, wf, fresh, map[string]any{"templates": []any{}, "leak": []any{}}, nil, map[string]any{
		"prom_results": []any{failed("restarts"), failed("latency")}, "prom_ok": false,
		"prom_errors": []any{map[string]any{"probe": "restarts", "error": "HTTPError: HTTP Error 500: Internal Server Error"},
			map[string]any{"probe": "latency", "error": "HTTPError: HTTP Error 500: Internal Server Error"}},
		"loki_per_query": map[string]any{}, "lanes": map[string]any{"loki": false, "prometheus": true, "probes": false}})
	if err == nil || !strings.Contains(stderr, "every configured lane failed") || !strings.Contains(stderr, "HTTP Error 500") {
		t.Fatalf("every probe failing refuses the tick, naming why: %v %s", err, stderr)
	}
}

// TestProdWatch_FailedProbeConcludesNothingAboutItsIncident: a probe that
// errors while another answers keeps the tick reported — and concludes
// nothing about the incidents of its lane: no "not observed any more" for
// a breach the lane could not look at; the lane's health is not refreshed
// and carries the error.
func TestProdWatch_FailedProbeConcludesNothingAboutItsIncident(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	lastOK := hoursAgo(1)
	state := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}},
		"health":    map[string]any{"prometheus": map[string]any{"last_ok": lastOK}},
		"incidents": map[string]any{"prom:restarts": incident("prom", "high", true, 49, 49)}}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, state, map[string]any{
		"prom_results": []any{map[string]any{"id": "restarts", "title": "restarts", "state": "error", "error": "HTTPError: HTTP Error 500"},
			map[string]any{"id": "latency", "title": "latency", "state": "healthy", "value": 0.2, "op": ">", "threshold": 1.0}},
		"prom_ok": false, "prom_errors": []any{map[string]any{"probe": "restarts", "error": "HTTPError: HTTP Error 500"}},
		"loki_per_query": map[string]any{}, "lanes": map[string]any{"loki": false, "prometheus": true, "probes": false}})
	if err != nil {
		t.Fatalf("a probe that answered keeps the tick reported: %v %s", err, stderr)
	}
	for _, a := range out["alerts"].([]any) {
		if m := a.(map[string]any); m["fingerprint"] == "prom:restarts" {
			t.Fatalf("nothing is concluded about a probe that did not answer: %v", m)
		}
	}
	hp := pwStateNext(t, out)["health"].(map[string]any)["prometheus"].(map[string]any)
	if hp["last_ok"] != lastOK || !strings.Contains(fmt.Sprint(hp["last_error"]), "restarts") {
		t.Fatalf("a lane with a failing probe keeps its last OK and records the error: %v", hp)
	}
}

// TestProdWatch_RefusedTokenIsALaneErrorNotADeadTick: Grafana refusing the
// token (an expired service account) fails the Loki and Prometheus lanes
// but not the tick: the health probe still posts and the coverage note
// names the refusal; with no other lane, the refusal of the tick names it.
func TestProdWatch_RefusedTokenIsALaneErrorNotADeadTick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.healthStatus.Store(503)
	const rawEmail = "marie.curie@example.org"
	h.lines.Store([]pwLine{{TS: nsAgo(90 * time.Second), Line: "ERROR login failed for " + rawEmail, Container: "api", Q: "errors-q"}})
	if err := os.WriteFile(h.tokenFile, []byte("glsa_refused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outs := h.tick(t, wf, false)
	if got := strings.Join(alertsOf(t, outs), ","); !strings.Contains(got, "probe:new:critical") {
		t.Fatalf("the health probe's alert is posted: %v", got)
	}
	if r := pwCoverageReasons(outs["decide"]); !strings.Contains(r, "errors: CredentialRefused: Grafana refused the token (HTTP 401)") ||
		!strings.Contains(r, "restarts: CredentialRefused") || !strings.Contains(r, "token expired") {
		t.Fatalf("the coverage note names the refusal on each lane, with its remedy: %q", r)
	}
	// The cursors did not move: once the token is fixed, the line written
	// during the refusal is read.
	if err := os.WriteFile(h.tokenFile, []byte(pwToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(alertsOf(t, h.tick(t, wf, false)), ","); !strings.Contains(got, "leak:new") {
		t.Fatalf("the line written while the token was refused is read once it is fixed: %v", got)
	}
	refused := "CredentialRefused: Grafana refused the token (HTTP 401) -- the grafana_token service account lacks datasource query rights, or the token expired"
	fresh := newPWHarness(t)
	_, stderr, err := pwDecide(t, wf, fresh, map[string]any{"templates": []any{}, "leak": []any{}}, nil, map[string]any{
		"loki_ok": false, "loki_errors": []any{map[string]any{"query": "errors", "error": refused}},
		"loki_per_query": map[string]any{"errors": map[string]any{"lines": 0, "error": refused, "truncated": false, "gap": false, "from_ns": "100", "to_ns": "900"}},
		"prom_results":   []any{map[string]any{"id": "restarts", "state": "error", "error": refused}}, "prom_ok": false,
		"prom_errors": []any{map[string]any{"probe": "restarts", "error": refused}},
		"lanes":       map[string]any{"loki": true, "prometheus": true, "probes": false}})
	if err == nil || !strings.Contains(stderr, "every configured lane failed") || !strings.Contains(stderr, "refused the token") {
		t.Fatalf("with no lane left the tick is refused, naming the token: %v %s", err, stderr)
	}
}

// TestProdWatch_CoverageNoteIgnoresNumbersInLongErrors: lane errors long
// enough to be cut — five whose join passes the note's budget, or one past
// the per-error cut — differing tick to tick only in a number's digit
// count, are one kind: the note posts once.
func TestProdWatch_CoverageNoteIgnoresNumbersInLongErrors(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, c := range []struct {
		name   string
		probes int
		tail   string
	}{{"five errors past the join budget", 5, ""}, {"one error past the per-error cut", 1, strings.Repeat(" (upstream reset)", 8)}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
			var notes []bool
			for _, n := range []int{987, 1234, 987, 56789} {
				var perr []any
				for i := 0; i < c.probes; i++ {
					read := 4000
					if i == 0 {
						read = n
					}
					perr = append(perr, map[string]any{"probe": fmt.Sprintf("p%d", i), "error": fmt.Sprintf("IncompleteRead: IncompleteRead(%d bytes read, 5 more expected) "+
						"while reading the Prometheus answer for the dashboard panel latency_p99 of service api", read) + c.tail})
				}
				out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "full"}, state, map[string]any{
					"prom_ok": false, "prom_errors": perr, "loki_per_query": map[string]any{}, "lanes": map[string]any{"loki": false, "prometheus": true, "probes": true}})
				if err != nil {
					t.Fatalf("decide: %v %s", err, stderr)
				}
				notes = append(notes, pwCoverageReasons(out) != "")
				state = pwStateNext(t, out)
			}
			if fmt.Sprint(notes) != "[true false false false]" {
				t.Fatalf("one note for one kind of error, whatever its numbers: %v", notes)
			}
		})
	}
}

// TestProdWatch_NotifyRendersAnyFieldName: an incident's fields are data —
// fields named like the renderer's own parameters render as text.
func TestProdWatch_NotifyRendersAnyFieldName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	alerts := []map[string]any{{"fingerprint": "loki:t", "kind": "loki", "severity": "low", "state": "quiet", "title_key": "loki_template",
		"title_arg": "ERROR x", "detail_key": "loki_detail", "fields": map[string]any{"key": "k", "label": "l", "count": 2, "first": "2026-09-23T10:00", "streams": "api"},
		"evidence": map[string]any{}, "count": 2, "first_seen": "2026-09-23T10:00:00+00:00"}}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": alerts, "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{"loki_template": "pattern", "loki_detail": "{count} line(s) since {first}, containers: {streams}"},
		"app":    map[string]any{"name": "demo"}, "release": "", "release_known": false, "dry_run": true, "max_message_chars": 14000},
		nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, stderr)
	}
	if text := fmt.Sprint(out["messages"]); !strings.Contains(text, "2 line(s) since") {
		t.Fatalf("the detail renders: %s", text)
	}
}

// TestProdWatch_ForeignValuesNoConsumerReadsRunATick: values plan does not
// type — no node needs their type — run whole ticks through notify and
// commit_state: a number where the bot writes text, strings where it writes
// booleans, dates that are not dates on a health record and a cursor's
// clock, coverage bookkeeping of another shape. The foreign incident
// reaches the quiet note, the foreign health record the stale note.
func TestProdWatch_ForeignValuesNoConsumerReadsRunATick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}})
	h.tick(t, wf, false)
	st := h.state(t)
	inc := incident("loki", "medium", true, 49, 49)
	for k, v := range map[string]any{"title_key": "loki_template", "detail_key": "loki_detail", "sources": []any{"errors"}, "title_arg": 5, "alerted": "yes",
		"quiet_noted": 0, "last_notified": "", "fp": 7, "prev_severity": []any{1}} {
		inc[k] = v
	}
	st["incidents"] = map[string]any{"loki:foreign": inc}
	st["health"] = map[string]any{"prometheus": map[string]any{"last_ok": "x", "last_warned": 3, "last_error": []any{1}}}
	st["coverage_posted"] = map[string]any{"a kind": 5}
	st["last_coverage"], st["last_coverage_key"] = 5, []any{}
	cur := st["cursors"].(map[string]any)["loki"].(map[string]any)
	for q := range cur {
		cur[q].(map[string]any)["at"] = 123
	}
	h.setState(t, st)
	h.prom.Store(map[string]pwProm{"restarts-q": {Status: 500}}) // the lane fails: its health record is not refreshed
	outs := h.tick(t, wf, false)
	if !strings.Contains(strings.Join(pwQuietFPs(outs), ","), "loki:foreign") {
		t.Fatalf("the foreign incident reaches the quiet note: %v", pwQuietFPs(outs))
	}
	stale := false
	for _, s := range outs["decide"]["stale_sources"].([]any) {
		if s.(map[string]any)["source"] == "prometheus" {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("the foreign health record reaches the stale note: %v", outs["decide"]["stale_sources"])
	}
}

// TestProdWatch_FarAheadCursorIsReportedNotClamped: a cursor more than a
// full window ahead of the window's end (a clock or an edited state) is an
// inverted window poll_loki reports — never clamped into an empty window
// that would park the query for good behind a full coverage.
func TestProdWatch_FarAheadCursorIsReportedNotClamped(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiOnly(1000, 60))
	ahead := strconv.FormatInt(time.Now().Add(2*time.Hour).UnixNano(), 10)
	h.setState(t, map[string]any{"version": 1, "generation": 1, "incidents": map[string]any{}, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": ahead, "frontier_ns": ahead, "band": []any{},
			"band_base_ns": ahead, "overlap_from_ns": ahead}}}})
	secrets := map[string]string{"grafana_token": h.tokenFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), secrets))
	if err != nil {
		t.Fatalf("plan: %v %s", err, stderr)
	}
	loki, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{"grafana": plan["grafana"], "loki": plan["loki"],
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki: %v %s", err, stderr)
	}
	if e := fmt.Sprint(loki["per_query"].(map[string]any)["errors"].(map[string]any)["error"]); !strings.HasPrefix(e, "inverted window") {
		t.Fatalf("a cursor two hours ahead is reported as an inverted window: %q", e)
	}
}

// TestProdWatch_SinkNamesAreText: a sink's channel or username written as a
// number is its name — against a sink that refuses a non-text field like
// Mattermost does, the alert is delivered and the tick consumed once, never
// re-posted every tick; a sink with no channel sends none; a webhook name,
// a channel, a username or an icon of another type is refused by name at
// the config.
func TestProdWatch_SinkNamesAreText(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["sinks"] = []map[string]any{{"webhook": "w1", "channel": 2024, "username": 5, "icon_emoji": ":warning:", "min_severity": "low"},
			{"webhook": "w1", "min_severity": "low"}}
	})
	h.healthStatus.Store(503)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}})
	var posted []string
	for k := 0; k < 3; k++ {
		outs := h.tick(t, wf, false)
		if outs["notify"]["consume"] != true {
			t.Fatalf("tick %d: a delivered tick is consumed: %v", k, outs["notify"])
		}
		if k == 0 {
			channels := map[any]bool{}
			for _, m := range outs["notify"]["messages"].([]any) {
				channels[m.(map[string]any)["channel"]] = true
			}
			if !channels["2024"] || !channels[""] || len(channels) != 2 {
				t.Fatalf("the channel travels as its name, and a sink without one sends none: %v", channels)
			}
		}
		posted = append(posted, alertsOf(t, outs)...)
	}
	if strings.Join(posted, ",") != "probe:new:critical" {
		t.Fatalf("the alert posts once over three ticks: %v", posted)
	}
	for _, sink := range []map[string]any{{"webhook": []any{"w1"}}, {"webhook": "w1", "channel": map[string]any{"name": "ops"}}, {"webhook": "w1", "channel": true},
		{"webhook": "w1", "username": []any{"Argus"}}, {"webhook": "w1", "icon_emoji": 5}, {"webhook": "w1", "icon_emoji": true}} {
		bad := newPWHarness(t)
		bad.writeConfig(t, func(cfg map[string]any) { cfg["sinks"] = []map[string]any{sink} })
		_, stderr, err := runPyWhole(t, bad.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(bad, 60, 0, 5000), map[string]string{"grafana_token": bad.tokenFile}))
		if err == nil || strings.Contains(stderr, "Traceback") || !strings.Contains(stderr, "sink") {
			t.Fatalf("sink %v: plan refuses it by name: %v %s", sink, err, stderr)
		}
	}
}

// TestProdWatch_NoLineCrashesTheScan: each redaction class's shape with a
// hostile character inserted at, or substituted for, every position — the
// non-ASCII letters Python folds into [A-Z] under IGNORECASE (İ ı ſ K),
// Unicode digits NFKC keeps, a combining mark, invisible and bidi controls,
// exotic spaces, an emoji — and an ordinary Turkish line: the scan reads
// them all and exits cleanly, so no log line can blind a tick; an IBAN
// glued to a non-ASCII letter is still one.
func TestProdWatch_NoLineCrashesTheScan(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	shapes := []string{"FR76 3000 6000 0112 3456 7890 189", "fr7630006000011234567890189", "GB82 WEST 1234 5698 7654 32", "1 85 03 75 123 456 41",
		"4111 1111 1111 1111", "4012888888881881", "jean.dupont@example.org", "06 12 34 56 78", "+33 6 12 34 56 78",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.abcdefghijk", "Bearer abcdefghijklmnopqrstuvwxyz", "password=hunter2hunter2"}
	hostile := []string{"İ", "ı", "ſ", "\u212a", "٣", "३", "１", "²", "\u0307", "\u200d", "\u200f", "\u00a0", "\u3000", "\u00ad", "🙂", "𝚤"}
	lines := []string{"ERROR SİPARİŞ İD12345678901234 başarısız"}
	for _, s := range shapes {
		r := []rune(s)
		for _, x := range hostile {
			for i := 0; i <= len(r); i++ {
				lines = append(lines, "ERROR account "+string(r[:i])+x+string(r[i:])+" refused")
				if i < len(r) {
					lines = append(lines, "ERROR account "+string(r[:i])+x+string(r[i+1:])+" refused")
				}
			}
		}
	}
	scan := func(name string, lines []string) (map[string]any, string, error) {
		raw := filepath.Join(h.scratch, name+".jsonl")
		var buf strings.Builder
		base := nsAgo(time.Minute)
		for i, l := range lines {
			b, _ := json.Marshal(map[string]any{"q": "errors", "ts": strconv.FormatInt(base+int64(i), 10), "line": l, "stream": map[string]string{"container": "api"}})
			buf.Write(b)
			buf.WriteString("\n")
		}
		if err := os.WriteFile(raw, []byte(buf.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
			"raw_file": raw, "per_query": map[string]any{"errors": map[string]any{}}, "app": map[string]any{"name": "demo"},
			"scratch_dir": filepath.Join(h.scratch, name)}, nil, nil))
	}
	out, stderr, err := scan("hostile", lines)
	if err != nil || strings.Contains(stderr, "Traceback") {
		t.Fatalf("no line crashes the scan (%d lines): %v\n%s", len(lines), err, stderr)
	}
	if out["lines_scanned"] != float64(len(lines)) {
		t.Fatalf("every line is scanned: %v of %d", out["lines_scanned"], len(lines))
	}
	out, stderr, err = scan("glued", []string{"ERROR compte éFR76 3000 6000 0112 3456 7890 189 refusé"})
	if err != nil || !strings.Contains(fmt.Sprint(out["leak_findings"]), "class:iban") {
		t.Fatalf("an IBAN glued to a non-ASCII letter is still an IBAN: %v %v %s", out["leak_findings"], err, stderr)
	}
}

// TestProdWatch_GrafanaSetupFailureIsALaneError: every way the Grafana
// lanes cannot start — a host that does not resolve, a token file holding
// only a newline, a token secret not bound at all — fails those lanes and
// not the tick: production down still posts the health probe's critical,
// and the coverage note names the cause on each Grafana lane. A Loki-only
// config reports the cause on every query (decide then refuses the tick,
// naming it).
func TestProdWatch_GrafanaSetupFailureIsALaneError(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, c := range []struct {
		name, cause string
		setup       func(t *testing.T, h *pwHarness)
	}{
		{"host does not resolve", "grafana.invalid", func(t *testing.T, h *pwHarness) {
			h.writeConfig(t, func(cfg map[string]any) { cfg["grafana"].(map[string]any)["base_url"] = "http://grafana.invalid" })
		}},
		{"token file holds a newline", "not bound or empty", func(t *testing.T, h *pwHarness) {
			if err := os.WriteFile(h.tokenFile, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"token secret not bound", "not bound or empty", func(t *testing.T, h *pwHarness) {
			if err := os.Remove(h.tokenFile); err != nil {
				t.Fatal(err)
			}
		}},
		{"token path unreadable", "Is a directory", func(t *testing.T, h *pwHarness) {
			if err := os.Remove(h.tokenFile); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(h.tokenFile, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.healthStatus.Store(503)
			c.setup(t, h)
			outs := h.tick(t, wf, false)
			if got := strings.Join(alertsOf(t, outs), ","); !strings.Contains(got, "probe:new:critical") {
				t.Fatalf("the health probe's alert is posted: %v", got)
			}
			if r := pwCoverageReasons(outs["decide"]); !strings.Contains(r, "errors: ") || !strings.Contains(r, "restarts: ") || !strings.Contains(r, c.cause) {
				t.Fatalf("the coverage note names the cause on each Grafana lane: %q", r)
			}
			for q, info := range outs["poll_loki"]["per_query"].(map[string]any) {
				if e := fmt.Sprint(info.(map[string]any)["error"]); !strings.Contains(e, c.cause) {
					t.Fatalf("Loki query %s reports the cause: %q", q, e)
				}
			}
			for _, e := range outs["poll_prom"]["errors"].([]any) {
				if m := e.(map[string]any); !strings.Contains(fmt.Sprint(m["error"]), c.cause) {
					t.Fatalf("Prometheus probe %v reports the cause, before any request: %q", m["probe"], m["error"])
				}
			}
			lokiOnlyH := newPWHarness(t)
			lokiOnlyH.writeConfig(t, lokiTwoQueries(1000, 60))
			c.setup(t, lokiOnlyH)
			if c.name == "host does not resolve" {
				lokiOnlyH.writeConfig(t, func(cfg map[string]any) {
					lokiTwoQueries(1000, 60)(cfg)
					cfg["grafana"].(map[string]any)["base_url"] = "http://grafana.invalid"
				})
			}
			secrets := map[string]string{"grafana_token": lokiOnlyH.tokenFile}
			plan, stderr, err := runPyWhole(t, lokiOnlyH.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(lokiOnlyH, 60, 0, 5000), secrets))
			if err != nil {
				t.Fatalf("plan refuses nothing here (the lanes report it): %v %s", err, stderr)
			}
			loki, stderr, err := runPyWhole(t, lokiOnlyH.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{"grafana": plan["grafana"], "loki": plan["loki"],
				"timeout_secs": 5, "scratch_dir": lokiOnlyH.scratch, "allow_private": true}, nil, secrets))
			if err != nil || loki["ok"] != false {
				t.Fatalf("poll_loki reports the cause, it does not die: %v %v %s", err, loki, stderr)
			}
			for q, info := range loki["per_query"].(map[string]any) {
				if e := fmt.Sprint(info.(map[string]any)["error"]); !strings.Contains(e, c.cause) {
					t.Fatalf("query %s names the cause: %q", q, e)
				}
			}
		})
	}
}

// TestProdWatch_CoverageNoteSaysEachKindOncePerInterval: sources failing in
// turn with the same error, or a lane flapping between failure and health,
// are one kind of partiality: the note posts once, and the same kind posts
// again only after renotify_hours.
func TestProdWatch_CoverageNoteSaysEachKindOncePerInterval(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	failing := func(ids ...string) []any {
		e := []any{}
		for _, id := range ids {
			e = append(e, map[string]any{"probe": id, "error": "HTTPError: HTTP Error 500: Internal Server Error"})
		}
		return e
	}
	tick := func(errs []any) bool {
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "full"}, state, map[string]any{
			"prom_ok": len(errs) == 0, "prom_errors": errs, "loki_per_query": map[string]any{}, "lanes": map[string]any{"loki": false, "prometheus": true, "probes": true}})
		if err != nil {
			t.Fatalf("decide: %v %s", err, stderr)
		}
		state = pwStateNext(t, out)
		return pwCoverageReasons(out) != ""
	}
	var notes []bool
	for _, errs := range [][]any{failing("restarts"), failing("latency"), failing("restarts"), nil, failing("latency"), nil, failing("restarts", "latency")} {
		notes = append(notes, tick(errs))
	}
	if fmt.Sprint(notes) != "[true false false false false false false]" {
		t.Fatalf("probes failing in turn, and a flapping lane, are one kind — one note: %v", notes)
	}
	posted := state["coverage_posted"].(map[string]any)
	for k := range posted {
		posted[k] = hoursAgo(25)
	}
	if !tick(failing("restarts")) {
		t.Fatalf("the same kind posts again after renotify_hours: %v", state["coverage_posted"])
	}
}

// TestProdWatch_RemovedSourceConcludesNothing: an incident whose own source
// left the config — a Prometheus probe, a health probe, the query a
// template was last counted by — is not concluded "not observed any more":
// nothing looks at it any more, retention forgets it. Incidents whose
// source is configured and observed still get their note.
func TestProdWatch_RemovedSourceConcludesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	inc := func(kind, source string) map[string]any {
		r := incident(kind, "high", true, 49, 49)
		r["sources"] = []any{source}
		return r
	}
	state := map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
		"incidents": map[string]any{"prom:gone": inc("prom", "gone"), "prom:live": inc("prom", "live"), "probe:gone-api": inc("probe", "gone-api"),
			"probe:api": inc("probe", "api"), "loki:tgone": inc("loki", "removed-q"), "loki:tlive": inc("loki", "errors")}}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, state, map[string]any{
		"prom_results": []any{map[string]any{"id": "live", "title": "live", "state": "healthy", "value": 0.1, "op": ">", "threshold": 1.0}},
		"http_results": []any{map[string]any{"id": "api", "url": "https://app.example/health", "ok": true, "status": 200}}})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	quiet := map[string]bool{}
	for _, a := range out["alerts"].([]any) {
		if m := a.(map[string]any); m["state"] == "quiet" {
			quiet[fmt.Sprint(m["fingerprint"])] = true
		}
	}
	if !quiet["prom:live"] || !quiet["probe:api"] || !quiet["loki:tlive"] {
		t.Fatalf("an incident whose source is configured and observed gets its note: %v", quiet)
	}
	if quiet["prom:gone"] || quiet["probe:gone-api"] || quiet["loki:tgone"] {
		t.Fatalf("nothing is concluded about an incident whose source left the config: %v", quiet)
	}
}

// TestProdWatch_PrometheusErrorBodyStaysOut: a backend answering a failed
// query with HTTP 200 and an error text that quotes a label value (an email
// here) — the text reaches neither the coverage note, the state nor the
// lane's output: the lane error names the status and the error type only.
func TestProdWatch_PrometheusErrorBodyStaysOut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.prom.Store(map[string]pwProm{"restarts-q": {Body: map[string]any{"status": "error", "errorType": "bad_data",
		"error": `found duplicate series for the match group {user="jean.dupont@example.org"}`}}})
	outs := h.tick(t, wf, false)
	r := pwCoverageReasons(outs["decide"])
	if !strings.Contains(r, "bad_data") || strings.Contains(r, "jean.dupont") {
		t.Fatalf("the note names the error type, never the body: %q", r)
	}
	if st, _ := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "state.json")); strings.Contains(string(st), "jean.dupont") {
		t.Fatalf("the state never holds the body: %s", st)
	}
	if strings.Contains(fmt.Sprint(outs["poll_prom"]), "jean.dupont") {
		t.Fatalf("the lane's output never holds the body: %v", outs["poll_prom"])
	}
}

// TestProdWatch_RefusalHorizonIsTheMaxWindowMinusTheOverlap: after a good
// tick, a refused token leaves the Loki cursor where the next window opens
// (the mark minus the overlap); once the token is back, a refusal shorter
// than max_window_minutes minus overlap_seconds skipped nothing, a longer
// one leaves a declared gap. Elapsed time is simulated with the ingest lag.
func TestProdWatch_RefusalHorizonIsTheMaxWindowMinusTheOverlap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, c := range []struct {
		elapsed int
		gap     bool
	}{{200, false}, {290, true}} { // max window 300 s, overlap 60 s: the horizon is 240 s
		c := c
		t.Run(fmt.Sprintf("refused for %ds", c.elapsed), func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, lokiTwoQueries(1000, 60))
			h.lines.Store([]pwLine{{TS: nsAgo(time.Duration(c.elapsed+30) * time.Second), Line: "ERROR warmup", Container: "api", Q: "errors-q"}})
			h.cursorTick(t, wf, cursorVars(h, 5, c.elapsed, 5000))
			if err := os.WriteFile(h.tokenFile, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// A health lane is configured: the tick is reported, the Loki lane failed.
			refused := h.cursorTickWith(t, wf, cursorVars(h, 5, c.elapsed, 5000), map[string]any{"lanes": map[string]any{"loki": true, "prometheus": false, "probes": true}})
			if e := fmt.Sprint(refused["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)["error"]); !strings.Contains(e, "not bound or empty") {
				t.Fatalf("the refused tick reports the token: %q", e)
			}
			if err := os.WriteFile(h.tokenFile, []byte(pwToken+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			back := h.cursorTick(t, wf, cursorVars(h, 5, 0, 5000))
			if got := back["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)["gap"]; got != c.gap {
				t.Fatalf("after %ds of refusal (horizon 240s) gap=%v, want %v", c.elapsed, got, c.gap)
			}
		})
	}
}

// TestProdWatch_AnIncidentTheBotOpenedIsQuietedByItsSource: the normal
// path — incidents the bot opened itself (a health probe down, a metric
// breached, a new error pattern) carry their source, so once each source
// answers healthy and quiet_after_hours pass, each gets its "not observed
// any more" note.
func TestProdWatch_AnIncidentTheBotOpenedIsQuietedByItsSource(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.healthStatus.Store(503)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	h.tick(t, wf, false) // bootstrap: the probe down and the breach are posted
	h.lines.Store([]pwLine{{TS: time.Now().UnixNano(), Line: "ERROR payment gateway refused the card", Container: "api", Q: "errors-q"}})
	if got := strings.Join(alertsOf(t, h.tick(t, wf, false)), ","); !strings.Contains(got, "loki:new") {
		t.Fatalf("the new error pattern is posted: %v", got)
	}
	h.healthStatus.Store(200)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}})
	st := h.state(t)
	for fp, rec := range st["incidents"].(map[string]any) {
		r := rec.(map[string]any)
		if src, _ := r["sources"].([]any); len(src) == 0 {
			t.Fatalf("incident %s carries its sources: %v", fp, r)
		}
		r["last_seen"] = hoursAgo(49)
	}
	h.setState(t, st)
	quiet := strings.Join(pwQuietFPs(h.tick(t, wf, false)), ",")
	for _, fp := range []string{"probe:api", "prom:restarts", pwTemplateFP("ERROR payment gateway refused the card")} {
		if !strings.Contains(quiet, fp) {
			t.Fatalf("%s gets its note once its source answers and quiet_after_hours passed: %v", fp, quiet)
		}
	}
}

// pwEverything is every byte a tick hands on: each node's stdout and
// stderr, the messages delivered, the state and the ledgers.
func pwEverything(t *testing.T, h *pwHarness, outs map[string]map[string]any) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprint(&b, outs)
	fmt.Fprint(&b, h.stderrs)
	b.WriteString(strings.Join(h.bodies(), "\n"))
	entries, _ := os.ReadDir(filepath.Join(h.ws, ".prod-watch"))
	for _, e := range entries {
		if c, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", e.Name())); err == nil {
			b.Write(c)
		}
	}
	return b.String()
}

// TestProdWatch_NoRawValueEscapesWhateverSurroundsIt: every redaction
// class's raw value glued on either side to a letter of another script, an
// accented letter, punctuation, an emoji or an invisible character never
// reaches what the scan hands on (signals.json, its stdout); an IBAN
// followed by an ordinary word is found and redacted. Glue of ASCII
// letters, digits or `_` is the documented limit: an identifier-shaped run
// is not split.
func TestProdWatch_NoRawValueEscapesWhateverSurroundsIt(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	values := []struct{ phrase, secret string }{ // what the log carries, what must never come out
		{"jean.dupont@example.org", "jean.dupont@example.org"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N", "eyJzdWIiOiIxMjM0NTY3ODkwIn0"},
		{"AKIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOP"},
		{"ghp_abcdefghijklmnopqrstuvwxyz0123", "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		{"Bearer abcdefghijklmnopqrstuvwxyz", "abcdefghijklmnopqrstuvwxyz"},
		{"password=hunter2hunter2", "hunter2hunter2"},
		{"FR7630006000011234567890189", "30006000011234567890189"},
		{"4111111111111111", "4111111111111111"},
		{"0612345678", "0612345678"},
	}
	glue := []string{"登录", "é", "д", "λ", "ع", "٣", ".", ",", ";", ":", "!", "?", "(", ")", "[", "]", "\"", "'", "`", "🙂", "\u200b", "\u200d", "\u00a0", "\u3000"}
	scan := func(name string, lines []string) (map[string]any, string) {
		raw := filepath.Join(h.scratch, name+".jsonl")
		var buf strings.Builder
		base := nsAgo(time.Minute)
		for i, l := range lines {
			b, _ := json.Marshal(map[string]any{"q": "errors", "ts": strconv.FormatInt(base+int64(i), 10), "line": l, "stream": map[string]string{"container": "api"}})
			buf.Write(b)
			buf.WriteString("\n")
		}
		if err := os.WriteFile(raw, []byte(buf.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(h.scratch, name)
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
			"raw_file": raw, "per_query": map[string]any{"errors": map[string]any{}}, "app": map[string]any{"name": "demo"}, "scratch_dir": dir}, nil, nil))
		if err != nil {
			t.Fatalf("leak_scan %s: %v %s", name, err, stderr)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "signals.json"))
		return out, fmt.Sprint(out) + stderr + string(b)
	}
	for _, v := range values {
		var lines []string
		for _, g := range glue {
			lines = append(lines, "ERROR "+g+v.phrase+" x", "ERROR x "+v.phrase+g, "ERROR "+g+v.phrase+g)
		}
		if _, all := scan("glued", lines); strings.Contains(all, v.secret) {
			i := strings.Index(all, v.secret)
			t.Fatalf("%q escapes the scan when glued: …%s…", v.phrase, all[max(0, i-60):min(len(all), i+len(v.secret)+20)])
		}
	}
	ibans := []string{"FR76 3000 6000 0112 3456 7890 189", "DE89 3704 0044 0532 0130 00", "GB82 WEST 1234 5698 7654 32", "BE68 5390 0754 7034",
		"KZ86 125K ZT50 0410 0100", "ES91 2100 0418 4502 0005 1332", "NL91 ABNA 0417 1643 00"}
	var lines []string
	for _, ib := range ibans {
		lines = append(lines, "ERROR iban "+ib+" refused", "ERROR iban "+strings.ReplaceAll(ib, " ", "")+" refused")
	}
	out, all := scan("ibans", lines)
	for _, ib := range ibans {
		if strings.Contains(all, ib) || strings.Contains(all, strings.ReplaceAll(ib, " ", "")) {
			t.Fatalf("an IBAN followed by a word escapes the scan: %s", ib)
		}
	}
	for _, f := range out["leak_findings"].([]any) {
		if m := f.(map[string]any); m["class"] == "iban" && m["count"] != float64(len(lines)) {
			t.Fatalf("every IBAN followed by a word is found: %v of %d", m["count"], len(lines))
		}
	}
	// A secret by its key, in the shapes logs carry it — an environment
	// variable and the suffixes a secret name takes, a JSON field, a camelCase
	// name, a quoted value running to its closing quote, a command-line flag,
	// `=>`, a short value; and credentials outside a key — a header scheme,
	// Basic, SQL, a DSN, a well-known token prefix: the value never comes out.
	long := strings.Repeat("Zq7h9xAb3c", 5)
	for _, c := range []struct{ shape, secret string }{
		{"DB_PASSWORD=hunter2hunter2", "hunter2hunter2"}, {"API_TOKEN=hunter2hunter2", "hunter2hunter2"}, {"CLIENT_SECRET=hunter2hunter2", "hunter2hunter2"},
		{"AWS_SECRET_ACCESS_KEY=hunter2hunter2", "hunter2hunter2"}, {"SECRET_KEY_BASE=hunter2hunter2", "hunter2hunter2"},
		{`{"password":"hunter2hunter2"}`, "hunter2hunter2"}, {`{"api_key": "hunter2hunter2"}`, "hunter2hunter2"}, {`{"dbPassword":"hunter2hunter2"}`, "hunter2hunter2"},
		{"dbPassword=hunter2hunter2", "hunter2hunter2"}, {"--password hunter2hunter2", "hunter2hunter2"}, {"-token hunter2hunter2", "hunter2hunter2"},
		{"Authorization: Basic aHVudGVyMmh1bnRlcjI=", "aHVudGVyMmh1bnRlcjI="},
		{"SECRET_KEY=Zq7h9xAb3c", "Zq7h9xAb3c"}, {"JWT_SECRET_KEY=Zq7h9xAb3d", "Zq7h9xAb3d"}, {"STRIPE_SECRET_KEY=Zq7h9xAb3e", "Zq7h9xAb3e"},
		{"secretKey: Zq7h9xAb3f", "Zq7h9xAb3f"}, {"CLIENT_SECRET_VALUE=Zq7h9xAb3g", "Zq7h9xAb3g"}, {"DB_PASSWORD_PROD=Zq7h9xAb3h", "Zq7h9xAb3h"},
		{"PASSWORD1=Zq7h9xAb3i", "Zq7h9xAb3i"}, {"ADMIN_PASSWORD_2=Zq7h9xAb3j", "Zq7h9xAb3j"}, {"TOKEN_V2=Zq7h9xAb3k", "Zq7h9xAb3k"},
		{"Config{DBPassword:Zq7h9xAb3l}", "Zq7h9xAb3l"},
		{`"password":"Xy7;abcdefgh"`, "Xy7;abcdefgh"}, {`password="Ab1,cd2efgh"`, "Ab1,cd2efgh"}, {`password: "my secret phrase"`, "my secret phrase"},
		{`"password": "correct horse battery"`, "horse battery"},
		{"--db-password Zq7h9xAb3m", "Zq7h9xAb3m"}, {"--auth-token Zq7h9xAb3n", "Zq7h9xAb3n"}, {"--client-secret Zq7h9xAb3o", "Zq7h9xAb3o"},
		{`{"password"=>"Zq7h9xAb3p"}`, "Zq7h9xAb3p"}, {"[password] => Zq7h9xAb3q", "Zq7h9xAb3q"}, {`'password' => 'Zq7h9xAb3r'`, "Zq7h9xAb3r"},
		{"password=abc123", "abc123"}, {`"token": "Zq7h9x"`, "Zq7h9x"}, {"DB_PASSWORD=Zq7h9xA", "Zq7h9xA"}, {"API_KEY=Zq7h9xAb3s", "Zq7h9xAb3s"},
		{"Authorization: Token " + long, long}, {"Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"CREATE USER app IDENTIFIED BY 'Zq7h9xAb3t'", "Zq7h9xAb3t"}, {"ALTER ROLE app WITH PASSWORD 'Zq7h9xAb3u'", "Zq7h9xAb3u"},
		{"redis://:Zq7h9xAb3v@redis:6379/0", "Zq7h9xAb3v"}, {"postgres://app:Zq7h9xAb3w@db:5432/app", "Zq7h9xAb3w"},
		{"sk_live_" + long[:24], long[:24]}, {"AIza" + long[:35], long[:35]}, {"hf_" + long[:34], long[:34]}, {"npm_" + long[:36], long[:36]},
		{"SG." + long[:22] + "." + long[:43], long[:43]},
		// An escaped quote stays inside a quoted value; a never-closed quote
		// runs to the end of the line; the bare word pass; a base64 token may
		// start with `/`; a Basic credential may be unpadded or hold latin-1
		// letters; two suffixes.
		{`{"password":"ab\"cd12345"}`, `ab\"cd12345`},
		{"ADMIN_PASS=Zq7h9xAb3cZq failed", "Zq7h9xAb3cZq"},
		{"--db-pass Zq7h9xAb3cZq app", "Zq7h9xAb3cZq"},
		{"dbPass=Zq7h9xAb3cZq failed", "Zq7h9xAb3cZq"},
		{"token=/+AbCdEf123456 failed", "/+AbCdEf123456"},
		{"creds basic dXNlcjpwYXNzd29yZOk= in config", "dXNlcjpwYXNzd29yZOk="},
		{"PASSWORD_2_PROD=Zq7h9xAb3Z ok", "Zq7h9xAb3Z"},
		// An unquoted value rides its plain tail; a passphrase is a secret
		// word; a Basic credential may be unpadded; a single-quoted value
		// takes an escaped quote and a never-closed quote; a base64 token
		// may hold more than one slash.
		{"password: hunter2 sekrit7", "sekrit7"},
		{"password = Zq7h9xAb3c please rotate", "Zq7h9xAb3c please rotate"},
		{`passphrase="correct horse battery staple"`, "correct horse battery staple"},
		{"passphrase=osk8DuubSideout ok", "osk8DuubSideout"},
		{"--passphrase osk8DuubSideou1 app", "osk8DuubSideou1"},
		{"creds basic dXNlcjpwYXNzd29yZA in config", "dXNlcjpwYXNzd29yZA"},
		{`password='ab\'cd12345' ok`, `ab\'cd12345`},
		{"db_pass=Zq7h9xAb3cZq failed", "Zq7h9xAb3cZq"},
		{"token=/Ab/CdEfGh12345 loaded", "/Ab/CdEfGh12345"},
		// The value-shape refusals hold only where a status feed can live:
		// a quoted, DSN or flag value is always a credential, and so is a
		// shout-case or snake_case value under a credential key; a quoted
		// flag value is a value; the tail rides base64 padding; a long
		// letters-only word is a secret.
		{"SECRET_KEY=PROD_DB_MASTER ok", "PROD_DB_MASTER"},
		{"db_password=prod_db_master_2026 ok", "prod_db_master_2026"},
		{`password="super_secret_value"`, "super_secret_value"},
		{"postgres://app:prod_db_master@db:5432/app ok", "prod_db_master"},
		{"started with --password my_secret_pass ok", "my_secret_pass"},
		{"authorization: token Backup_Admin_Token_Long ok", "Backup_Admin_Token_Long"},
		{"password=supersecretvalue ok", "supersecretvalue"},
		{"password=hunter2 beta Zq7h9xAb3cZq== done", "Zq7h9xAb3cZq=="},
		{"token=Zq7h9xAb3c dGVzdA= done", "dGVzdA="},
		{`start --password "Zq7h9xAb3cZq" --host db`, "Zq7h9xAb3cZq"},
		{`start --password "hunter2 sekrit7" ok`, "hunter2 sekrit7"},
		{`start --token 'Zq7h9xAb3cZq' ok`, "Zq7h9xAb3cZq"},
		{"token=/ab/cdefgh12 loaded", "/ab/cdefgh12"},
		{"token=/ab12/cd34ef56 loaded", "/ab12/cd34ef56"},
		// A credential riding behind ANY refused head is scanned; the quoted
		// floor is character-based (an escape pair counts its two chars).
		{"password=/run/secrets/db Zq7h9xAb3cZq== rejected", "Zq7h9xAb3cZq=="},
		{`password="/run/secrets/db Zq7h9xAb3cZq" rejected`, "Zq7h9xAb3cZq"},
		{"deploy start --password \"/run/secrets/db Zq7h9xAb3cZq\" rejected", "Zq7h9xAb3cZq"},
		{"password=/etc/shadow Zq7h9xAb3cZq== rejected", "Zq7h9xAb3cZq=="},
		{"password=~/secrets/db Zq7h9xAb3cZq== rejected", "Zq7h9xAb3cZq=="},
		{"password=../secrets/db Zq7h9xAb3cZq== rejected", "Zq7h9xAb3cZq=="},
		{"password=./secrets/db Zq7h9xAb3cZq== rejected", "Zq7h9xAb3cZq=="},
		{"password=required Zq7h9xAb3cZq rejected", "Zq7h9xAb3cZq"},
		{"password=abcdefghijk Zq7h9xAb3cZq rejected", "Zq7h9xAb3cZq"},
		{"password=[Redacted] Zq7h9xAb3cZq rejected", "Zq7h9xAb3cZq"},
		{"password=${DB_PASSWORD} Zq7h9xAb3cZq rejected", "Zq7h9xAb3cZq"},
		{"resume_token=826C1A2B3C4D Zq7h9xAb3cZq", "Zq7h9xAb3cZq"},
		{`password="a\bcde"`, `a\bcde`},
		{"CREATE USER app IDENTIFIED BY 'a\\bc' ok", "a\\bc"},
		{"job.token=12345678 Zq7h9xAb3cZq", "Zq7h9xAb3cZq"},
		{"mountain_pass=closed_for_winter Zq7h9xAb3cZq", "Zq7h9xAb3cZq"},
		// A credential riding behind a path-shaped head is scanned; an
		// escaped quote inside a never-closed value (after a key, a flag,
		// in SQL) no longer defeats the arms.
		{"token=/data/redis Zq7h9xAb3cZq== done", "Zq7h9xAb3cZq=="},
		{`deploy start --password "/data/redis/backup Zq7h9xAb3cZq`, "Zq7h9xAb3cZq"},
		{`password="Zq7h\"9xAb3c`, `Zq7h\"9xAb3c`},
		{`deploy start --password "Zq7h\"9xAb3c`, `Zq7h\"9xAb3c`},
		{"CREATE USER app IDENTIFIED BY 'Zq7h\\'9xAb3c ok", "9xAb3c"},
	} {
		// A secret holding a quote or a backslash rides the JSON-escaped
		// samples: both forms must be absent.
		esc := func(s string) string {
			s = strings.ReplaceAll(s, `\`, `\\`)
			return strings.ReplaceAll(s, `"`, `\"`)
		}
		if _, all := scan("shape", []string{"ERROR config " + c.shape + " rejected"}); strings.Contains(all, c.secret) || strings.Contains(all, esc(c.secret)) {
			t.Fatalf("a secret in the shape %q escapes the scan", c.shape)
		}
	}
	// A quoted secret longer than the 300-character samples still yields its
	// finding — the shape battery cannot see a secret longer than the samples
	// it greps, so the finding itself is what is asserted.
	out, _ = scan("quoted-400", []string{`ERROR config password="` + strings.Repeat("Zq7h9xAb3c", 40) + `" ok rejected`})
	if !strings.Contains(fmt.Sprint(out["leak_findings"]), "class:secret_kv") {
		t.Fatalf("a quoted secret longer than the samples yields no finding: %v", out["leak_findings"])
	}
	// A quote never closed keeps its witnesses on the exact reported lines —
	// the shape battery's ` rejected` tail would read as a sentence. A
	// truncated passphrase (two spaces), base64 (its `+`), and a value with
	// a timestamp tail are secrets, not prose — after a key, after a flag
	// and in SQL alike.
	_, all = scan("eol-keep", []string{`ERROR config password="Zq7h9xAb3cZq running`, `ERROR config password='Zq7h9xAb3cZq running`,
		`ERROR config password="correct horse battery`, `ERROR config password='S3cretV4lue at 0x7f9a3b2c`,
		`ERROR config password="c2VjcmV0+K3Rva2Vu`,
		`deploy start --password "hunter2 sekrit7`, `deploy start --token 'Zq7h9xAb3cZq`,
		`CREATE USER app IDENTIFIED BY 'Zq7h9xAb3c`})
	if strings.Contains(all, "Zq7h9xAb3cZq running") || strings.Contains(all, "correct horse battery") ||
		strings.Contains(all, "S3cretV4lue at 0x7f9a3b2c") || strings.Contains(all, "c2VjcmV0+K3Rva2Vu") ||
		strings.Contains(all, "hunter2 sekrit7") || strings.Contains(all, "Zq7h9xAb3c") {
		t.Fatalf("a secret on a quote never closed escapes the scan")
	}
	// A tail starting with a space is continuation text, not a value — the
	// one refusal no other guard covers.
	out, _ = scan("eol-space", []string{`ERROR config password=" abcd tail`})
	if strings.Contains(fmt.Sprint(out["leak_findings"]), "class:secret_kv") {
		t.Fatalf("a continuation tail after a quote raises a leak: %v", out["leak_findings"])
	}
	// The three-space trade, pinned: a three-word tail after an unclosed
	// quote pages (the value is masked) — the shape battery's ` rejected`
	// tail would make it a four-word sentence, so the exact line is what
	// is asserted.
	out, _ = scan("eol-trade", []string{`ERROR config password="no such file`})
	if !strings.Contains(fmt.Sprint(out["leak_findings"]), "class:secret_kv") {
		t.Fatalf("a three-word tail no longer pages: %v", out["leak_findings"])
	}
	// A value ending in a single backslash (exact lines — the battery's
	// ` rejected` suffix would let the escape pair cross it), and a
	// credential riding behind a refused head on a never-closed quote.
	_, all = scan("backslash-keep", []string{
		`ERROR config password="abcdef\`, `ERROR config password='abcdef\`,
		`CREATE USER app IDENTIFIED BY 'abcdef\`, `deploy start --password "abcdef\`,
		`ERROR config password="/run/secrets/db Zq7h9xAb3cZq`})
	if strings.Contains(all, `abcdef\`) || strings.Contains(all, "Zq7h9xAb3cZq") {
		t.Fatalf("RVA20: a value ending in a backslash or a path-headed rider escapes the scan")
	}
	// Look-alikes raise no secret finding: counters and paths named after a
	// secret word, a logger's own mask or placeholder, a reference to a
	// variable, API pagination tokens, a status word, CLI usage and flag
	// errors, `basic` followed by an ordinary word.
	benign := []string{"INFO usage total_tokens=123456", "INFO token_count=4096 ok", "INFO PASSWORD_FILE=/run/secrets/db",
		`{"password":"********"}`, `{"password":"[FILTERED]"}`, `{"req":{"password":"[Redacted]"}}`, "password: xxxxxxxx", "password=${DB_PASSWORD}",
		`{"prompt_token": 123456}`, `{"input_token":150000}`, `{"cached_token": 204800}`,
		"NextToken=AAAAQmFzZTY0UGFnZVRva2Vu", `{"nextPageToken": "CAoQAA7h9xAb"}`, "next_token=eyJvZmZzZXQiOjEwMH0&limit=100", "page_token=CAoQAA7h",
		`{"continuationToken": "abc123def456"}`, "sync_token: 7h9xAb3cZq", "resume_token=826C1A2B3C4D", "ContinuationToken=1ueGcxLPRx1Tr",
		"refreshToken: expired", "csrfToken: missing", "invalidToken: signature mismatch", "resetPassword: failed", "forgotPassword: request accepted",
		"Error: --token requires a value", "missing --password argument", "usage: backup --password PASSWORD --host HOST", "--api-key missing",
		"option --passwd ignored", "--token /run/secrets/token",
		"basic authentication/authorization failed", "using basic internationalization support", "basic misconfiguration detected",
		"auth=basic /api/v1/users/login 401", "falling back to basic authentication.method",
		// a scheme word followed by a short word is prose, not a credential
		"authorization: token authentication failed", "authorization: token validation failed",
		"authorization: bot escalation created", "authorization: bot deactivated by admin",
		"authorization: ssws session expired ok", "authorization: apikey revoked for user42",
		"authorization: basic authentication required", "authorization: ssws authentication expired",
		// a snake_case status word after a scheme word is prose; so is code
		// or a sentence quoting a key with an unclosed quote, a status value
		// under a `*_pass` key, a shout-case placeholder, a Basic blob whose
		// password part is empty, and a relative path to a secret
		`logger.debug("token=" + token);`, "console.log('token=' + token)",
		`res.json({error: "missing token=" + key})`, "token=' + tok)",
		`parse stopped at token=" near offset 12`, `the filter pattern token="ABC must be quoted`,
		"authorization: token authentication_required", "authorization: bot deactivated_by_administrator",
		"authorization: ssws session_expired_and_locked",
		"mountain_pass=closed_for_winter status", "border_pass=control_post_9 status", "gate_pass=open_gate check",
		"usage: app --pass CHANGE_ME --host H", "auth basic MTIzNDU2Nzg6 handler", "token=../secrets/db-pass loaded",
		// a glued code tail after a quoted key (no leading space, no spaces
		// at all), and a JWT already redacted: the plain word after it must
		// not turn the marker into a finding
		`res.write("token="+tok);`,
		"auth token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnopqrstuvwxyz used",
		// a filesystem path under a secret key is no leak; /var/lib needs
		// its prefix (the segments hold an uppercase); a tail holding `;`
		// alone, or `)` alone, still reads as code; a two-slash lowercase
		// path; an uppercase status feed; a shout placeholder after a flag,
		// quoted or not
		"token=/data/redis/sessions loaded", "token=/var/log/app.log rotated", "mount token=/var/lib/App7/secret ok",
		"token=/data/redis/sessions",
		// the character floor: a short quoted value is no secret
		`password="a1!" rejected`,
		`password="ab;cd12345 done`, `password="p@ssw0rd)123 done`,
		"token=/data/redis loaded", "MOUNTAIN_PASS=closed_for_winter status",
		`app start --pass "CHANGE_ME_NOW" --host db`,
		// the refusal side of the round-18 arms, each on its own witness
		`deploy start --password "no such file here`,
		`CREATE USER app IDENTIFIED BY 'no such file here`,
		`app start --pass 'CHANGE_ME_NOW`}
	secretFinding := func(out map[string]any) bool {
		f := fmt.Sprint(out["leak_findings"])
		return strings.Contains(f, "class:secret_kv") || strings.Contains(f, "class:bearer")
	}
	if out, _ := scan("benign", benign); secretFinding(out) {
		for _, l := range benign {
			if o, _ := scan("benign-one", []string{l}); secretFinding(o) {
				t.Fatalf("a look-alike raises a secret finding: %q -> %v", l, o["leak_findings"])
			}
		}
		t.Fatalf("look-alikes raise a secret finding: %v", out["leak_findings"])
	}
	// A run of host names and short words is no IBAN because some prefix of it
	// passes the checksum — the country's IBAN length decides; two IBANs one
	// space apart are both found.
	out, _ = scan("iban-prose", []string{"INFO job TD32 done cart next shut last with ok", "INFO job WL58 item done sent shut fail last item fail ok"})
	if strings.Contains(fmt.Sprint(out["leak_findings"]), "class:iban") {
		t.Fatalf("IBAN-shaped prose is no IBAN: %v", out["leak_findings"])
	}
	out, all = scan("iban-pair", []string{"ERROR payer payee BE68 5390 0754 7034 NL91 ABNA 0417 1643 00 refused"})
	if strings.Contains(all, "NL91 ABNA") || strings.Contains(all, "BE68 5390") || !strings.Contains(fmt.Sprint(out["leak_findings"]), "class:iban count:2") {
		t.Fatalf("two IBANs one space apart are both found: %v", out["leak_findings"])
	}
	// A secret's sample shows its length only, never a character of it —
	// the length of the whole redacted span (the plain tail rides along),
	// whitespace folded.
	out, _ = scan("sample", []string{"ERROR login password=abc123def rejected"})
	for _, f := range out["leak_findings"].([]any) {
		if m := f.(map[string]any); m["class"] == "secret_kv" && m["sample_masked"] != "*** (17 chars)" {
			t.Fatalf("a secret's sample shows its length only: %q", m["sample_masked"])
		}
	}
	// A card word glued to a letter of another script still makes the
	// digit run next to it a card: a leak found, not only a masked number.
	out, _ = scan("card-words", []string{"ERROR 支付carte 4539148803436467 refusée", "ERROR Покупкаcard 4539148803436467 declined"})
	if !strings.Contains(fmt.Sprint(out["leak_findings"]), "class:card count:2") {
		t.Fatalf("a card word glued to another script is still a card word: %v", out["leak_findings"])
	}
}

// TestProdWatch_NoSecretEscapesAnyOutput: a Grafana token or a webhook URL
// in a malformed shape — two lines in the token file, a space, a tab or a
// control character in the token, a URL without a scheme, with a space or a
// control character — never reaches any output: no node's stdout or
// stderr, the messages, the state, the ledgers. The lane or the delivery
// fails by name.
func TestProdWatch_NoSecretEscapesAnyOutput(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, token := range []string{"glsa_CANARYnew0123456789\nglsa_CANARYold9876543210", "glsa_CANARY 0123456789", "glsa_CANARY\t0123456789", "glsa_CANARY\x010123456789"} {
		h := newPWHarness(t)
		h.healthStatus.Store(503)
		if err := os.WriteFile(h.tokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		outs := h.tick(t, wf, false)
		if all := pwEverything(t, h, outs); strings.Contains(all, "CANARY") {
			i := strings.Index(all, "CANARY")
			t.Fatalf("token %q escapes: …%s…", token, all[max(0, i-80):min(len(all), i+60)])
		}
		if r := pwCoverageReasons(outs["decide"]); !strings.Contains(r, "not one token on one line") {
			t.Fatalf("token %q: the lanes name the malformed token, without quoting it: %q", token, r)
		}
		for _, lane := range []string{"poll_loki", "poll_prom"} {
			if e := fmt.Sprint(outs[lane]["errors"]); !strings.Contains(e, "not one token on one line") {
				t.Fatalf("token %q: %s names the malformed token on its own errors: %s", token, lane, e)
			}
		}
	}
	alert := []map[string]any{{"fingerprint": "probe:api", "kind": "probe", "severity": "critical", "state": "new", "title_key": "probe_down", "title_arg": "api",
		"detail_key": "probe_detail", "fields": map[string]any{}, "evidence": map[string]any{}, "count": 1, "first_seen": "2026-09-23T10:00:00+00:00"}}
	for _, hook := range []string{"hooks.example/CANARY", "/hook CANARY", "/hook\x01CANARY", "https://bob:CANARY@hooks.example.com/hooks/abc",
		"https://hooks.example.com:CANARY/hooks/abc", "file://localhost/CANARY-hook"} {
		h := newPWHarness(t)
		if strings.HasPrefix(hook, "/") {
			hook = h.srv.URL + hook
		}
		b, _ := json.Marshal(map[string]string{"w1": hook})
		if err := os.WriteFile(h.webhooksFile, b, 0o600); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
			"alerts": alert, "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "", "min_severity": "low"}},
			"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false, "dry_run": false, "max_message_chars": 14000},
			nil, map[string]string{"webhooks": h.webhooksFile}))
		if all := fmt.Sprint(out) + stderr + fmt.Sprint(err); strings.Contains(all, "CANARY") {
			t.Fatalf("webhook %q escapes: %s", hook, all)
		}
		if err == nil || !strings.Contains(stderr, "w1") {
			t.Fatalf("webhook %q: the delivery fails, naming the webhook: %v %s", hook, err, stderr)
		}
	}
}

// TestProdWatch_AnyQueryThatCountedAPatternKeepsItObserved: a pattern two
// overlapping queries count, or a leak a query found, stays observed while
// any query that counted it is configured — its "not observed any more"
// comes once they answered without it; a leak only a removed query found
// gets none (nothing looks for it any more). The normal path for a leak
// incident included.
func TestProdWatch_AnyQueryThatCountedAPatternKeepsItObserved(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	queries := func(qs map[string]any) func(cfg map[string]any) {
		return func(cfg map[string]any) {
			lokiOnly(1000, 60)(cfg)
			cfg["loki"].(map[string]any)["queries"] = qs
		}
	}
	h.writeConfig(t, queries(map[string]any{"a-errors": "a-q", "b-errors": "b-q", "leak_sweep": "sweep-q"}))
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	now := time.Now().UnixNano()
	const pattern = "ERROR payment gateway refused the card"
	h.lines.Store([]pwLine{
		{TS: now, Line: pattern, Container: "api", Q: "a-q"}, {TS: now, Line: pattern, Container: "api", Q: "b-q"},
		{TS: now + 1, Line: "WARN login ok for jean.dupont@example.org", Container: "api", Q: "sweep-q"},
		{TS: now + 2, Line: "ERROR transfer to FR7630006000011234567890189 refused", Container: "api", Q: "b-q"},
	})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	st := h.state(t)
	for fp, rec := range st["incidents"].(map[string]any) {
		rec.(map[string]any)["last_seen"] = hoursAgo(49)
		if fp == pwTemplateFP(pattern) && fmt.Sprint(rec.(map[string]any)["sources"]) != "[a-errors b-errors]" {
			t.Fatalf("the pattern records every query that counted it: %v", rec)
		}
	}
	h.setState(t, st)
	h.writeConfig(t, queries(map[string]any{"b-errors": "b-q"}))
	quiet := strings.Join(pwQuietFPs(h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))), ",")
	if !strings.Contains(quiet, pwTemplateFP(pattern)) || !strings.Contains(quiet, "leak:iban") {
		t.Fatalf("a query that counted it is still configured: the pattern and the IBAN leak get their note: %v", quiet)
	}
	if strings.Contains(quiet, "leak:email") {
		t.Fatalf("a leak only the removed sweep found gets no note: %v", quiet)
	}
}

// TestProdWatch_CoverageNoteSaysEachComponentOnce: a tick's partiality is
// made of components — a lane's error kind, a query's gap, a truncation, a
// cut; the note posts when a component is new or its last mention is older
// than renotify_hours, whatever the combination; the same error text on
// two lanes is two components; a component expires from the state after
// the interval.
func TestProdWatch_CoverageNoteSaysEachComponentOnce(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	const e500 = "HTTPError: HTTP Error 500: Internal Server Error"
	window := func(truncated, gap bool, err string) map[string]any {
		return map[string]any{"lines": 20, "error": err, "truncated": truncated, "gap": gap, "from_ns": "100", "to_ns": "900"}
	}
	tick := func(prom, gap, trunc, lokiErr bool) bool {
		perr, lerr := []any{}, []any{}
		if prom {
			perr = append(perr, map[string]any{"probe": "restarts", "error": e500})
		}
		pq := map[string]any{"errors": window(false, gap, ""), "other": window(trunc, false, "")}
		if lokiErr {
			lerr = append(lerr, map[string]any{"query": "third", "error": e500})
			pq["third"] = window(false, false, e500)
		}
		cov := "full"
		if gap || trunc || lokiErr {
			cov = "partial"
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": cov}, state, map[string]any{
			"prom_ok": !prom, "prom_errors": perr, "loki_errors": lerr, "loki_ok": true, "loki_per_query": pq,
			"lanes": map[string]any{"loki": true, "prometheus": true, "probes": true}})
		if err != nil {
			t.Fatalf("decide: %v %s", err, stderr)
		}
		state = pwStateNext(t, out)
		return pwCoverageReasons(out) != ""
	}
	var notes []bool
	for _, c := range [][4]bool{{true, false, false, false}, {true, true, false, false}, {false, true, false, false}, {true, false, true, false},
		{true, true, true, false}, {false, true, true, false}, {false, false, false, true}} {
		notes = append(notes, tick(c[0], c[1], c[2], c[3]))
	}
	if fmt.Sprint(notes) != "[true true false true false false true]" {
		t.Fatalf("a note when a component is new, whatever the combination (the same error on another lane is new): %v", notes)
	}
	posted := state["coverage_posted"].(map[string]any)
	for k := range posted {
		posted[k] = hoursAgo(25)
	}
	tick(false, false, false, false)
	if left := state["coverage_posted"].(map[string]any); len(left) != 0 {
		t.Fatalf("a component older than renotify_hours leaves the state: %v", left)
	}
}

// TestProdWatch_UpstreamTextStaysOut: what an upstream answers with HTTP
// 200 in the wrong shape — a Loki status that is not an enum, a log line
// where a timestamp belongs, a Prometheus scalar that is text, an error
// type that is not an enum — never reaches the note, the state or any
// output: the lane error says it did not parse.
func TestProdWatch_UpstreamTextStaysOut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	const pii = "jean.dupont@example.org"
	for _, c := range []struct {
		name      string
		loki      map[string]any
		prom      map[string]any
		promW     []string // Prometheus warnings on a breached value
		rawG      string   // the status line every Grafana call answers with
		rawH      string   // the status line the health endpoint answers with
		version   string   // the health endpoint's version field
		announced bool     // a lane error the coverage note announces

		wantWarnings int // the Prometheus warning count the alert evidence carries
	}{
		{name: "a Loki status that is text", loki: map[string]any{"status": "no data for " + pii}, announced: true},
		{name: "a line in the timestamp slot", loki: map[string]any{"status": "success", "data": map[string]any{"resultType": "streams",
			"result": []any{map[string]any{"stream": map[string]any{"container": "api"}, "values": []any{[]any{"login failed for " + pii, "x"}}}}}}, announced: true},
		{name: "a Prometheus scalar that is text", prom: map[string]any{"status": "success", "data": map[string]any{"resultType": "scalar", "result": []any{0, "user " + pii}}}, announced: true},
		{name: "an error type that is text", prom: map[string]any{"status": "error", "errorType": "bad data for " + pii}, announced: true},
		{name: "a Grafana 5xx reason phrase that is text", rawG: "HTTP/1.1 500 login failed for " + pii, announced: true},
		{name: "a Grafana 4xx reason phrase that is text", rawG: "HTTP/1.1 400 bad data for " + pii, announced: true},
		{name: "a health status line that is text", rawH: "login failed for " + pii},
		{name: "a health reason phrase that is text", rawH: "HTTP/1.1 500 login failed for " + pii},
		{name: "a version that is not a version", version: "user " + pii},
		{name: "a version that is an email", version: pii},
		{name: "Prometheus warnings quoting text", promW: []string{"bad label value for " + pii}, wantWarnings: 1},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			if c.loki != nil {
				h.lokiBody.Store(c.loki)
			}
			if c.prom != nil {
				h.prom.Store(map[string]pwProm{"restarts-q": {Body: c.prom}})
			}
			if c.promW != nil {
				h.prom.Store(map[string]pwProm{"restarts-q": {Value: "999", Warnings: c.promW}})
			}
			h.rawGrafana.Store(c.rawG)
			h.rawHealth.Store(c.rawH)
			h.healthVersion.Store(c.version)
			outs := h.tick(t, wf, false)
			if all := pwEverything(t, h, outs); strings.Contains(all, pii) {
				i := strings.Index(all, pii)
				t.Fatalf("upstream text escapes: …%s…", all[max(0, i-120):min(len(all), i+40)])
			}
			if r := pwCoverageReasons(outs["decide"]); c.announced && r == "" {
				t.Fatalf("the lane error is announced: %v", outs["decide"]["stale_sources"])
			}
			if c.wantWarnings > 0 {
				if got := fmt.Sprint(outs["decide"]["alerts"]); !strings.Contains(got, "warnings:"+strconv.Itoa(c.wantWarnings)) {
					t.Fatalf("the alert evidence carries the warning count: %s", got)
				}
			}
		})
	}
}

// TestProdWatch_ZeroWidthWindowStillReportsAnUnusableGrafana: a first
// window of zero minutes reads nothing — but a token the lane cannot use
// is still that query's error, never a covered window and a healthy lane.
func TestProdWatch_ZeroWidthWindowStillReportsAnUnusableGrafana(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		lokiOnly(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["bootstrap_window_minutes"] = 0
	})
	if err := os.WriteFile(h.tokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{"grafana_token": h.tokenFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), secrets))
	if err != nil {
		t.Fatalf("plan: %v %s", err, stderr)
	}
	loki, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{"grafana": plan["grafana"], "loki": plan["loki"],
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil || loki["ok"] != false {
		t.Fatalf("an unusable token fails the lane even on a zero-width window: %v %v %s", err, loki, stderr)
	}
	if e := fmt.Sprint(loki["per_query"].(map[string]any)["errors"].(map[string]any)["error"]); !strings.Contains(e, "not bound or empty") {
		t.Fatalf("the query names the cause: %q", e)
	}
}

// TestProdWatch_ABrokenFirstTokenLosesNoLine: a token unusable from the first
// tick on — whatever the bootstrap window, zero minutes included — loses no
// line: once the token works, the window reopens where the first one opened,
// and the lines logged meanwhile are read.
func TestProdWatch_ABrokenFirstTokenLosesNoLine(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, boot := range []int{0, 10} {
		h := newPWHarness(t)
		h.writeConfig(t, func(cfg map[string]any) {
			lokiOnly(1000, 60)(cfg)
			cfg["loki"].(map[string]any)["bootstrap_window_minutes"] = boot
			cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
		})
		if err := os.WriteFile(h.tokenFile, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		vars := cursorVars(h, 60, 0, 5000)
		h.cursorTick(t, wf, vars)
		time.Sleep(30 * time.Millisecond)
		h.lines.Store([]pwLine{{TS: time.Now().UnixNano(), Line: "ERROR payment refused while the token was broken", Container: "api", Q: "errors-q"}})
		time.Sleep(30 * time.Millisecond)
		h.cursorTick(t, wf, vars)
		if err := os.WriteFile(h.tokenFile, []byte(pwToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
		read := 0
		for i := 0; i < 2; i++ {
			read += int(h.cursorTick(t, wf, vars)["poll_loki"]["lines"].(float64))
		}
		if read != 1 {
			t.Fatalf("bootstrap %d min: the line logged while the token was broken is read once it works: %d line(s)", boot, read)
		}
	}
}

// TestProdWatch_CoverageNoteNamesWhatItCut: the note says what fits its
// budget, what is new first, and counts as said only what it said — after an
// outage that gaps many queries, the one still losing lines is named, then
// the note stops; a Prometheus error behind Loki errors is named; a component
// said again is stamped again.
func TestProdWatch_CoverageNoteNamesWhatItCut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	win := func(gap bool, err string) map[string]any {
		return map[string]any{"lines": 20, "error": err, "truncated": false, "gap": gap, "from_ns": "100", "to_ns": "900"}
	}
	names := []string{"api-errors", "billing-errors", "checkout-errors", "jobs-errors", "mailer-errors", "orders-errors", "payments-errors", "search-errors"}
	gaps := func(gapped ...string) map[string]any {
		pq := map[string]any{}
		for _, n := range names {
			pq[n] = win(false, "")
		}
		for _, n := range gapped {
			pq[n] = win(true, "")
		}
		return pq
	}
	for _, next := range [][]string{{"search-errors"}, names} {
		h := newPWHarness(t)
		state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
		tick := func(pq map[string]any) string {
			out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}, state, map[string]any{
				"loki_per_query": pq, "lanes": map[string]any{"loki": true, "prometheus": false, "probes": false}})
			if err != nil {
				t.Fatalf("decide: %v %s", err, stderr)
			}
			state = pwStateNext(t, out)
			return pwCoverageReasons(out)
		}
		r1 := tick(gaps(names...))
		r2 := tick(gaps(next...))
		r3 := tick(gaps(next...))
		if len(r1) > 600 || !strings.Contains(r1+r2, "search-errors: gap") {
			t.Fatalf("after %v: the query still losing lines is named within the budget: %q / %q", next, r1, r2)
		}
		if r3 != "" {
			t.Fatalf("after %v: once every component is said, the note stops: %q", next, r3)
		}
	}
	h := newPWHarness(t)
	state := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{}, "health": map[string]any{}}
	const lokiErr = "URLError: <urlopen error [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1000)>"
	const promErr = "LaneError: Prometheus answered status error (execution)"
	tick := func(lokiDown bool) string {
		pq, lerr := map[string]any{"zz-ok": win(false, "")}, []any{}
		for _, n := range names[:5] {
			pq[n] = win(false, "")
			if lokiDown {
				pq[n] = win(false, lokiErr)
				lerr = append(lerr, map[string]any{"query": n, "error": lokiErr})
			}
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}, state, map[string]any{
			"loki_per_query": pq, "loki_errors": lerr, "loki_ok": true, "prom_ok": false,
			"prom_errors":  []any{map[string]any{"probe": "restarts", "error": promErr}},
			"prom_results": []any{map[string]any{"id": "restarts", "state": "error", "error": promErr}, map[string]any{"id": "cpu", "state": "healthy"}}})
		if err != nil {
			t.Fatalf("decide: %v %s", err, stderr)
		}
		state = pwStateNext(t, out)
		return pwCoverageReasons(out)
	}
	if r1, r2 := tick(true), tick(false); !strings.Contains(r1+r2, "Prometheus answered status error") {
		t.Fatalf("a Prometheus error behind Loki errors is named: %q / %q", r1, r2)
	}
	old := hoursAgo(20)
	state["coverage_posted"] = map[string]any{"gap: api-errors": old}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}, state, map[string]any{
		"loki_per_query": gaps("api-errors", "billing-errors"), "lanes": map[string]any{"loki": true, "prometheus": false, "probes": false}})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	if r := pwCoverageReasons(out); !strings.Contains(r, "api-errors: gap") {
		t.Fatalf("a note for a new component says the older one too: %q", r)
	}
	if stamp := pwStateNext(t, out)["coverage_posted"].(map[string]any)["gap: api-errors"]; stamp == old {
		t.Fatalf("a component said again is stamped again: %v", stamp)
	}
}

// TestProdWatch_ALoggerMaskIsNoLeakAndHidesNone: a logger's own mask where a
// password was (`[Redacted]`, pino's default) raises no leak; the real
// password logged next is a new leak alert, not swallowed by a false one.
func TestProdWatch_ALoggerMaskIsNoLeakAndHidesNone(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	line := func(pw string) {
		time.Sleep(20 * time.Millisecond)
		h.lines.Store([]pwLine{{TS: time.Now().UnixNano(), Line: `{"level":30,"msg":"login","req":{"body":{"user":"42","password":"` + pw + `"}}}`, Container: "api", Q: "sweep-q"}})
		time.Sleep(20 * time.Millisecond)
	}
	line("[Redacted]")
	if a := strings.Join(alertsOf(t, h.tick(t, wf, false)), ","); strings.Contains(a, "leak") {
		t.Fatalf("a logger's own mask is no leak: %s", a)
	}
	line("Zq7hunter2Xy9")
	outs := h.tick(t, wf, false)
	if a := strings.Join(alertsOf(t, outs), ","); !strings.Contains(a, "leak:new:high") {
		t.Fatalf("the real password logged next is a new leak alert: %s", a)
	}
	if all := pwEverything(t, h, outs); strings.Contains(all, "Zq7hunter2Xy9") {
		t.Fatalf("the real password never comes out")
	}
}

// TestProdWatch_LeakSourcesAreEveryQueryThatSawIt: a leak records every
// query that returned one of its lines — the sweep and a second template
// query included, however many streams it was found in — so removing one of
// them never freezes the incident while another still looks.
func TestProdWatch_LeakSourcesAreEveryQueryThatSawIt(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	queries := func(qs map[string]any) func(cfg map[string]any) {
		return func(cfg map[string]any) {
			lokiOnly(1000, 60)(cfg)
			cfg["loki"].(map[string]any)["queries"] = qs
		}
	}
	for _, c := range []struct {
		name, class, sources string
		lines                func(now int64) []pwLine
		configured, after    map[string]any
	}{
		{"one line three queries", "iban", "[a-errors b-errors leak_sweep]", func(now int64) []pwLine {
			const leak = "ERROR transfer to FR7630006000011234567890189 refused"
			return []pwLine{{TS: now, Line: leak, Container: "api", Q: "a-q"}, {TS: now, Line: leak, Container: "api", Q: "b-q"}, {TS: now, Line: leak, Container: "api", Q: "sweep-q"}}
		}, map[string]any{"a-errors": "a-q", "b-errors": "b-q", "leak_sweep": "sweep-q"}, map[string]any{"b-errors": "b-q", "leak_sweep": "sweep-q"}},
		{"nine streams and one", "email", "[a-errors b-errors]", func(now int64) []pwLine {
			var lines []pwLine
			for i := 0; i < 9; i++ {
				for k := 0; k < 3; k++ {
					lines = append(lines, pwLine{TS: now + int64(i*10+k), Line: fmt.Sprintf("ERROR user %d login jean.dupont@example.org", i*10+k), Container: fmt.Sprintf("api%d", i), Q: "a-q"})
				}
			}
			return append(lines, pwLine{TS: now + 500, Line: "WARN mail bounced for marie.curie@example.org", Container: "mailer", Q: "b-q"})
		}, map[string]any{"a-errors": "a-q", "b-errors": "b-q"}, map[string]any{"b-errors": "b-q"}},
	} {
		h := newPWHarness(t)
		h.writeConfig(t, queries(c.configured))
		h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		h.lines.Store(c.lines(time.Now().UnixNano()))
		h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
		st := h.state(t)
		if rec := st["incidents"].(map[string]any)["leak:"+c.class].(map[string]any); fmt.Sprint(rec["sources"]) != c.sources {
			t.Fatalf("%s: the leak records every query that returned its lines: %v", c.name, rec["sources"])
		}
		for _, r := range st["incidents"].(map[string]any) {
			r.(map[string]any)["last_seen"] = hoursAgo(49)
		}
		h.setState(t, st)
		h.lines.Store([]pwLine{})
		h.writeConfig(t, queries(c.after))
		if quiet := strings.Join(pwQuietFPs(h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))), ","); !strings.Contains(quiet, "leak:"+c.class) {
			t.Fatalf("%s: a query that found the leak is still configured, so it gets its note: %v", c.name, quiet)
		}
	}
}

// TestProdWatch_AStampInTheFutureSilencesNothing: a stamp a clock running
// ahead wrote — a component said, an incident notified, a source warned —
// reads as not said: it never silences what it stamps.
func TestProdWatch_AStampInTheFutureSilencesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	const future = "2999-01-01T00:00:00+00:00"
	state := func() map[string]any {
		return map[string]any{"version": 1, "generation": 7, "cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": "111", "frontier_ns": "111", "band": []any{}, "overlap_from_ns": "0"}}},
			"incidents": map[string]any{}, "health": map[string]any{}}
	}
	h := newPWHarness(t)
	st := state()
	st["coverage_posted"] = map[string]any{"gap: errors": future}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "coverage": "partial"}, st, map[string]any{
		"loki_per_query": map[string]any{"errors": map[string]any{"lines": 0, "error": "", "truncated": false, "gap": true, "from_ns": "100", "to_ns": "900"}},
		"lanes":          map[string]any{"loki": true, "prometheus": false, "probes": false}})
	if err != nil || !strings.Contains(pwCoverageReasons(out), "errors: gap") {
		t.Fatalf("a component said in the future is said now: %v %s %v", pwCoverageReasons(out), stderr, err)
	}
	st = state()
	st["incidents"] = map[string]any{"probe:api": incident("probe", "critical", true, 0, 0.1)}
	st["incidents"].(map[string]any)["probe:api"].(map[string]any)["last_notified"] = future
	probeDown := []map[string]any{{"id": "api", "url": "u", "ok": false, "status": 503, "ms": 5, "error": "HTTP 503", "expected": 200, "severity": "critical"}}
	out, stderr, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, st, map[string]any{"http_results": probeDown})
	if err != nil || strings.Join(alertsOf(t, map[string]map[string]any{"decide": out}), ",") != "probe:reminder:critical" {
		t.Fatalf("an incident notified in the future is reminded: %v %s %v", out["summary"], stderr, err)
	}
	st = state()
	st["health"] = map[string]any{"prometheus": map[string]any{"last_ok": hoursAgo(10), "last_warned": future}}
	out, stderr, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, st, map[string]any{
		"prom_ok": false, "prom_errors": []any{map[string]any{"probe": "restarts", "error": "LaneError: Prometheus answered status error (execution)"}},
		"lanes": map[string]any{"loki": false, "prometheus": true, "probes": true}, "http_results": []map[string]any{{"id": "api", "url": "u", "ok": true, "status": 200, "ms": 5, "error": "", "expected": 200, "severity": "critical"}}})
	if err != nil || !strings.Contains(fmt.Sprint(out["stale_sources"]), "source:prometheus") {
		t.Fatalf("a source warned in the future is warned now: %v %s %v", out["stale_sources"], stderr, err)
	}
}

// TestProdWatch_ATokenFileWithAByteOrderMarkStillWorks: a token file an
// editor saved with a UTF-8 byte-order mark is read as the token alone.
func TestProdWatch_ATokenFileWithAByteOrderMarkStillWorks(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	if err := os.WriteFile(h.tokenFile, []byte("\ufeff"+pwToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outs := h.tick(t, wf, false)
	for _, lane := range []string{"poll_loki", "poll_prom"} {
		if e := outs[lane]["errors"]; fmt.Sprint(e) != "[]" {
			t.Fatalf("%s reads the token behind a byte-order mark: %v", lane, e)
		}
	}
}

// TestProdWatch_ALokiStatusErrorIsNamed: a Loki answer whose status is
// "error" is the lane's own message — named as such, not "did not parse".
func TestProdWatch_ALokiStatusErrorIsNamed(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.lokiBody.Store(map[string]any{"status": "error"})
	if e := fmt.Sprint(h.tick(t, wf, false)["poll_loki"]["errors"]); !strings.Contains(e, "Loki answered status error") {
		t.Fatalf("a Loki status error is named: %s", e)
	}
}

// TestProdWatch_AnIncidentsSourcesAreTheQueriesThatLastCountedIt: an
// incident's sources follow the queries that counted it on its last counted
// tick — not the ones that counted it first.
func TestProdWatch_AnIncidentsSourcesAreTheQueriesThatLastCountedIt(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		lokiOnly(1000, 60)(cfg)
		cfg["loki"].(map[string]any)["queries"] = map[string]any{"a-errors": "a-q", "b-errors": "b-q"}
	})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	const pattern = "ERROR payment gateway refused the card"
	now := time.Now().UnixNano()
	h.lines.Store([]pwLine{{TS: now, Line: pattern, Container: "api", Q: "a-q"}, {TS: now, Line: pattern, Container: "api", Q: "b-q"}})
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	time.Sleep(20 * time.Millisecond)
	h.lines.Store([]pwLine{{TS: time.Now().UnixNano(), Line: pattern, Container: "api", Q: "b-q"}})
	time.Sleep(20 * time.Millisecond)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	if rec := h.state(t)["incidents"].(map[string]any)[pwTemplateFP(pattern)].(map[string]any); fmt.Sprint(rec["sources"]) != "[b-errors]" {
		t.Fatalf("the sources are the queries that last counted it: %v", rec["sources"])
	}
}
