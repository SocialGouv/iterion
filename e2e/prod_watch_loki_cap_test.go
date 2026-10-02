package e2e

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// lokiCapState: the state decide reads, with a generation already advanced by
// n decides, so multi-tick chains are explicit.
func lokiCapState(incidents map[string]any, gen int) map[string]any {
	st := pwLokiState(incidents)
	st["generation"] = gen
	return st
}

func lokiStore(t *testing.T, out map[string]any) []string {
	t.Helper()
	inc, _ := pwStateNext(t, out)["incidents"].(map[string]any)
	var ids []string
	for fp, r := range inc {
		if m, _ := r.(map[string]any); m["kind"] == "loki" {
			ids = append(ids, strings.TrimPrefix(fp, "loki:"))
		}
	}
	sort.Strings(ids)
	return ids
}

func lokiMinted(from, n int) []any {
	var tpls []any
	for k := 0; k < n; k++ {
		id := fmt.Sprintf("t%02d", from+k)
		tpls = append(tpls, map[string]any{"template_id": id, "template": "ERROR minted pattern " + id, "count": 3, "count_live": 3,
			"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
			"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"})
	}
	return tpls
}

func lokiReasons(out map[string]any) string {
	ss, _ := out["stale_sources"].([]any)
	for _, s := range ss {
		m := s.(map[string]any)
		if m["source"] == "coverage" {
			return fmt.Sprint(m["reasons"])
		}
	}
	return ""
}

// TestProdWatch_LokiCapCutsAMintedFloodDownToTheCap: minted templates the last
// ticks read are protected, then the oldest admissions go down to the cap —
// the cut said once in the coverage note.
func TestProdWatch_LokiCapCutsAMintedFloodDownToTheCap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	in := map[string]any{"loki": map[string]any{"max_records": 6}}
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 8), "leak": []any{}}, lokiCapState(map[string]any{}, 1), in)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if n := len(lokiStore(t, out)); n != 8 {
		t.Fatalf("the tick that read 8 templates kept %d", n)
	}
	// The next ticks read fresh minted templates; t00-t07 go unread.
	for _, gen := range []int{3, 4} {
		out, _, err = pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(20, 8), "leak": []any{}}, lokiCapState(pwStateNext(t, out)["incidents"].(map[string]any), gen), in)
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
	}
	reasonsCutTick := lokiReasons(out)
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(20, 8), "leak": []any{}}, lokiCapState(pwStateNext(t, out)["incidents"].(map[string]any), 5), in)
	if err != nil {
		t.Fatalf("tick 5: %v", err)
	}
	got := lokiStore(t, out)
	for _, id := range []string{"00", "01", "02", "03", "04", "05", "06", "07"} {
		if strings.Contains(","+strings.Join(got, ",")+",", ","+id+",") {
			t.Fatalf("the oldest admissions (t%s, unread for 3 generations) must go first: got %v", id, got)
		}
	}
	if len(got) != 8 {
		t.Fatalf("want the 8 protected fresh reads left, got %v", got)
	}
	if r := reasonsCutTick; !strings.Contains(r, "loki: ") || !strings.Contains(r, "record(s) cut past max_records") {
		t.Fatalf("the cut was not said in the coverage note of the cutting tick: %q", r)
	}
	_ = got
	if rc, carried := pwStateNext(t, out)["records_cut"]; carried {
		t.Fatalf("a cut the note said is still carried: %v", rc)
	}
}

// TestProdWatch_LokiCapSparesAPendingTemplate: a template alert the per-run cap
// deferred is owed — its record outlives the cut, although nothing reads it.
func TestProdWatch_LokiCapSparesAPendingTemplate(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	in := map[string]any{"loki": map[string]any{"max_records": 3}, "max_alerts": 1}
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 6), "leak": []any{}}, lokiCapState(map[string]any{}, 1), in)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	st := pwStateNext(t, out)
	var pending []string
	for fp, r := range st["incidents"].(map[string]any) {
		if m := r.(map[string]any); m["pending"] == "new" {
			pending = append(pending, strings.TrimPrefix(fp, "loki:"))
		}
	}
	if len(pending) != 4 {
		t.Fatalf("setup: want 4 templates pending after a per-run cap of 1, got %v", pending)
	}
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}}, lokiCapState(st["incidents"].(map[string]any), 3), in)
	if err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}}, lokiCapState(pwStateNext(t, out)["incidents"].(map[string]any), 4), in)
	if err != nil {
		t.Fatalf("tick 4: %v", err)
	}
	// The pendings still standing just before the deciding tick — some may
	// have been posted along the way, and a posted template may go.
	st = pwStateNext(t, out)
	pending = pending[:0]
	for fp, r := range st["incidents"].(map[string]any) {
		if m := r.(map[string]any); m["pending"] == "new" {
			pending = append(pending, strings.TrimPrefix(fp, "loki:"))
		}
	}
	if len(pending) == 0 {
		t.Fatal("setup: no pending template left to protect")
	}
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}}, lokiCapState(st["incidents"].(map[string]any), 5), in)
	if err != nil {
		t.Fatalf("tick 5: %v", err)
	}
	for _, id := range pending {
		found := false
		for _, got := range lokiStore(t, out) {
			found = found || got == id
		}
		if !found {
			t.Fatalf("the record of a pending template was cut (store %v)", lokiStore(t, out))
		}
	}
}

