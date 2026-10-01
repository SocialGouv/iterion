package e2e

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// capConfig: a Sentry-only config whose incident store holds n records, the
// by-id reads set to tracked (0 turns them off: only the lists read an issue).
func capConfig(h *pwHarness, n, tracked int, more func(s map[string]any)) func(cfg map[string]any) {
	return sentryOnly(h, func(s map[string]any) {
		s["max_records"] = n
		s["max_tracked"] = tracked
		if more != nil {
			more(s)
		}
	})
}

// capPut adds issues first processed now: the next tick's new list reads them.
func capPut(h *pwHarness, level string, ids ...string) {
	now := time.Now()
	for _, id := range ids {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("CAP-" + id), Title: "cap " + id, Level: level,
			FirstProcessed: now, LastSeen: now, Count: 1})
	}
}

// capAge takes issues out of every list a tick reads: first processed two days
// ago, out of the new list's window, and in no transition.
func capAge(h *pwHarness, ids ...string) {
	for _, id := range ids {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = time.Now().Add(-48 * time.Hour) })
	}
}

func capIDs(from, n int) []string {
	ids := make([]string, n)
	for k := range ids {
		ids[k] = fmt.Sprint(from + k)
	}
	return ids
}

// capStore returns the ids of the state's Sentry records, sorted.
func capStore(t *testing.T, h *pwHarness) []string {
	t.Helper()
	inc, _ := h.state(t)["incidents"].(map[string]any)
	var ids []string
	for fp, r := range inc {
		if m, _ := r.(map[string]any); m["kind"] == "sentry" {
			ids = append(ids, strings.TrimPrefix(fp, "sentry:"))
		}
	}
	sort.Strings(ids)
	return ids
}

func capHolds(t *testing.T, h *pwHarness, id string) bool {
	t.Helper()
	return sentryIncident(t, h, id) != nil
}

func capGen(t *testing.T, h *pwHarness) int {
	t.Helper()
	g, _ := h.state(t)["generation"].(float64)
	return int(g)
}

// capFlood arms the lane, then records 30 folded issues on the second tick and
// takes them out of every list: they are read last at the returned generation.
func capFlood(t *testing.T, h *pwHarness, wf *ir.Workflow, cap int) (junk []string, readGen int) {
	t.Helper()
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, cap, 0, nil))
	sentryTick(t, h, wf)
	junk = capIDs(7001, 30)
	capPut(h, "error", junk...)
	sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 30 {
		t.Fatalf("setup: the tick that read 30 issues recorded %d", n)
	}
	capAge(h, junk...)
	return junk, capGen(t, h)
}

// TestProdWatch_SentryCapCutsAFloodDownToTheCap: a flood of folded issues past
// max_records stays while the last three ticks read it, then is cut down to the
// cap; the cut is said once in the coverage note, and the count, said, is not
// carried.
func TestProdWatch_SentryCapCutsAFloodDownToTheCap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	capFlood(t, h, wf, 20)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 30 {
		t.Fatalf("records read within the last 3 ticks were cut: %d left", n)
	}
	o := sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 20 {
		t.Fatalf("records unread for 3 ticks, 30 for max_records 20: want 20 left, got %d", n)
	}
	if r := sentryNoteReasons(o); !strings.Contains(r, "sentry: 10 record(s) cut past max_records") {
		t.Fatalf("the cut was not said in the coverage note: %q", r)
	}
	if rc, carried := h.state(t)["records_cut"]; carried {
		t.Fatalf("a cut the note said is still carried: %v", rc)
	}
	o = sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 20 {
		t.Fatalf("a store at its cap lost records with nothing new: %d left", n)
	}
	if r := sentryNoteReasons(o); strings.Contains(r, "record(s) cut") {
		t.Fatalf("a cut said once is said again: %q", r)
	}
}

// TestProdWatch_SentryCapSparesWhatTheLastThreeTicksRead: a tick blind to Sentry
// (both lists fail; the probe lane answers, so the tick is reported) records
// nothing and cuts nothing read within the last three generations; a record
// read three generations ago goes before a newer one. B is admitted first and
// read through the third tick, A after it, read once.
func TestProdWatch_SentryCapSparesWhatTheLastThreeTicksRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["max_records"] = 2; s["max_tracked"] = 0 }))
	sentryTick(t, h, wf)
	capPut(h, "error", "900", "901")
	sentryTick(t, h, wf)
	capPut(h, "error", "100", "101", "102")
	sentryTick(t, h, wf)
	capAge(h, "100", "101", "102")
	sentryTick(t, h, wf)
	capAge(h, "900", "901")
	h.sentry.failNext("list", 500, 500, 500, 500)
	o := sentryTick(t, h, wf)
	if errs, _ := o["poll_sentry"]["errors"].([]any); len(errs) == 0 {
		t.Fatal("setup: the blind tick read its lists")
	}
	if got := capStore(t, h); strings.Join(got, ",") != "100,101,102,900,901" {
		t.Fatalf("a blind tick cut what the last ticks read, or recorded something: %v", got)
	}
	sentryTick(t, h, wf)
	if got := capStore(t, h); strings.Join(got, ",") != "900,901" {
		t.Fatalf("A unread for 3 generations, B read 2 ago, max_records 2: want B left, got %v", got)
	}
	capPut(h, "error", "300")
	sentryTick(t, h, wf)
	if got := capStore(t, h); strings.Join(got, ",") != "300,901" {
		t.Fatalf("B read 3 generations ago is the oldest admission past the cap: want 900 cut, got %v", got)
	}
}

