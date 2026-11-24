package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pwFullConfig writes the harness's config with every lane on (the default
// grafana/prometheus/probes plus Sentry) and, when days is non-empty, one
// session window around now. writeConfig rebuilds the whole file, so a test
// that needs the Sentry lane beside the session writes both here.
func pwFullConfig(t *testing.T, h *pwHarness, days []string, probes []map[string]any) {
	t.Helper()
	h.writeConfig(t, func(cfg map[string]any) {
		if probes != nil {
			cfg["prometheus"] = map[string]any{"probes": probes}
		}
		cfg["sentry"] = map[string]any{"base_url": h.srv.URL, "org": "org", "project": "proj", "environment": "preprod",
			"min_level": "error", "overlap_minutes": 60}
		if len(days) > 0 {
			now := time.Now().UTC()
			nowMin := now.Hour()*60 + now.Minute()
			from, to := nowMin-120, nowMin+120
			cfg["session"] = map[string]any{
				"active_windows": []map[string]any{{"days": days, "from": pwWindowHM(from), "to": pwWindowHM(to), "tz": "UTC"}},
				"heartbeat":      map[string]any{"metrics": []map[string]any{{"id": "rps", "title": "requests/s", "query": "rps-q", "unit": "req/s"}}},
			}
		}
	})
}

// pwWindowHM spells a minute-of-day count the way the config's windows do;
// past midnight it clamps to the day's last minute (the window covers now
// either way).
func pwWindowHM(mins int) string {
	if mins < 0 {
		mins = 0
	}
	if mins > 1439 {
		mins = 1439
	}
	return strconv.Itoa(mins/60) + ":" + pad2(mins%60)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// pwReadJSON reads one JSON object from the workspace.
func pwReadJSON(t *testing.T, h *pwHarness, rel string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.ws, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return m
}

// TestProdWatch_AWatchTickAdoptsTheSessionsMemory: the first watch tick
// after a session window adopts the fork's memory READ-ONLY (#2835) — the
// breach the session escalated and the Sentry issue it announced are not
// news again; the session's files stay the session's; the adoption is
// stamped on the watch state, once per session.
func TestProdWatch_AWatchTickAdoptsTheSessionsMemory(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	pwFullConfig(t, h, nil, pwRestartsProbe)
	// The watch is established and quiet: cursors armed, state on disk.
	h.prom.Store(map[string]pwProm{"rps-q": {Value: "12"}, "restarts-q": {Value: "0"}})
	h.tick(t, wf, false)
	pwFullConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	// The session's window: a breach and a new Sentry issue, announced once.
	h.prom.Store(map[string]pwProm{"rps-q": {Value: "14"}, "restarts-q": {Value: "3"}})
	h.sentry.put(&pwSentryIssue{ID: "301", ShortID: strp("PROJ-9"), Title: "TypeError: during the window",
		FirstProcessed: time.Now().Add(-time.Minute), LastSeen: time.Now(), Count: 2})
	before := int(h.sinkHits.Load())
	outs := h.modeTick(t, wf, "session", nil)
	if sess, _ := outs["plan"]["session"].(map[string]any); sess["new"] != true {
		t.Fatalf("the session must open on the window's first tick: %v", outs["plan"]["summary"])
	}
	if got := pwCount(h.bodies()[before:], "metric threshold breached"); got != 1 {
		t.Fatalf("the session must escalate the breach once, got %d:\n%s", got, strings.Join(h.bodies()[before:], "\n---\n"))
	}
	if got := pwCount(h.bodies()[before:], "PROJ-9"); got != 1 {
		t.Fatalf("the session must announce the issue once, got %d", got)
	}
	// The session's memory, as the window leaves it — snapshotted after the
	// window's config and envelope are aged (the test's own writes), so the
	// comparison below judges the WATCH tick's writes only.
	pwFullConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	h.editSession(t, func(env map[string]any) {
		env["ends_at"] = time.Now().UTC().Add(-23 * time.Minute).Format(time.RFC3339)
	})
	forkBefore, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "session", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	envelopeBefore, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	outs = h.tick(t, wf, false)
	if outs["plan"]["step_back"] == true {
		t.Fatalf("the watch tick must resume past the window: %v", outs["plan"]["summary"])
	}
	// The adoption: NEITHER signal is news again.
	if got := pwCount(h.bodies()[before:], "metric threshold breached"); got != 1 {
		t.Fatalf("the watch must not re-announce the breach the session escalated, got %d:\n%s", got, strings.Join(h.bodies()[before:], "\n---\n"))
	}
	if got := pwCount(h.bodies()[before:], "PROJ-9"); got != 1 {
		t.Fatalf("the watch must not re-announce the Sentry issue the session announced, got %d", got)
	}
	st := h.state(t)
	if marker, _ := st["adopted_session"].(string); marker == "" {
		t.Fatalf("the watch tick after the window must stamp the adoption marker, state has %v", st["adopted_session"])
	}
	inc, _ := st["incidents"].(map[string]any)
	for fp := range map[string]bool{"prom:restarts": true, "sentry:301": true} {
		rec, _ := inc[fp].(map[string]any)
		if rec == nil || rec["alerted"] != true {
			t.Fatalf("the adopted state lacks the session's alerted incident %s: %v", fp, rec)
		}
	}
	// The generation moved on (the fork's records' stamps must stay valid
	// for plan's next read of the adopted state).
	if gen, _ := st["generation"].(float64); gen < 2 {
		t.Fatalf("the adopted state carries no fresh generation: %v", st["generation"])
	}
	// Read-only: the session's files are byte-for-byte what the window left.
	forkAfter, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "session", "state.json"))
	if err != nil || string(forkAfter) != string(forkBefore) {
		t.Fatalf("the watch tick wrote the session's state (%v)", err)
	}
	envelopeAfter, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"))
	if err != nil || string(envelopeAfter) != string(envelopeBefore) {
		t.Fatalf("the watch tick wrote the session's envelope (%v)", err)
	}
	// Once per session: the NEXT watch tick adopts nothing and posts nothing.
	hits := h.sinkHits.Load()
	h.tick(t, wf, false)
	if h.sinkHits.Load() != hits {
		t.Fatalf("a steady watch tick after the adoption posted")
	}
}