// TestProdWatch_LokiCapSparesAPostedAloneTemplateForItsWindow: a template whose
// new alert posted alone is kept past the cap within its window — a window
// longer than the read protection, so this witness isolates it.
func TestProdWatch_LokiCapSparesAPostedAloneTemplateForItsWindow(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(1)
	in := map[string]any{"loki": map[string]any{"max_records": 100}}
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 1), "leak": []any{}}, lokiCapState(map[string]any{}, 1), in)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if w, _ := pwStateNext(t, out)["incidents"].(map[string]any)["loki:t00"].(map[string]any)["alone_until_gen"].(float64); int(w) != 12 {
		t.Fatalf("setup: the window is 100 / (2 × 5) = 10 generations from generation 2: got %v", w)
	}
	// Four ticks that never read t00 again, the store over its cap: the alone
	// window (10 generations) is the only protection left (the read
	// protection covers 3).
	for _, gen := range []int{3, 4, 5, 6} {
		out, _, err = pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(30+10*gen, 6), "leak": []any{}},
			lokiCapState(pwStateNext(t, out)["incidents"].(map[string]any), gen), in)
		if err != nil {
			t.Fatalf("tick %d: %v", gen, err)
		}
	}
	found := false
	for _, got := range lokiStore(t, out) {
		found = found || got == "t00"
	}
	if !found {
		t.Fatalf("the template posted alone was cut inside its 50-generation window (store %d)", len(lokiStore(t, out)))
	}
}

// TestProdWatch_LokiCapCutsLegacyFirst: a pre-upgrade loki record (no admission
// stamp) goes before any stamped one, alerted or not.
func TestProdWatch_LokiCapCutsLegacyFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	legacyBacklog := map[string]any{"fp": "loki:zz", "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
		"title_key": "loki_template", "detail_key": "loki_detail", "first_seen": "2026-09-01T00:00:00+00:00",
		"last_seen": "2026-09-30T00:00:00+00:00", "count": 1, "alerted": false}
	legacyAlerted := map[string]any{"fp": "loki:yy", "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
		"title_key": "loki_template", "detail_key": "loki_detail", "first_seen": "2026-09-01T00:00:00+00:00",
		"last_seen": "2026-09-30T00:00:00+00:00", "count": 1, "alerted": true}
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 3), "leak": []any{}},
		lokiCapState(map[string]any{"loki:zz": legacyBacklog, "loki:yy": legacyAlerted}, 1),
		map[string]any{"loki": map[string]any{"max_records": 4}, "max_alerts_per_lane": 0})
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	// The minted ones are read this tick (protected); the legacy records are
	// not read and are the oldest admissions: the backlog one goes first —
	// not the lowest fingerprint (yy sorts before zz).
	st := pwStateNext(t, out)
	out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}},
		lokiCapState(st["incidents"].(map[string]any), 5),
		map[string]any{"loki": map[string]any{"max_records": 4}, "max_alerts_per_lane": 0})
	if err != nil {
		t.Fatalf("tick 5: %v", err)
	}
	if _, there := pwStateNext(t, out)["incidents"].(map[string]any)["loki:zz"]; there {
		t.Fatalf("the legacy backlog record was kept past the cap (store %d)", len(lokiStore(t, out)))
	}
	if _, there := pwStateNext(t, out)["incidents"].(map[string]any)["loki:yy"]; !there {
		t.Fatalf("the legacy alerted record must be kept over the cap before any stamped one (store %d)", len(lokiStore(t, out)))
	}
}