// TestProdWatch_SentryCapSparesThePlannedByIdRead: an issue planned for this
// tick's by-id read is spared when that read fails — nothing else read it for
// three generations, and it is the oldest admission past the cap.
func TestProdWatch_SentryCapSparesThePlannedByIdRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 1, nil))
	sentryTick(t, h, wf)
	capPut(h, "error", "300")
	sentryTick(t, h, wf)
	capAge(h, "300")
	h.sentry.failNext("tracked", 500, 500, 500, 500, 500, 500) // the by-id read fails 3 ticks running (2 tries a tick)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	capPut(h, "error", "400", "401")
	o := sentryTick(t, h, wf)
	if errs, _ := o["poll_sentry"]["errors"].([]any); len(errs) == 0 {
		t.Fatal("setup: the by-id read did not fail")
	}
	if !capHolds(t, h, "300") {
		t.Fatalf("the issue planned for a failed by-id read was cut (store %v)", capStore(t, h))
	}
	if r := sentryNoteReasons(o); !strings.Contains(r, "3 records kept for max_records 2, every one protected: 3 read in the last 3 ticks") {
		t.Fatalf("a store kept over its cap by protected records: want it said, got %q", r)
	}
}

// TestProdWatch_SentryCapKeepsTheBootstrapBacklog: what the bootstrap reads is
// read — kept past the cap that tick — so the next tick's overlap does not post
// the backlog as NEW.
func TestProdWatch_SentryCapKeepsTheBootstrapBacklog(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 0, nil))
	capPut(h, "error", capIDs(600, 5)...)
	sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 5 {
		t.Fatalf("the bootstrap's backlog was cut the tick it was read: %d of 5 left", n)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("backlog read again by the overlap posted as news: %v", got)
	}
}

// TestProdWatch_SentryCapSparesAPendingAlert: an alert the per-run cap holds
// pending is owed — its record outlives the cut, although it is the oldest
// admission and nothing reads it any more.
func TestProdWatch_SentryCapSparesAPendingAlert(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.alertCap.Store(1)
	h.writeConfig(t, capConfig(h, 3, 0, nil))
	sentryTick(t, h, wf)
	fatal, folded := capIDs(5001, 5), capIDs(6001, 6)
	capPut(h, "fatal", fatal...)
	capPut(h, "error", folded...)
	sentryTick(t, h, wf)
	capAge(h, append(fatal, folded...)...)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	var pending []string
	for _, id := range fatal {
		if rec := sentryIncident(t, h, id); rec != nil && rec["pending"] != nil {
			pending = append(pending, id)
		}
	}
	if len(pending) != 2 {
		t.Fatalf("setup: want 2 alerts still pending after 3 ticks of a per-run cap of 1, got %v", pending)
	}
	sentryTick(t, h, wf)
	for _, id := range pending {
		if !capHolds(t, h, id) {
			t.Fatalf("the record of a pending alert was cut (store %v)", capStore(t, h))
		}
	}
	if n := len(capStore(t, h)); n != 3 {
		t.Fatalf("want the store cut to max_records 3, got %d", n)
	}
}

