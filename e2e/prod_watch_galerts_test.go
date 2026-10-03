package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The fake Alertmanager is deliberately less kind than the real Grafana one:
// held (suppressed / unprocessed) alerts come FIRST in the answer, a record
// may be malformed, a generatorURL may point at another host, a label value
// may carry a secret (the identity must survive the scrub), and every state
// word the API version invents lands in the held count.

// pwGaAlert builds one Alertmanager alert as the API v2 shape carries it.
func pwGaAlert(fp, alertname, severity string, mutate func(map[string]any)) map[string]any {
	a := map[string]any{
		"labels":      map[string]any{"alertname": alertname, "severity": severity},
		"annotations": map[string]any{"summary": alertname + " is firing"},
		"startsAt":    "2026-10-03T08:00:00Z",
		"endsAt":      "0001-01-01T00:00:00Z",
		"status":      map[string]any{"state": "active", "silences": []any{}, "inhibits": []any{}},
		"fingerprint": fp,
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

// mountGalerts mounts the Alertmanager endpoint the lane polls.
func (h *pwHarness) mountGalerts(mux *http.ServeMux) {
	mux.HandleFunc("/api/alertmanager/grafana/api/v2/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pwToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if st := int(h.galertsStatus.Load()); st != 0 {
			w.WriteHeader(st)
			return
		}
		if raw, _ := h.galertsRaw.Load().(string); raw != "" {
			// Served verbatim: a lone surrogate on the wire survives Go's
			// own marshalling (which would replace it with U+FFFD).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(raw))
			return
		}
		alerts, _ := h.galerts.Load().([]map[string]any)
		if alerts == nil {
			alerts = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(alerts)
	})
}

// gaOnly reshapes the config to a grafana_alerts-only deployment: the other
// lanes off, the lane on with its defaults.
func gaOnly(extra func(cfg map[string]any)) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		for _, k := range []string{"loki", "prometheus", "probes", "sentry"} {
			delete(cfg, k)
		}
		cfg["grafana_alerts"] = map[string]any{}
		if extra != nil {
			extra(cfg)
		}
	}
}

// gaPoll runs poll_galerts alone. cfg is the grafana_alerts section; the
// lane is on unless the test turned it off.
func gaPoll(t *testing.T, wf *ir.Workflow, h *pwHarness, cfg map[string]any) map[string]any {
	t.Helper()
	if _, ok := cfg["enabled"]; !ok {
		cfg["enabled"] = true
	}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_galerts").Script, map[string]any{
		"grafana": map[string]any{"base_url": h.srv.URL, "deadline_secs": 30}, "grafana_alerts": cfg,
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil,
		map[string]string{"grafana_token": h.tokenFile}))
	if err != nil {
		t.Fatalf("poll_galerts: %v\n%s", err, stderr)
	}
	return out
}

// gaSignal is one scrubbed handoff record as leak_scan writes it.
func gaSignal(fp string, labels map[string]any) map[string]any {
	return map[string]any{"fingerprint": fp, "labels": labels, "annotations": map[string]any{},
		"starts_at": "2026-10-03T08:00:00Z", "ends_at": "", "state": "active", "generator_url": "", "leaks": []any{}}
}

// gaState is the state carrying one alerted galert incident.
func gaState(fp string) map[string]any {
	return map[string]any{"version": 1, "generation": 7,
		"cursors": map[string]any{"loki": map[string]any{}}, "health": map[string]any{},
		"incidents": map[string]any{"galert:" + fp: map[string]any{
			"fp": "galert:" + fp, "kind": "galert", "sources": []any{"grafana_alerts"}, "severity": "high",
			"title_key": "galert_firing", "title_arg": "Latency is firing", "detail_key": "galert_detail",
			"fields": map[string]any{"alertname": "Latency", "labels": "severity=high", "starts": "2026-10-03T08:00"},
			"first_seen": hoursAgo(1), "last_seen": hoursAgo(0.1), "count": 1, "alerted": true,
			"last_notified": hoursAgo(0.1), "quiet_noted": false, "closed_said": false}},
	}
}