// TestProdWatch_LokiCapOffNeverCutsAndLeavesOtherKindsAlone: max_records 0
// never cuts; a leak class and a Sentry leak class are never the Loki cut's.
func TestProdWatch_LokiCapOffNeverCutsAndLeavesOtherKindsAlone(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	seeded := map[string]any{
		"loki:keep-leak": map[string]any{"fp": "loki:keep-leak", "kind": "leak", "sources": []any{"errors"},
			"severity": "high", "title_key": "leak", "detail_key": "leak_detail", "first_seen": "2026-09-30T00:00:00+00:00",
			"last_seen": "2026-09-30T00:00:00+00:00", "count": 1, "alerted": false},
	}
	in := map[string]any{"loki": map[string]any{"max_records": 0}, "max_alerts_per_lane": 0}
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 10), "leak": []any{}}, lokiCapState(seeded, 1), in)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	st := pwStateNext(t, out)
	if st["incidents"].(map[string]any)["loki:keep-leak"] == nil {
		t.Fatal("the leak class record was cut by the template store's cap")
	}
	if n := len(lokiStore(t, out)); n != 10 {
		t.Fatalf("setup: 10 minted templates kept with the cap off, got %d", n)
	}
	for k := 0; k < 4; k++ {
		st = pwStateNext(t, out)
		out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}}, lokiCapState(st["incidents"].(map[string]any), 3+k), in)
		if err != nil {
			t.Fatalf("tick %d: %v", 3+k, err)
		}
	}
	if n := len(lokiStore(t, out)); n != 10 {
		t.Fatalf("max_records 0 cut the store: %d of 10 left", n)
	}
	if r := lokiReasons(out); strings.Contains(r, "record(s) cut") {
		t.Fatalf("max_records 0 said a cut: %q", r)
	}
}

// TestProdWatch_LokiCapBothLanesCutInOneTick: the two minting lanes cut in the
// same tick and the coverage note names each — the per-lane pieces are generic.
func TestProdWatch_LokiCapBothLanesCutInOneTick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	seeded := map[string]any{}
	for _, id := range []string{"70", "71", "72", "73"} {
		seeded["sentry:"+id] = map[string]any{"fp": "sentry:" + id, "kind": "sentry", "sources": []any{"sentry"}, "severity": "medium",
			"title_key": "sentry_issue", "detail_key": "sentry_detail_short", "first_seen": "2026-09-30T00:00:00+00:00",
			"last_seen": "2026-09-30T00:00:00+00:00", "count": 1, "alerted": false, "status": "",
			"admitted_gen": 1, "tracked_read_gen": 2}
	}
	in := map[string]any{"loki": map[string]any{"max_records": 2}, "sentry": map[string]any{"max_records": 2, "read_protect_ticks": 3, "enabled": true,
		"identity": map[string]any{"base_url": "https://s.example", "org": "o", "project": "p", "environment": ""}},
		"max_alerts_per_lane": 0, "lanes": map[string]any{"loki": true, "sentry": true}, "sentry_issues": 0,
		"sentry_ok": true, "sentry_truncated": false, "sentry_errors": []any{},
		"sentry_walk": map[string]any{"answered": true, "new_complete": true, "transition_complete": true,
			"tracked_complete": true, "tracked_requested": []any{}}}
	// The first tick reads the four minted templates (protected) while the
	// seeded sentry records are read too (their stamp says generation 2).
	out, _, err := pwDecide(t, wf, h, map[string]any{"templates": lokiMinted(0, 4), "leak": []any{},
		"sentry_issues": []any{}}, lokiCapState(seeded, 3), in)
	if err != nil {
		t.Fatalf("decide 1: %v", err)
	}
	// Three quiet ticks age every read past the protection; both lanes then
	// cut (the sentry one first, its records are older), and every cut is
	// said in the coverage note of the tick that made it.
	seen := ""
	for _, gen := range []int{4, 5, 6} {
		out, _, err = pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}, "sentry_issues": []any{}},
			lokiCapState(pwStateNext(t, out)["incidents"].(map[string]any), gen), in)
		if err != nil {
			t.Fatalf("decide %d: %v", gen, err)
		}
		seen += " " + lokiReasons(out)
	}
	if !strings.Contains(seen, "sentry: ") || !strings.Contains(seen, "record(s) cut past max_records") || !strings.Contains(seen, "loki: ") {
		t.Fatalf("both lanes must say their cut across the quiet ticks: %q", seen)
	}
}