// TestProdWatch_SentryCapCutsAnOwedClosingNote: a closing note is a follow-up —
// resolved issues whose notes wait their turn (one note of its kind a tick) are
// cut past the cap like any record nothing protects, and the cut is said (a
// hostile server answering closed statuses must not grow the store for ever).
func TestProdWatch_SentryCapCutsAnOwedClosingNote(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.msgChars.Store(1500) // a 500-character names budget: about 25 long short ids a note
	h.writeConfig(t, capConfig(h, 10, 100, nil))
	sentryTick(t, h, wf)
	ids := capIDs(8001, 100)
	now := time.Now()
	for _, id := range ids {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("CLOSING-OWED-" + id), Title: "c", Level: "error",
			FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	for _, id := range ids {
		h.sentry.edit(id, func(i *pwSentryIssue) {
			i.Status = "resolved"
			i.FirstProcessed = time.Now().Add(-48 * time.Hour)
		})
	}
	sentryTick(t, h, wf)
	h.writeConfig(t, capConfig(h, 10, 0, nil))
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	var owed []string
	for _, id := range ids {
		if rec := sentryIncident(t, h, id); rec != nil && rec["quiet_noted"] != true {
			owed = append(owed, id)
		}
	}
	if len(owed) == 0 || len(owed) == len(ids) {
		t.Fatalf("setup: want some closing notes said and some still owed, got %d owed of %d", len(owed), len(ids))
	}
	o := sentryTick(t, h, wf)
	if n := len(capStore(t, h)); n != 10 {
		t.Fatalf("records owed only a closing note, unread for 3 generations: want the store cut to max_records 10, got %d (%d owed before)", n, len(owed))
	}
	if r := sentryNoteReasons(o); !strings.Contains(r, "record(s) cut past max_records") {
		t.Fatalf("the cut of owed closing notes was not said: %q", r)
	}
}

// TestProdWatch_SentryCapSparesAPostedAloneIssueForItsWindow: an issue posted
// alone outlives the cut for its window (max_records / (2 × the per-tick cap)
// generations after that post), then goes first; a fold member has no window.
func TestProdWatch_SentryCapSparesAPostedAloneIssueForItsWindow(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(1)
	h.writeConfig(t, capConfig(h, 10, 0, nil)) // window: 10 / (2 × 1) = 5 generations
	sentryTick(t, h, wf)
	folded := capIDs(201, 12)
	capPut(h, "fatal", "100")
	capPut(h, "error", folded...)
	sentryTick(t, h, wf)
	posted := capGen(t, h)
	if w, _ := sentryIncident(t, h, "100")["alone_until_gen"].(float64); int(w) != posted+5 {
		t.Fatalf("the issue posted alone at generation %d: want its window until %d, got %v", posted, posted+5, sentryIncident(t, h, "100")["alone_until_gen"])
	}
	if w, has := sentryIncident(t, h, "201")["alone_until_gen"]; has {
		t.Fatalf("a fold member got a window: %v", w)
	}
	capAge(h, append([]string{"100"}, folded...)...)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf) // read 3 generations ago: only the window spares it
	if !capHolds(t, h, "100") {
		t.Fatalf("the issue posted alone was cut inside its window (store %v)", capStore(t, h))
	}
	capPut(h, "error", "401", "402")
	sentryTick(t, h, wf) // posted + 4
	if !capHolds(t, h, "100") {
		t.Fatalf("the issue posted alone was cut on the last generation of its window (store %v)", capStore(t, h))
	}
	capPut(h, "error", "403", "404")
	sentryTick(t, h, wf) // posted + 5: the window is over
	if capHolds(t, h, "100") {
		t.Fatalf("the window outlived posted + 5 generations: the oldest admission past the cap was kept (store %v)", capStore(t, h))
	}
}

// TestProdWatch_SentryCapWindowOpensOnceAndNotForAReminder: the window opens
// on the first fact posted alone and never moves — a regression posted alone
// later leaves it where it was — and a reminder posted alone opens none.
func TestProdWatch_SentryCapWindowOpensOnceAndNotForAReminder(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(1)
	h.writeConfig(t, capConfig(h, 10, 0, nil))
	sentryTick(t, h, wf)
	capPut(h, "fatal", "150")
	capPut(h, "error", "171")
	sentryTick(t, h, wf)
	first, _ := sentryIncident(t, h, "150")["alone_until_gen"].(float64)
	if first == 0 {
		t.Fatal("setup: the issue posted alone has no window")
	}
	if _, has := sentryIncident(t, h, "171")["alone_until_gen"]; has {
		t.Fatal("setup: the folded issue has a window")
	}
	now := time.Now()
	h.sentry.edit("150", func(i *pwSentryIssue) {
		i.Substatus = strp("regressed")
		i.LastSeen = now
		i.Acts = []pwSentryAct{{Type: "set_regression", At: now}}
	})
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); !eqStrings(got, []string{"regressed:CAP-150:high"}) {
		t.Fatalf("setup: want the regression posted alone, got %v", got)
	}
	if w, _ := sentryIncident(t, h, "150")["alone_until_gen"].(float64); w != first {
		t.Fatalf("a second post alone moved the window: %v, was %v", w, first)
	}
	st := h.state(t)
	rec := st["incidents"].(map[string]any)["sentry:171"].(map[string]any)
	rec["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	h.sentry.edit("171", func(i *pwSentryIssue) { i.LastSeen = time.Now(); i.Count = 2 })
	o = sentryTick(t, h, wf)
	if got := sentryAlerts(o); !eqStrings(got, []string{"reminder:CAP-171:medium"}) {
		t.Fatalf("setup: want a reminder posted alone, got %v", got)
	}
	if w, has := sentryIncident(t, h, "171")["alone_until_gen"]; has {
		t.Fatalf("a reminder posted alone opened a window: %v", w)
	}
}

// TestProdWatch_SentryCapCutsTheOldestAdmissionFirst: past the cap the oldest
// admission goes first — not the lowest id (the fingerprint order disagrees).
func TestProdWatch_SentryCapCutsTheOldestAdmissionFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 0, nil))
	sentryTick(t, h, wf)
	capPut(h, "error", "900")
	sentryTick(t, h, wf)
	capAge(h, "900")
	capPut(h, "error", "100")
	sentryTick(t, h, wf)
	capAge(h, "100")
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	capPut(h, "error", "500")
	sentryTick(t, h, wf) // 900 read 4 generations ago, 100 read 3 ago: both cut-able, one goes
	if got := capStore(t, h); strings.Join(got, ",") != "100,500" {
		t.Fatalf("the oldest admission (900) must go first, not the lowest fingerprint: got %v", got)
	}
}