func TestProdWatch_GAlerts_FiringThenResolvedThenRefire(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "a1b2c3d4e5f60718293a4b5c6d7e8f90"

	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", nil)})
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 || got[0] != "galert:new:critical" {
		t.Fatalf("a firing alert must mint one NEW incident at its label severity: %v (%s)", got, outs["decide"]["summary"])
	}
	st := h.state(t)
	rec := st["incidents"].(map[string]any)["galert:"+fp].(map[string]any)
	if rec["alerted"] != true {
		t.Fatalf("the incident must be alerted: %v", rec)
	}
	if len(h.bodies()) != 1 || !strings.Contains(h.bodies()[0], "HighLatency") {
		t.Fatalf("the channel must hear the alert by name: %v", h.bodies())
	}

	// The alert stops firing; the next read is whole (200, no errors): the
	// channel that heard it fire hears it stop, once, at low severity.
	h.galerts.Store([]map[string]any{})
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 || got[0] != "galert:resolved:low" {
		t.Fatalf("a whole read without the alert must post one resolved note: %v", got)
	}
	st = h.state(t)
	rec = st["incidents"].(map[string]any)["galert:"+fp].(map[string]any)
	if rec["closed_said"] != true {
		t.Fatalf("the incident must be stamped closed_said: %v", rec)
	}

	// It fires again: news again (a reminder would bury it), and posted, the
	// closure is consumed — a later tick inside the renotify window is silent.
	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", nil)})
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 || got[0] != "galert:new:critical" {
		t.Fatalf("a refiring alert must be news again: %v", got)
	}
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 0 {
		t.Fatalf("still firing within the renotify window: silence, not a second news: %v", got)
	}
}

func TestProdWatch_GAlerts_HeldNeverMints(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "0102030405060708090a0b0c0d0e0f10"

	// Held first in the answer, and a state word the API version invented.
	h.galerts.Store([]map[string]any{
		pwGaAlert("deadbeefdeadbeefdeadbeefdeadbeef", "Silenced", "critical", func(a map[string]any) {
			a["status"] = map[string]any{"state": "suppressed"}
		}),
		pwGaAlert("cafebabecafebabecafebabecafebabe", "Routed", "low", func(a map[string]any) {
			a["status"] = map[string]any{"state": "awaiting-route"}
		}),
		pwGaAlert(fp, "Watched", "high", nil),
	})
	out := gaPoll(t, wf, h, map[string]any{})
	if out["alerts"] != float64(1) || out["walk"].(map[string]any)["active"] != float64(1) ||
		out["walk"].(map[string]any)["held"] != float64(2) {
		t.Fatalf("only the active alert is handed off; held is counted: %v", out)
	}
	// The held alerts are fingerprinted (hex only): decide can tell
	// "present but held" from "gone".
	if fps, _ := out["walk"].(map[string]any)["held_fps"].([]any); len(fps) != 2 ||
		fps[0] != "deadbeefdeadbeefdeadbeefdeadbeef" && fps[1] != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("the held alerts must carry their fingerprints in the walk: %v", out["walk"])
	}
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 || got[0] != "galert:new:high" {
		t.Fatalf("a suppressed alert never mints: %v", got)
	}
}

func TestProdWatch_GAlerts_SeverityMapping(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	gaCfg := map[string]any{"enabled": true, "severity_label": "prio", "severity": map[string]any{"page": "critical"},
		"default_severity": "low", "max_severity": "high"}
	alerts := []map[string]any{gaSignal("f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f1", map[string]any{"alertname": "A", "prio": "page"}),
		gaSignal("f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f2", map[string]any{"alertname": "B", "prio": "weird"}),
		gaSignal("f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f3", map[string]any{"alertname": "C"})}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"galerts": alerts}, nil, map[string]any{
		"grafana_alerts": gaCfg, "galerts_ok": true, "galerts_truncated": false, "galerts_errors": []any{},
		"galerts_walk": map[string]any{"answered": true}, "galerts_expected": len(alerts),
		"lanes": map[string]any{"grafana_alerts": true}})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	sev := map[string]string{}
	for _, a := range out["alerts"].([]any) {
		m := a.(map[string]any)
		sev[strings.TrimPrefix(m["fingerprint"].(string), "galert:")] = m["severity"].(string)
	}
	if sev["f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f1"] != "high" || sev["f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f2"] != "low" ||
		sev["f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f3"] != "low" {
		t.Fatalf("mapped value is capped by max_severity, unmapped and missing take the default: %v", sev)
	}
}

