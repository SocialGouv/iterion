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
// bursts under churn, zero-width ticks (lag 400 s). Each seed forces one of
// the four rare dimensions (seed % 4: first-window failure, gap, drop and
// re-add, zero-width) and asserts it was exercised. Oracle: the read-point
// oracle (no hole; every owed line written exactly once unless a
// legitimately declared gap excused it) and no line written twice.
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
	dimensions := []string{"first-window failure", "gap", "drop/re-add", "zero-width"}
	for seed := first; seed < first+seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			rnd := rand.New(rand.NewSource(int64(seed)*104729 + 17))
			force := seed % 4
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
			var drops, gaps, fails, bootErr, truncs, zero int
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
			tick := func(k, maxLines, lag, maxWin, failAt int) {
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
					return
				}
				pq := pqa.(map[string]any)
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
				}
				if lag > 2 {
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
				tick(k, 6000, 0, 60, 0)
			}
			t.Logf("seed %d (forced: %s) exercised: drops=%d gaps=%d failed-walks=%d (first-window=%d) truncated=%d zero-width=%d lines=%d",
				seed, dimensions[force], drops, gaps, fails, bootErr, truncs, zero, len(lines))
			if []int{bootErr, gaps, drops, zero}[force] == 0 {
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
	if !strings.Contains(reasons, "template list cut") {
		t.Fatalf("the coverage note names the cut: %q", reasons)
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