// TestProdWatch_SentryCapCutsLegacyBacklogFirst: a record from before the
// admission stamp goes before any stamped one (100 is stamped, its fingerprint
// the lowest), its backlog (779) before its alerted (778, the lower id).
func TestProdWatch_SentryCapCutsLegacyBacklogFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 3, 0, nil))
	capPut(h, "error", "779") // read by the bootstrap: backlog, never alerted
	sentryTick(t, h, wf)
	capPut(h, "error", "778", "100")
	sentryTick(t, h, wf) // posted in a note: alerted
	capAge(h, "779", "778", "100")
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	st := h.state(t)
	for _, id := range []string{"778", "779"} {
		delete(st["incidents"].(map[string]any)["sentry:"+id].(map[string]any), "admitted_gen")
	}
	h.setState(t, st)
	if r := sentryIncident(t, h, "778"); r["alerted"] != true || sentryIncident(t, h, "779")["alerted"] == true {
		t.Fatalf("setup: want 778 alerted and 779 backlog, got %v / %v", r["alerted"], sentryIncident(t, h, "779")["alerted"])
	}
	capPut(h, "error", "500")
	sentryTick(t, h, wf)
	if got := capStore(t, h); strings.Join(got, ",") != "100,500,778" {
		t.Fatalf("legacy backlog (779) must go first — before legacy alerted (778) and before any stamped record (100): got %v", got)
	}
}

// TestProdWatch_SentryCapOffNeverCuts: max_records 0 turns the cut off.
func TestProdWatch_SentryCapOffNeverCuts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	capFlood(t, h, wf, 0)
	for k := 0; k < 4; k++ {
		if r := sentryNoteReasons(sentryTick(t, h, wf)); strings.Contains(r, "record(s) cut") {
			t.Fatalf("max_records 0 said a cut: %q", r)
		}
	}
	if n := len(capStore(t, h)); n != 30 {
		t.Fatalf("max_records 0 cut the store: %d of 30 left", n)
	}
}

// TestProdWatch_SentryIdentityChangeDropsTheCutCount: another project or
// environment drops the old memory's carried cut count and its note stamps.
func TestProdWatch_SentryIdentityChangeDropsTheCutCount(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	capFlood(t, h, wf, 20)
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	st := h.state(t)
	if _, said := st["coverage_posted"].(map[string]any)["records cut: sentry"]; !said {
		t.Fatalf("setup: the cut was not said: %v", st["coverage_posted"])
	}
	st["records_cut"] = map[string]any{"sentry": 4}
	h.setState(t, st)
	h.sentry.envs["staging"] = true
	h.writeConfig(t, capConfig(h, 20, 0, func(s map[string]any) { s["environment"] = "staging" }))
	o := sentryTick(t, h, wf)
	st = h.state(t)
	if r := sentryNoteReasons(o); strings.Contains(r, "record(s) cut") {
		t.Fatalf("the old identity's cut count was said under the new one: %q", r)
	}
	if rc, carried := st["records_cut"]; carried {
		t.Fatalf("the old identity's cut count is carried: %v", rc)
	}
	if _, kept := st["coverage_posted"].(map[string]any)["records cut: sentry"]; kept {
		t.Fatalf("the old identity's note stamp would silence the new one's first cut: %v", st["coverage_posted"])
	}
}

// TestProdWatch_SentryARegressionPostsWhateverItsLevel: min_level decides what
// posts as NEW, nothing else — a regression below it posts, at its severity.
func TestProdWatch_SentryARegressionPostsWhateverItsLevel(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "41", ShortID: strp("CAP-41"), Title: "w", Level: "warning", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	h.sentry.put(&pwSentryIssue{ID: "42", ShortID: strp("CAP-42"), Title: "w", Level: "warning", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:CAP-41:low"}) {
		t.Fatalf("a regression below min_level posts, a NEW issue below it does not: got %v", got)
	}
}

// TestProdWatch_SentryCapRefusals: a malformed cap, or a store stamp of the
// wrong kind, is refused by name.
func TestProdWatch_SentryCapRefusals(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	vars := func(h *pwHarness) map[string]any {
		return map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	}
	for _, c := range []struct {
		name, key string
		v         any
	}{
		{"max_records negative", "max_records", -1},
		{"max_records bool", "max_records", true},
		{"max_records float", "max_records", 1.5},
		{"max_records a string", "max_records", "5000"},
		{"max_records past its bound", "max_records", 2_000_000_000},
		{"read_protect_ticks negative", "read_protect_ticks", -1},
		{"read_protect_ticks bool", "read_protect_ticks", true},
		{"read_protect_ticks float", "read_protect_ticks", 1.5},
		{"read_protect_ticks a string", "read_protect_ticks", "3"},
		{"read_protect_ticks past its bound", "read_protect_ticks", 2_000_000},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s[c.key] = c.v }))
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars(h), nil))
			if err == nil || !strings.Contains(stderr, "config.sentry."+c.key) {
				t.Fatalf("want a refusal naming config.sentry.%s, got err=%v stderr=%s", c.key, err, stderr)
			}
		})
	}
	t.Run("read_protect_ticks zero", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["read_protect_ticks"] = 0 }))
		_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars(h), nil))
		if err == nil || !strings.Contains(stderr, "config.sentry.read_protect_ticks") {
			t.Fatalf("want a refusal naming config.sentry.read_protect_ticks, got err=%v stderr=%s", err, stderr)
		}
	})
	for _, c := range []struct {
		name, want string
		mod        func(st map[string]any)
	}{
		{"admitted_gen not an integer", ".admitted_gen", func(st map[string]any) { capRec(st)["admitted_gen"] = "3" }},
		{"alone_until_gen past any window", ".alone_until_gen", func(st map[string]any) { capRec(st)["alone_until_gen"] = 2_000_000_000 }},
		{"records_cut not an object", "records_cut", func(st map[string]any) { st["records_cut"] = []any{1} }},
		{"records_cut negative", "records_cut", func(st map[string]any) { st["records_cut"] = map[string]any{"sentry": -1} }},
		{"records_cut float", "records_cut", func(st map[string]any) { st["records_cut"] = map[string]any{"sentry": 1.5} }},
		{"a watch stamp past the state generation", ".transition_seen_gen", func(st map[string]any) { capRec(st)["transition_seen_gen"] = 1_000_000 }},
		{"a read stamp past the state generation", ".tracked_read_gen", func(st map[string]any) { capRec(st)["tracked_read_gen"] = 1_000_000 }},
		{"a check stamp past the state generation", ".transition_checked_gen", func(st map[string]any) { capRec(st)["transition_checked_gen"] = 1_000_000 }},
		{"an admission past the state generation", ".admitted_gen", func(st map[string]any) { capRec(st)["admitted_gen"] = 1_000_000 }},
		{"a Sentry incident keyed without its id", "keyed 'sentry:<issue id>'", func(st map[string]any) {
			inc := st["incidents"].(map[string]any)
			inc["sentry-11"] = capRec(st)
		}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			capPut(h, "error", "11")
			sentryTick(t, h, wf)
			st := h.state(t)
			c.mod(st)
			h.setState(t, st)
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars(h), nil))
			if err == nil || !strings.Contains(stderr, c.want) {
				t.Fatalf("want a refusal naming %q, got err=%v stderr=%s", c.want, err, stderr)
			}
		})
	}
}