func TestProdWatch_GAlerts_DeadLaneNeverResolves(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "11111111222222223333333344444444"

	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", nil)})
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 {
		t.Fatalf("tick 1 must alert: %v", got)
	}
	// The lane dies (a 5xx — the retry is paid): the read is refused, the
	// error named, and the whole tick REFUSES (zero façade: the lane is the
	// only one configured). Nothing is concluded.
	h.galertsStatus.Store(500)
	pollOut := gaPoll(t, wf, h, map[string]any{})
	if pollOut["ok"] != false || !strings.Contains(pollOut["errors"].([]any)[0].(map[string]any)["error"].(string), "HTTP Error 500") {
		t.Fatalf("a refused read names its error: %v", pollOut)
	}
	h.galertsStatus.Store(401)
	pollOut = gaPoll(t, wf, h, map[string]any{})
	if pollOut["ok"] != false || !strings.Contains(pollOut["errors"].([]any)[0].(map[string]any)["error"].(string), "refused the token") {
		t.Fatalf("a refused token is named too: %v", pollOut)
	}
	h.galertsStatus.Store(0)
	// decide over a refused read, in a live multi-lane deployment (a probe
	// answers): the incident keeps its clocks — no resolution — and the
	// error is said.
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"galerts": []any{}}, gaState(fp), map[string]any{
		"grafana_alerts": map[string]any{"enabled": true}, "galerts_ok": false, "galerts_truncated": false,
		"galerts_errors": []map[string]any{{"query": "grafana alerts", "error": "HTTPError: HTTP Error 500: Internal Server Error"}},
		"galerts_walk":   map[string]any{}, "galerts_expected": 0,
		"lanes":        map[string]any{"probes": true, "grafana_alerts": true},
		"http_results": []map[string]any{{"id": "api", "url": "u", "ok": true, "status": 200, "ms": 5, "expected": 200}}})
	if err != nil {
		t.Fatalf("decide over a refused read: %v %s", err, stderr)
	}
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 0 {
		t.Fatalf("a dead lane is not an absence of observation: %v", got)
	}
	// The error is said: its coverage component (the summary says "partial",
	// the component names the error kind) is stamped as said this tick.
	said := false
	for c := range pwStateNext(t, out)["coverage_posted"].(map[string]any) {
		if strings.Contains(c, "grafana_alerts") && strings.Contains(c, "HTTPError") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the lane error must be said as a coverage component: %v", pwStateNext(t, out)["coverage_posted"])
	}
	if rec := pwStateNext(t, out)["incidents"].(map[string]any)["galert:"+fp].(map[string]any); rec["closed_said"] == true {
		t.Fatalf("a refused read must not close the incident: %v", rec)
	}
	// The lane answers whole again: the fact is said now.
	out, _, err = pwDecide(t, wf, h, map[string]any{"galerts": []any{}}, gaState(fp), map[string]any{
		"grafana_alerts": map[string]any{"enabled": true}, "galerts_ok": true, "galerts_truncated": false,
		"galerts_errors": []any{}, "galerts_walk": map[string]any{"answered": true}, "galerts_expected": 0,
		"lanes": map[string]any{"grafana_alerts": true}})
	if err != nil {
		t.Fatalf("decide over a whole read: %v", err)
	}
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 1 || got[0] != "galert:resolved:low" {
		t.Fatalf("the resolution must be said on the next whole read: %v", got)
	}
	// The lane turned OFF (the section left the config): its poll answers
	// ok and whole by construction — and still nothing is concluded. Every
	// other lane freezes here; galert must not be the one that resolves.
	out, _, err = pwDecide(t, wf, h, map[string]any{"galerts": []any{}}, gaState(fp), map[string]any{
		"grafana_alerts": map[string]any{"enabled": false}, "galerts_ok": true, "galerts_truncated": false,
		"galerts_errors": []any{}, "galerts_walk": map[string]any{"answered": true}, "galerts_expected": 0,
		"lanes":        map[string]any{"probes": true},
		"http_results": []map[string]any{{"id": "api", "url": "u", "ok": true, "status": 200, "ms": 5, "expected": 200}}})
	if err != nil {
		t.Fatalf("decide with the lane off: %v", err)
	}
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 0 {
		t.Fatalf("a lane that is off is not an absence of observation: %v", got)
	}
	if rec := pwStateNext(t, out)["incidents"].(map[string]any)["galert:"+fp].(map[string]any); rec["closed_said"] == true {
		t.Fatalf("an off lane must not close the incident: %v", rec)
	}
	// The alert is PRESENT but held (a silence, an inhibition): the lane
	// sees it — "resolved — no longer firing" would be a lie told exactly
	// during incidents. Nothing is concluded; when the silence ends the
	// incident is still open, its clocks untouched.
	out, _, err = pwDecide(t, wf, h, map[string]any{"galerts": []any{}}, gaState(fp), map[string]any{
		"grafana_alerts": map[string]any{"enabled": true}, "galerts_ok": true, "galerts_truncated": false,
		"galerts_errors": []any{}, "galerts_walk": map[string]any{"answered": true, "held_fps": []any{fp}}, "galerts_expected": 0,
		"lanes":        map[string]any{"probes": true, "grafana_alerts": true},
		"http_results": []map[string]any{{"id": "api", "url": "u", "ok": true, "status": 200, "ms": 5, "expected": 200}}})
	if err != nil {
		t.Fatalf("decide over a held alert: %v", err)
	}
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 0 {
		t.Fatalf("a held alert is seen, not absent: %v", got)
	}
	if rec := pwStateNext(t, out)["incidents"].(map[string]any)["galert:"+fp].(map[string]any); rec["closed_said"] == true {
		t.Fatalf("a held alert must not close the incident: %v", rec)
	}
	// The walk could not fingerprint everything it held: the seen-set is
	// incomplete, so no resolution at all this tick.
	out, _, err = pwDecide(t, wf, h, map[string]any{"galerts": []any{}}, gaState(fp), map[string]any{
		"grafana_alerts": map[string]any{"enabled": true}, "galerts_ok": true, "galerts_truncated": false,
		"galerts_errors": []any{}, "galerts_walk": map[string]any{"answered": true, "held_fps": []any{}, "held_fps_cut": true},
		"galerts_expected": 0, "lanes": map[string]any{"probes": true, "grafana_alerts": true},
		"http_results": []map[string]any{{"id": "api", "url": "u", "ok": true, "status": 200, "ms": 5, "expected": 200}}})
	if err != nil {
		t.Fatalf("decide over a cut held set: %v", err)
	}
	if got := alertsOf(t, map[string]map[string]any{"decide": out}); len(got) != 0 {
		t.Fatalf("a cut held set concludes nothing either: %v", got)
	}
}