// TestProdWatch_TheAdoptionKeepsTheSessionsFrontier: with the Loki and
// Sentry lanes dark on the adoption tick, the watch's cursors come out of
// it as the session's (#2835) — the next healthy tick reads from the
// session's frontier instead of replaying the window's lines, and the
// template the session announced does not fire again.
func TestProdWatch_TheAdoptionKeepsTheSessionsFrontier(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	pwFullConfig(t, h, nil, pwRestartsProbe)
	h.prom.Store(map[string]pwProm{"rps-q": {Value: "12"}, "restarts-q": {Value: "0"}})
	// The quiet watch tick arms the cursors at T0.
	h.tick(t, wf, false)
	t0 := h.state(t)
	wCur, _ := t0["cursors"].(map[string]any)["loki"].(map[string]any)["errors"].(map[string]any)
	wCov, _ := strconv.ParseInt(wCur["covered_to_ns"].(string), 10, 64)
	// The window: error lines land inside it — just ABOVE the watch's armed
	// cursor (below it they are the walk's history, above the poll's end
	// they are its future) — and the session reads them, announcing the
	// template once. The watch's cursor is aged ten minutes first, so the
	// session's frontier is meaningfully ahead of it (a gap the 60 s
	// overlap cannot bridge: "the window was not replayed" stays decidable).
	st0 := pwReadJSON(t, h, ".prod-watch/state.json")
	cur0 := st0["cursors"].(map[string]any)["loki"].(map[string]any)["errors"].(map[string]any)
	// The three cursor fields age together: an overlap_from above the
	// covered mark is an inverted cursor, and its bound would dominate the
	// next walk's window.
	cur0["covered_to_ns"] = strconv.FormatInt(wCov-600e9, 10)
	cur0["frontier_ns"] = strconv.FormatInt(wCov-600e9, 10)
	cur0["overlap_from_ns"] = strconv.FormatInt(wCov-600e9, 10)
	b0, err := json.MarshalIndent(st0, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), b0, 0o644); err != nil {
		t.Fatal(err)
	}
	wCov -= 600e9
	pwFullConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	h.lines.Store([]pwLine{
		{TS: wCov + 200e6, Line: "zedflash connection refused for order 41", Container: "app", Q: "errors-q"},
		{TS: wCov + 300e6, Line: "zedflash connection refused for order 42", Container: "app", Q: "errors-q"},
		{TS: wCov + 400e6, Line: "zedflash connection refused for order 43", Container: "app", Q: "errors-q"},
	})
	before := int(h.sinkHits.Load())
	h.modeTick(t, wf, "session", nil)
	if got := pwCount(h.bodies()[before:], "zedflash"); got != 1 {
		t.Fatalf("the session must announce the new template once, got %d:\n%s", got, strings.Join(h.bodies()[before:], "\n---\n"))
	}
	fork := pwReadJSON(t, h, ".prod-watch/session/state.json")
	forkCursors, _ := fork["cursors"].(map[string]any)["loki"].(map[string]any)
	forkErr, _ := forkCursors["errors"].(map[string]any)
	sessionCov, _ := strconv.ParseInt(forkErr["covered_to_ns"].(string), 10, 64)
	if sessionCov <= wCov {
		t.Fatalf("the session's frontier (%d) must be ahead of the watch's (%d)", sessionCov, wCov)
	}
	forkSentry, _ := fork["cursors"].(map[string]any)["sentry"].(map[string]any)
	sessionSince, _ := forkSentry["since"].(string)
	if sessionSince == "" {
		t.Fatalf("the session's Sentry cursor carries no since: %v", fork["cursors"])
	}
	// The window is over; the adoption tick runs with Loki and Sentry dark:
	// nothing the lanes read this tick may move the adopted cursors back.
	pwFullConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	h.editSession(t, func(env map[string]any) {
		env["ends_at"] = time.Now().UTC().Add(-23 * time.Minute).Format(time.RFC3339)
	})
	h.callMu.Lock()
	failFrom := int64(len(h.lokiCalls)) + 1
	h.callMu.Unlock()
	h.failLokiFrom.Store(failFrom)
	h.failLokiCount.Store(12)
	// The walk retries a 5xx once: the lane stays dark only if every list
	// answer of its budget fails.
	h.sentry.failNext("list", 500, 500, 500)
	outs := h.tick(t, wf, false)
	if outs["plan"]["step_back"] == true {
		t.Fatalf("the watch tick must resume past the window: %v", outs["plan"]["summary"])
	}
	st := h.state(t)
	gotCursors, _ := st["cursors"].(map[string]any)["loki"].(map[string]any)
	gotErr, _ := gotCursors["errors"].(map[string]any)
	gotCov, _ := strconv.ParseInt(gotErr["covered_to_ns"].(string), 10, 64)
	if gotCov != sessionCov {
		t.Fatalf("the watch's Loki cursor after the adoption must be the session's frontier %d, got %d", sessionCov, gotCov)
	}
	gotSentry, _ := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	if gotSentry["since"] != sessionSince {
		t.Fatalf("the watch's Sentry since after the adoption must be the session's %v, got %v", sessionSince, gotSentry["since"])
	}
	// The template incident the session minted travelled with the memory.
	hasTpl := false
	for fp := range st["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "loki:") {
			hasTpl = true
		}
	}
	if !hasTpl {
		t.Fatalf("the adopted state lacks the session's template incident: %v", st["incidents"])
	}
	// The next healthy tick reads FROM the session's frontier — the window's
	// lines are behind it (the band knows them), the template stays said.
	h.failLokiFrom.Store(0)
	h.failLokiCount.Store(0)
	hits := h.sinkHits.Load()
	h.tick(t, wf, false)
	if h.sinkHits.Load() != hits {
		t.Fatalf("the tick after the adoption re-announced the window's template:\n%s", strings.Join(h.bodies()[before:], "\n---\n"))
	}
	h.callMu.Lock()
	var from int64
	for _, q := range h.lokiCalls {
		if q.Get("query") == "errors-q" {
			from, _ = strconv.ParseInt(q.Get("start"), 10, 64)
		}
	}
	h.callMu.Unlock()
	// The walk opened at the frontier less the overlap (60 s) — never back
	// at the watch's pre-window cursor: the window is not replayed.
	if from < sessionCov-61e9 || from <= wCov {
		t.Fatalf("the healthy tick re-read the window: its walk started at %d, the session frontier is %d, the watch's old cursor %d", from, sessionCov, wCov)
	}
}