// capRec returns the first Sentry record of a state.
func capRec(st map[string]any) map[string]any {
	for fp, r := range st["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "sentry:") {
			return r.(map[string]any)
		}
	}
	return nil
}

// TestProdWatch_SentryCapCutSaidBesideAFullHistoryCarry: the cut is said with
// the losses, before the history names — 100 of them cannot push it out of the
// note's budget.
func TestProdWatch_SentryCapCutSaidBesideAFullHistoryCarry(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	capFlood(t, h, wf, 20)
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	st := h.state(t)
	var unsaid []any
	for k := 0; k < 100; k++ {
		unsaid = append(unsaid, fmt.Sprintf("HIST-%d@2026-09-30T10:00", k))
	}
	st["sentry_history_unsaid"] = unsaid
	h.setState(t, st)
	o := sentryTick(t, h, wf)
	if r := sentryNoteReasons(o); !strings.Contains(r, "record(s) cut past max_records") {
		t.Fatalf("100 history names pushed the cut out of the note: %q", r)
	}
}

// TestProdWatch_SentryCapLeavesOtherKindsAlone: the Sentry lane's cut counts and
// cuts Sentry issue records only — a log template, a Sentry leak class stay.
func TestProdWatch_SentryCapLeavesOtherKindsAlone(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	capFlood(t, h, wf, 20)
	st := h.state(t)
	now := time.Now().UTC().Format(time.RFC3339)
	inc := st["incidents"].(map[string]any)
	inc["loki:tpl1"] = map[string]any{"fp": "loki:tpl1", "kind": "loki", "sources": []any{"q"}, "severity": "medium",
		"title_key": "loki_template", "detail_key": "loki_detail", "first_seen": now, "last_seen": now, "count": 1, "alerted": false}
	inc["sentry_leak:email"] = map[string]any{"fp": "sentry_leak:email", "kind": "sentry_leak", "sources": []any{"sentry"},
		"severity": "high", "title_key": "sentry_leak", "detail_key": "sentry_leak_detail", "first_seen": now, "last_seen": now,
		"count": 1, "alerted": false}
	h.setState(t, st)
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	inc = h.state(t)["incidents"].(map[string]any)
	for _, fp := range []string{"loki:tpl1", "sentry_leak:email"} {
		if inc[fp] == nil {
			t.Fatalf("the Sentry lane's cut took a record of another kind: %s", fp)
		}
	}
	if n := len(capStore(t, h)); n != 20 {
		t.Fatalf("setup: want the Sentry records cut to 20, got %d", n)
	}
}

// TestProdWatch_SentryCapBoundsABootstrapThatNeverArms: a lane whose new list
// is never read whole never arms — every tick is a bootstrap — and the cut
// still bounds what those bootstraps record.
func TestProdWatch_SentryCapBoundsABootstrapThatNeverArms(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.sentry.pageSize = 1
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 4, 0, func(s map[string]any) { s["max_issues"] = 1 }))
	said := false
	for k := 0; k < 10; k++ {
		capPut(h, "error", capIDs(9000+10*k, 3)...)
		said = strings.Contains(sentryNoteReasons(sentryTick(t, h, wf)), "record(s) cut") || said
	}
	if cur, _ := h.state(t)["cursors"].(map[string]any); cur["sentry"] != nil {
		t.Fatalf("setup: the lane armed: %v", cur["sentry"])
	}
	if n := len(capStore(t, h)); n > 4+3 {
		t.Fatalf("a bootstrap that never arms grew the store past max_records + 3 ticks of reads: %d", n)
	}
	if !said {
		t.Fatal("the bootstraps' cut was never said")
	}
}