func TestProdWatch_GAlerts_TruncatedNeverResolves(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "2122232425262728292a2b2c2d2e2f30"

	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", nil)})
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 {
		t.Fatalf("tick 1 must alert: %v", got)
	}
	// A malformed record makes the read partial: absence proves nothing.
	// (An active-shaped record whose labels are not a map is the malformed
	// one; a record with no active state is held, not malformed.)
	h.galerts.Store([]map[string]any{{"labels": "not-a-map", "status": map[string]any{"state": "active"}}})
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 0 {
		t.Fatalf("a partial read never resolves: %v", got)
	}
	if rec := h.state(t)["incidents"].(map[string]any)["galert:"+fp].(map[string]any); rec["closed_said"] == true {
		t.Fatalf("a truncated read must not close the incident: %v", rec)
	}
}

func TestProdWatch_GAlerts_ScrubbedEverywhere(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "3132333435363738393a3b3c3d3e3f40"
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N0XgL0n3I9PlFUP0THsR8U"
	gtok := "ghp_AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1111"
	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", func(a map[string]any) {
		a["annotations"] = map[string]any{"summary": "latency up, token " + jwt + " leaked by p@ir-mailler.example",
			// The BARE token as the key: short enough to survive the poll's
			// 64-char cap whole, and a shape the scrub's word-boundary
			// classes actually match (a `_`-glued prefix would hide the
			// boundary and mask nothing — the same everywhere in the bot).
			gtok: "the key is free text too"}
		a["labels"] = map[string]any{"alertname": "HighLatency", "severity": "critical", "note": "pasted: " + jwt,
			// A label NAME the poll's own LABEL_KEY accepts (letters,
			// digits, underscore) that IS a token — the shape that reaches
			// leak_scan's key scrub.
			gtok: "the label key too"}
	})})
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 {
		t.Fatalf("the alert must still fire: %v", got)
	}
	if !strings.Contains(h.bodies()[0], "REDACTED") {
		t.Fatalf("the free text must reach the channel masked: %v", h.bodies())
	}
	// Nothing raw anywhere the tick wrote: the messages, the state, the
	// ledgers, the signals of this run — the values AND the keys.
	for _, root := range []string{filepath.Join(h.ws, ".prod-watch")} {
		if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, raw := range []string{jwt, gtok} {
				if strings.Contains(string(b), raw) {
					t.Errorf("%s carries the raw token %s...", path, raw[:12])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	joined := strings.Join(h.bodies(), "\n")
	for _, raw := range []string{jwt, gtok} {
		if strings.Contains(joined, raw) {
			t.Fatalf("the channel must never see the raw token %s...: %v", raw[:12], h.bodies())
		}
	}
	// What leaves leak_scan is DERIVED and REDACTED — the signals file is
	// the handoff every later consumer reads, annotation keys included,
	// whether or not this slice's decide reads them.
	sig, err := os.ReadFile(outs["leak_scan"]["signals_file"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{jwt, gtok} {
		if strings.Contains(string(sig), raw) {
			t.Fatalf("the signals carry the raw token %s... — nothing leaves the scan unredacted", raw[:12])
		}
	}
}

func TestProdWatch_GAlerts_GeneratorURLOnlySameOrigin(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fpGood := "4142434445464748494a4b4c4d4e4f50"
	fpEvil := "5152535455565758595a5b5c5d5e5f60"

	h.galerts.Store([]map[string]any{
		pwGaAlert(fpGood, "Good", "high", func(a map[string]any) {
			a["generatorURL"] = h.srv.URL + "/alerting/list?alertSource=expression"
		}),
		pwGaAlert(fpEvil, "Evil", "high", func(a map[string]any) {
			a["generatorURL"] = "https://evil.example/alerting/steal"
		}),
	})
	out := gaPoll(t, wf, h, map[string]any{})
	b, err := os.ReadFile(out["raw_file"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "evil.example") {
		t.Fatalf("a foreign-origin generatorURL is dropped before the handoff: %s", b)
	}
	if !strings.Contains(string(b), "/alerting/list") {
		t.Fatalf("the Grafana's own generatorURL survives: %s", b)
	}
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 2 {
		t.Fatalf("both alerts mint: %v", got)
	}
	joined := strings.Join(h.bodies(), "\n")
	if !strings.Contains(joined, "](http://"+strings.TrimPrefix(h.srv.URL, "http://")+"/alerting/list") {
		t.Fatalf("the same-origin link must render: %v", h.bodies())
	}
	if strings.Contains(joined, "evil.example") {
		t.Fatalf("the foreign link must never render: %v", h.bodies())
	}
}

func TestProdWatch_GAlerts_SurrogateSurvivesTheHandoff(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	fp := "7172737475767778797a7b7c7d7e7f80"
	// A lone surrogate (valid JSON on the wire, invalid UTF-8) in an
	// annotation key or a timestamp must not kill the handoff write AFTER
	// the walk — the tick would die in a loop while the alert stayed.
	// Served RAW: Go's own marshalling would replace the surrogate with
	// U+FFFD before it ever reached the wire.
	h.galertsRaw.Store(`[{"labels":{"alertname":"HighLatency","severity":"critical"},
		"annotations":{"bad\ud800key":"v","summary":"firing"},
		"startsAt":"2026-10-03T08:00:0\ud8001Z","endsAt":"","status":{"state":"active"},"fingerprint":"` + fp + `"}]`)
	out := gaPoll(t, wf, h, map[string]any{})
	if out["ok"] != true || out["alerts"] != float64(1) {
		t.Fatalf("a surrogate must not kill the handoff: %v", out)
	}
}

func TestProdWatch_GAlerts_OverCapRefusedHandsOffNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	// 101 distinct firing alerts against max_alerts 100: the walk refuses.
	var many []map[string]any
	for i := 0; i < 101; i++ {
		many = append(many, pwGaAlert(fmt.Sprintf("%064x", i), fmt.Sprintf("Alert%d", i), "high", nil))
	}
	h.galerts.Store(many)
	out := gaPoll(t, wf, h, map[string]any{"max_alerts": 100})
	if out["ok"] != false || out["alerts"] != float64(0) {
		t.Fatalf("a refused read hands off nothing: %v", out)
	}
	if !strings.Contains(out["errors"].([]any)[0].(map[string]any)["error"].(string), "max_alerts") {
		t.Fatalf("the refusal names its bound: %v", out["errors"])
	}
}

func TestProdWatch_GAlerts_PlanValidatesTheKnobs(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	runPlan := func(mod func(cfg map[string]any)) map[string]any {
		h.writeConfig(t, mod)
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, map[string]any{
			"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000,
		}, map[string]string{"grafana_token": h.tokenFile}))
		if err != nil {
			return map[string]any{"stderr": stderr}
		}
		return out
	}
	// The lane is off unless the section is present; the plan names it.
	out := runPlan(func(cfg map[string]any) { delete(cfg, "grafana_alerts") })
	if out["grafana_alerts"].(map[string]any)["enabled"] != false {
		t.Fatalf("no section: the lane is off: %v", out["grafana_alerts"])
	}
	// A non-object section is refused by name.
	if s, _ := runPlan(func(cfg map[string]any) { cfg["grafana_alerts"] = 42 })["stderr"].(string); !strings.Contains(s, "must be an object") {
		t.Fatalf("a non-object section is refused: %s", s)
	}
	// The lane needs the grafana section it polls.
	if s, _ := runPlan(func(cfg map[string]any) {
		delete(cfg, "grafana")
		cfg["grafana_alerts"] = map[string]any{}
	})["stderr"].(string); !strings.Contains(s, "grafana.base_url is missing") {
		t.Fatalf("the lane needs config.grafana.base_url: %s", s)
	}
	// An unknown severity value, and a bad integer, are refused by name.
	for _, bad := range []func(cfg map[string]any){
		func(cfg map[string]any) { cfg["grafana_alerts"] = map[string]any{"severity": map[string]any{"x": "apocalyptic"}} },
		func(cfg map[string]any) { cfg["grafana_alerts"] = map[string]any{"max_alerts": true} },
		func(cfg map[string]any) { cfg["grafana_alerts"] = map[string]any{"severity_label": "9bad"} },
	} {
		if s, _ := runPlan(bad)["stderr"].(string); s == "" {
			t.Fatalf("a bad knob must be refused (got a plan)")
		}
	}
	// The defaults hold when the config is silent, and the lane lands in
	// the plan's output and the lanes map.
	out = runPlan(func(cfg map[string]any) { cfg["grafana_alerts"] = map[string]any{} })
	ga := out["grafana_alerts"].(map[string]any)
	if ga["enabled"] != true || ga["severity_label"] != "severity" || ga["default_severity"] != "high" ||
		ga["max_severity"] != "critical" || ga["max_alerts"] != float64(200) {
		t.Fatalf("the defaults must hold: %v", ga)
	}
	if out["lanes"].(map[string]any)["grafana_alerts"] != true {
		t.Fatalf("the lanes map must carry the lane: %v", out["lanes"])
	}
}

func TestProdWatch_GAlerts_FingerprintSurvivesScrub(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, gaOnly(nil))
	fp := "6162636465666768696a6b6c6d6e6f70"
	h.galerts.Store([]map[string]any{pwGaAlert(fp, "HighLatency", "critical", func(a map[string]any) {
		a["labels"] = map[string]any{"alertname": "HighLatency", "severity": "critical",
			"note": "operator token ghp_AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1111"}
	})})
	outs := h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 1 {
		t.Fatalf("tick 1 must alert: %v", got)
	}
	// The identity IS the Alertmanager's fingerprint: the incident the
	// state carries is keyed by it, not by a hash recomputed after the
	// scrub (traceability to the source, and stability across ticks).
	st := h.state(t)
	if _, ok := st["incidents"].(map[string]any)["galert:"+fp]; !ok {
		t.Fatalf("the incident must be keyed by the Alertmanager's fingerprint: %v", st["incidents"])
	}
	// The same alert, still firing: its identity must not have moved with
	// the scrub — a second incident would post NEW again.
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); len(got) != 0 {
		t.Fatalf("the identity survives the scrub: still one incident, silent inside the window: %v", got)
	}
	st = h.state(t)
	if n := len(st["incidents"].(map[string]any)); n != 1 {
		t.Fatalf("one incident, not two: %d (%v)", n, st["incidents"])
	}
}
