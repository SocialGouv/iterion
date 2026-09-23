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
// query entering a gap does (lines are lost).
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
	if notes[4] == "" || !strings.Contains(notes[5], "template list cut") || !strings.Contains(notes[6], "other: truncated") {
		t.Fatalf("under a standing gap, a truncation ending, a cut appearing and a truncation appearing each re-post: %q", notes[4:])
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
	for _, title := range []any{5, 99.9} {
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
// in any case, a sink channel written as text, a number or not at all) and
// random production conditions — Prometheus failing in part or entirely,
// the health URL down, the Grafana token refused, lines with and without
// personal data — fourteen ticks per seed (PW_CHAIN_SEEDS, default 4): no
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
			h.writeConfig(t, func(cfg map[string]any) {
				restarts := map[string]any{"query": "restarts-q", "op": ">", "threshold": 0, "severity": sevs[rnd.Intn(4)]}
				if title := titles[rnd.Intn(4)]; title != nil {
					restarts["title"] = title
				}
				latency := map[string]any{"query": "lat-q", "op": ">=", "threshold": 1.5, "severity": sevs[rnd.Intn(4)], "title": titles[rnd.Intn(3)]}
				if rnd.Intn(2) == 0 {
					cfg["prometheus"] = map[string]any{"probes": map[string]any{"restarts": restarts, "latency": latency}}
				} else {
					restarts["id"], latency["id"] = "restarts", 7
					cfg["prometheus"] = map[string]any{"probes": []any{restarts, latency}}
				}
				cfg["probes"] = []map[string]any{{"id": 42, "url": h.srv.URL + "/health", "expect_status": 200, "severity": sevs[rnd.Intn(4)]}}
				sink := map[string]any{"webhook": "w1", "min_severity": sevs[rnd.Intn(4)]}
				if channel := []any{"ops", 2024, nil}[rnd.Intn(3)]; channel != nil {
					sink["channel"] = channel
				}
				cfg["sinks"] = []map[string]any{sink}
			})
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
				token := pwToken
				if rnd.Intn(5) == 0 {
					token = "glsa_refused"
				}
				if err := os.WriteFile(h.tokenFile, []byte(token+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
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
// type — no node needs their type — run a whole tick: a number where the
// bot writes text, strings where it writes booleans, dates that are not
// dates on the health records and a cursor's clock, unknown kinds of
// coverage keys.
func TestProdWatch_ForeignValuesNoConsumerReadsRunATick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, lokiTwoQueries(1000, 60))
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
	st := h.state(t)
	inc := incident("loki", "medium", true, 1, 1)
	for k, v := range map[string]any{"title_arg": 5, "alerted": "yes", "quiet_noted": "no", "last_notified": "", "fp": 7, "prev_severity": []any{1}} {
		inc[k] = v
	}
	st["incidents"] = map[string]any{"loki:foreign": inc}
	st["health"] = map[string]any{"loki": map[string]any{"last_ok": "x", "last_warned": 3, "last_error": map[string]any{}}}
	st["last_coverage"], st["last_coverage_key"] = 5, []any{}
	cur := st["cursors"].(map[string]any)["loki"].(map[string]any)
	for q := range cur {
		cur[q].(map[string]any)["at"] = 123
	}
	h.setState(t, st)
	h.cursorTick(t, wf, cursorVars(h, 60, 0, 5000))
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

// TestProdWatch_SinkNamesAreText: a sink's channel written as a number is
// its name — the alert is delivered and the tick consumed once, never
// re-posted every tick; a webhook name or a channel of another type is
// refused by name at the config.
func TestProdWatch_SinkNamesAreText(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["sinks"] = []map[string]any{{"webhook": "w1", "channel": 2024, "min_severity": "low"}}
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
			if m := outs["notify"]["messages"].([]any); len(m) == 0 || m[0].(map[string]any)["channel"] != "2024" {
				t.Fatalf("the channel travels as its name: %v", m)
			}
		}
		posted = append(posted, alertsOf(t, outs)...)
	}
	if strings.Join(posted, ",") != "probe:new:critical" {
		t.Fatalf("the alert posts once over three ticks: %v", posted)
	}
	for _, sink := range []map[string]any{{"webhook": []any{"w1"}}, {"webhook": "w1", "channel": map[string]any{"name": "ops"}}, {"webhook": "w1", "channel": true}} {
		bad := newPWHarness(t)
		bad.writeConfig(t, func(cfg map[string]any) { cfg["sinks"] = []map[string]any{sink} })
		_, stderr, err := runPyWhole(t, bad.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(bad, 60, 0, 5000), map[string]string{"grafana_token": bad.tokenFile}))
		if err == nil || strings.Contains(stderr, "Traceback") || !strings.Contains(stderr, "sink") {
			t.Fatalf("sink %v: plan refuses it by name: %v %s", sink, err, stderr)
		}
	}
}