// TestProdWatch_SentryCapAnnouncesARegressionOfAnAnnouncedIssue: the watch
// owes a first announcement for the EVENT, not the record — an issue already
// announced for its NEW has its later, undated, never-checked regression
// announced before the cut reaches it.
func TestProdWatch_SentryCapAnnouncesARegressionOfAnAnnouncedIssue(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(3)
	h.sentry.pageSize = 1 // the fresher regression holds the lists' one slot: the watch goes on
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "50", ShortID: strp("CAP-50"), Title: "r", Level: "error",
		FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	if rec := sentryIncident(t, h, "50"); rec["alerted"] != true {
		t.Fatalf("setup: issue 50 was not announced: %v", rec)
	}
	h.sentry.edit("50", func(i *pwSentryIssue) {
		i.Substatus = strp("regressed")
		i.LastSeen = time.Now()
		i.Acts = []pwSentryAct{{Type: "set_regression", At: time.Now()}}
	})
	sentryTick(t, h, wf)
	watch, _ := sentryIncident(t, h, "50")["transition_seen_at"].(string)
	if watch == "" {
		t.Fatal("setup: the later regression is not watched")
	}
	h.sentry.edit("50", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(-100 * 24 * time.Hour) })
	// Two fresher regressions: the lists stay cut (incomplete), so the watch
	// on 50 goes on while nothing reads it.
	for _, id := range []string{"51", "52"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("CAP-" + id), Title: "f", Level: "error", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(), Acts: []pwSentryAct{{Type: "set_regression", At: time.Now()}}})
	}
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	capPut(h, "error", "61", "62")
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if !capHolds(t, h, "50") {
		t.Fatalf("the regression of an announced issue was cut silently (store %v)", capStore(t, h))
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "CAP-50") {
		t.Fatalf("the later regression was not announced before the cut:\n%s", body)
	}
	rec := sentryIncident(t, h, "50")
	if rec["alerted"] != true || rec["transition_at"] != watch {
		t.Fatalf("announced: want it alerted and dated at its watch %s, got %v", watch, rec)
	}
}

// TestProdWatch_SentryCapNeverAnnouncesRecordedHistory: a regression dated at
// the arming is history — the cut takes its record without announcing it as
// news, whatever its watch.
func TestProdWatch_SentryCapNeverAnnouncesRecordedHistory(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0 }))
	now := time.Now()
	// The bootstrap dates the pre-arming regression from its activity and
	// records it as history, never announced.
	h.sentry.put(&pwSentryIssue{ID: "70", ShortID: strp("CAP-70"), Title: "h", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Hour),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-25 * time.Hour)}}})
	sentryTick(t, h, wf)
	rec := sentryIncident(t, h, "70")
	if rec["transition_at"] == nil || rec["alerted"] == true {
		t.Fatalf("setup: want the pre-arming regression dated as history and never announced: %v", rec)
	}
	h.sentry.edit("70", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(-100 * 24 * time.Hour) })
	capPut(h, "error", "71", "72")
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	if capHolds(t, h, "70") {
		t.Fatalf("history was kept past the cap: %v", capStore(t, h))
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("history was announced as news: %v", got)
	}
}

// TestProdWatch_SentryCheckQueueOldestWatchAmongWatched: with one check a tick,
// among the regressions never checked, the one watched longest goes first — a
// pool of watched regressions is drained oldest watch first (their watch ages
// are state generations, not stamps a runner ahead could outrank).
func TestProdWatch_SentryCheckQueueOldestWatchAmongWatched(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 0 }))
	sentryTick(t, h, wf) // bootstrap
	now := time.Now()
	mk := func(id string, ago time.Duration) {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("CAP-" + id), Title: "r", Level: "error", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-ago),
			Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-ago)}}})
	}
	for _, c := range []struct {
		id  string
		ago time.Duration
	}{{"91", 30 * time.Minute}, {"92", 20 * time.Minute}, {"93", 10 * time.Minute}} {
		mk(c.id, c.ago)
		time.Sleep(1100 * time.Millisecond) // distinct watch generations (one tick each)
		sentryTick(t, h, wf)
		if sentryIncident(t, h, c.id)["transition_seen_at"] == nil {
			t.Fatalf("setup: %s is not watched", c.id)
		}
	}
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	var order []string
	for k := 0; k < 3; k++ {
		order = append(order, sentryAlerts(sentryTick(t, h, wf))...)
	}
	want := []string{"regressed:CAP-91:medium", "regressed:CAP-92:medium", "regressed:CAP-93:medium"}
	if !eqStrings(order, want) {
		t.Fatalf("one check a tick: want the longest watched first %v, got %v", want, order)
	}
}