// TestProdWatch_TheAdoptionNeverWritesACursorThePlanRefuses: the fork is a
// file this mode does not own and reads leniently — a cursor that fails the
// shapes plan judges a state by is never let into the watch state (#2835).
// A hand-edited or older-writer fork whose Sentry `at` lost its offset (the
// adoption guard types `since` and the identity only) must not land the
// plan-refused empty string: the adoption falls back to the `since` it is
// guarded by, the tick commits, and the next tick still runs.
func TestProdWatch_TheAdoptionNeverWritesACursorThePlanRefuses(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t) // default config: the Sentry lane dark, decide writes no cursor of its own
	sd := filepath.Join(h.ws, ".prod-watch", "session")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	// The envelope and the fork, hand-written: exactly the foreign file the
	// lenient reader exists for. `since` and `armed_at` carry their offsets,
	// `at` does not — the naive stamp plan refuses for cursors.sentry.at.
	if err := os.WriteFile(filepath.Join(sd, "session.json"),
		[]byte(`{"id":"env-1","ends_at":"2026-10-31T09:59:00+00:00","closed":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sd, "state.json"),
		[]byte(`{"version":1,"generation":7,"cursors":{"sentry":{"identity":{"base_url":"https://sentry.example","org":"org","project":"proj","environment":"preprod"},`+
			`"since":"2026-10-31T09:00:00+00:00","armed_at":"2026-10-31T09:00:00+00:00","at":"2026-10-31T09:30:00"}},"incidents":{},"health":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h.tick(t, wf, false)
	st := h.state(t)
	cur, _ := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	if cur == nil {
		t.Fatalf("the adoption tick must land the fork's Sentry cursor: %v", st["cursors"])
	}
	if cur["since"] != "2026-10-31T09:00:00+00:00" {
		t.Fatalf("the adopted cursor lost the fork's since: %v", cur)
	}
	at, _ := cur["at"].(string)
	if at == "" {
		t.Fatalf("the adoption wrote the plan-refused empty at — the next tick refuses the state it just wrote: %v", cur)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("the adopted cursor's at %q is not an offset-ISO stamp: %v", at, err)
	}
	// The state the adoption wrote must not brick the watchdog: the next
	// tick runs (h.tick fails the test on plan's refusal).
	h.tick(t, wf, false)
}