// TestProdWatch_SentryRetentionForgetsAWatchedRegressionSaid: retention is the
// one path that can still forget a never-announced regression (unread behind a
// capped list for forget_after_days) — it is counted and said.
func TestProdWatch_SentryRetentionForgetsAWatchedRegressionSaid(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, capConfig(h, 20, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	h.sentry.pageSize = 1
	watch := capWatched(t, h, wf)
	st := h.state(t)
	rec := st["incidents"].(map[string]any)["sentry:50"].(map[string]any)
	old := time.Now().Add(-16 * 24 * time.Hour).UTC().Format(time.RFC3339)
	rec["first_seen"], rec["last_seen"] = old, old
	h.setState(t, st)
	o := sentryTick(t, h, wf)
	if capHolds(t, h, "50") {
		t.Fatal("setup: retention did not forget the regression")
	}
	if rec["transition_seen_at"] != watch {
		t.Fatalf("setup: the watch ended before retention (%v)", rec["transition_seen_at"])
	}
	if r := sentryNoteReasons(o); !strings.Contains(r, "watched regression(s) the lane never dated nor announced were forgotten") {
		t.Fatalf("a watched regression forgotten by retention was not said: %q", r)
	}
}

// TestProdWatch_SentryAnnouncedDeferredKeepsTheUndatedLine: an announcement the
// per-run cap defers re-emits as pending and still says it is undated.
func TestProdWatch_SentryAnnouncedDeferredKeepsTheUndatedLine(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.alertCap.Store(1)
	h.sentry.pageSize = 1
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	watch := capWatched(t, h, wf)
	capPut(h, "error", "61", "62")
	said := false
	for k := 0; k < 4 && !said; k++ {
		n := len(h.bodies())
		sentryTick(t, h, wf)
		said = strings.Contains(strings.Join(h.bodies()[n:], "\n"), "undated — in the list since `"+watch[:16]+"`")
	}
	if !said {
		t.Fatalf("the re-emitted announcement lost its undated line")
	}
}

// TestProdWatch_SentryCheckQueueAWatchStampAheadDoesNotReorder: the watch's age
// is a state generation — a watch stamp a runner ahead wrote for the OLDER
// watch must not send the queue to the fresher one first (both are checked this
// tick; the order of the activity lookups is the verdict).
func TestProdWatch_SentryCheckQueueAWatchStampAheadDoesNotReorder(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 2 }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "95", ShortID: strp("CAP-95"), Title: "a", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Hour), Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Hour)}}})
	sentryTick(t, h, wf) // 95 watched
	h.sentry.put(&pwSentryIssue{ID: "96", ShortID: strp("CAP-96"), Title: "b", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(), Acts: []pwSentryAct{{Type: "set_regression", At: time.Now()}}})
	sentryTick(t, h, wf) // 96 watched, a generation later
	st := h.state(t)
	// A runner whose clock is a year ahead wrote the older watch's stamp.
	st["incidents"].(map[string]any)["sentry:95"].(map[string]any)["transition_seen_at"] = time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	n := len(h.sentry.callsTo("activities"))
	sentryTick(t, h, wf)
	var order []string
	for _, c := range h.sentry.callsTo("activities")[n:] {
		if strings.Contains(c.Path, "/issues/95/") {
			order = append(order, "95")
		} else if strings.Contains(c.Path, "/issues/96/") {
			order = append(order, "96")
		}
	}
	if strings.Join(order, ",") != "95,96" {
		t.Fatalf("the older watch (95) must be checked first, whatever its stamp says: %v", order)
	}
}

// capWatched records issue 50 as a regression the lane watches but never dated
// nor announced (no activity check), then keeps it unread the way a regression
// flood does: a fresher regression takes the transition list's only slot every
// tick (pages of 1, max_issues 1), so the list is never read whole and the
// watch goes on. It returns the watch's start.
func capWatched(t *testing.T, h *pwHarness, wf *ir.Workflow) string {
	t.Helper()
	h.sentry.pageSize = 1
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "50", ShortID: strp("CAP-50"), Title: "r", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute), Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Minute)}}})
	sentryTick(t, h, wf)
	rec := sentryIncident(t, h, "50")
	watch, _ := rec["transition_seen_at"].(string)
	if watch == "" || rec["alerted"] == true || rec["transition_at"] != nil {
		t.Fatalf("setup: want issue 50 watched, undated and never announced, got %v", rec)
	}
	h.sentry.put(&pwSentryIssue{ID: "51", ShortID: strp("CAP-51"), Title: "f", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(), Acts: []pwSentryAct{{Type: "set_regression", At: time.Now()}}})
	sentryTick(t, h, wf)
	sentryTick(t, h, wf)
	if got := sentryIncident(t, h, "50")["transition_seen_at"]; got != watch {
		t.Fatalf("setup: the watch of issue 50 ended (%v) — its list must stay cut", got)
	}
	return watch
}

// TestProdWatch_SentryCapAnnouncesAWatchedRegressionBeforeItsCut: a regression
// the lane watched but never dated nor announced is announced — not cut — when
// the cut reaches it, recorded as dated at its watch (its date, found later, is
// no news twice); the next cut takes it.
func TestProdWatch_SentryCapAnnouncesAWatchedRegressionBeforeItsCut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	watch := capWatched(t, h, wf)
	capPut(h, "error", "61", "62")
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if !capHolds(t, h, "50") {
		t.Fatalf("a watched regression never announced was cut silently (store %v)", capStore(t, h))
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "CAP-50") {
		t.Fatalf("the watched regression was not announced before its cut:\n%s", body)
	}
	rec := sentryIncident(t, h, "50")
	if rec["alerted"] != true || rec["transition_at"] != watch || rec["transition_seen_at"] != nil {
		t.Fatalf("announced: want it alerted and dated at its watch %s, got %v", watch, rec)
	}
	sentryTick(t, h, wf)
	if capHolds(t, h, "50") {
		t.Fatalf("announced and nothing protecting it: the next cut must take it (store %v)", capStore(t, h))
	}
}

// TestProdWatch_SentryCapBootstrapAnnouncesNothing: a bootstrap posts nothing —
// a cursor removed by hand, records kept — so it keeps a watched regression it
// cannot announce rather than announce it or cut it silently.
func TestProdWatch_SentryCapBootstrapAnnouncesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(0)
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	capWatched(t, h, wf)
	capPut(h, "error", "61", "62")
	st := h.state(t)
	delete(st["cursors"].(map[string]any), "sentry")
	h.setState(t, st)
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if w, _ := o["poll_sentry"]["walk"].(map[string]any); w["bootstrap"] != true {
		t.Fatalf("setup: the tick after the cursor's removal is not a bootstrap: %v", w)
	}
	if got := sentryAlerts(o); len(got) != 0 {
		t.Fatalf("a bootstrap posted: %v", got)
	}
	if body := strings.Join(h.bodies()[n:], "\n"); strings.Contains(body, "CAP-50") {
		t.Fatalf("a bootstrap announced the watched regression:\n%s", body)
	}
	if !capHolds(t, h, "50") {
		t.Fatalf("a bootstrap cut a watched regression it could not announce (store %v)", capStore(t, h))
	}
}

// TestProdWatch_SentryCapAnnouncedUndatedSaysSo: posted alone, the announcement
// says it is undated, and since when the lane saw it in the list.
func TestProdWatch_SentryCapAnnouncedUndatedSaysSo(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, capConfig(h, 2, 0, func(s map[string]any) { s["max_transition_checks"] = 0; s["max_issues"] = 1 }))
	watch := capWatched(t, h, wf)
	capPut(h, "error", "61", "62")
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); !strings.Contains(strings.Join(got, " "), "regressed:CAP-50:") {
		t.Fatalf("setup: want the watched regression announced alone, got %v", got)
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "undated — in the list since `"+watch[:16]+"`") {
		t.Fatalf("the announcement does not say it is undated, since %s:\n%s", watch[:16], body)
	}
}

// TestProdWatch_SentryCheckQueuePutsTheFloorFirst: with one activity check a
// tick, a regression at min_level goes before a fresher one below it — the
// floor decides what posts NEW only, but low-level noise never starves a real
// regression's dating.
func TestProdWatch_SentryCheckQueuePutsTheFloorFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "71", ShortID: strp("CAP-71"), Title: "r", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Hour), Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Hour)}}})
	h.sentry.put(&pwSentryIssue{ID: "81", ShortID: strp("CAP-81"), Title: "j", Level: "info", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:CAP-71:medium"}) {
		t.Fatalf("one check: the regression at min_level must be dated before the fresher one below it, got %v", got)
	}
}

// TestProdWatch_SentryCheckQueuePutsTheOldestWatchFirst: among regressions never
// checked, the one the lane has watched longest goes first — a fresher one, or
// junk the store's cut sent back unknown, cannot hold the check for ever.
func TestProdWatch_SentryCheckQueuePutsTheOldestWatchFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	// Both dated after the arming (minus its overlap): news, once dated.
	h.sentry.put(&pwSentryIssue{ID: "91", ShortID: strp("CAP-91"), Title: "a", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-30 * time.Minute), Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-30 * time.Minute)}}})
	h.sentry.put(&pwSentryIssue{ID: "92", ShortID: strp("CAP-92"), Title: "x", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-20 * time.Minute), Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-20 * time.Minute)}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:CAP-92:medium"}) {
		t.Fatalf("setup: two unwatched regressions, one check: want the fresher dated, got %v", got)
	}
	if sentryIncident(t, h, "91")["transition_seen_at"] == nil {
		t.Fatal("setup: the regression left undated is not watched")
	}
	h.sentry.put(&pwSentryIssue{ID: "93", ShortID: strp("CAP-93"), Title: "b", Level: "error", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(), Acts: []pwSentryAct{{Type: "set_regression", At: time.Now()}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:CAP-91:medium"}) {
		t.Fatalf("the regression watched since the last tick must be checked before a fresher unwatched one, got %v", got)
	}
}

// TestProdWatch_SentryCapOffStillStampsTheDefaultWindow: with the cap off, a post
// alone still opens the window the default cap computes — turning the cap on
// later finds the incidents posted meanwhile protected, not stripped bare.
func TestProdWatch_SentryCapOffStillStampsTheDefaultWindow(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.setMaxPerLane(1)
	h.writeConfig(t, capConfig(h, 0, 0, nil))
	sentryTick(t, h, wf)
	capPut(h, "fatal", "100")
	sentryTick(t, h, wf)
	g := capGen(t, h)
	if w, _ := sentryIncident(t, h, "100")["alone_until_gen"].(float64); int(w) != g+2500 {
		t.Fatalf("max_records 0, one post alone a tick: want the default cap's window (5000 / 2 = 2500 generations) until %d, got %v", g+2500, w)
	}
}
