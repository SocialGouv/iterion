package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pwTwoAddressStub makes host twoaddr.test resolve to 127.0.0.2 then
// 127.0.0.1 (the given port), and a connect to 127.0.0.2 hang like a dropped
// SYN: the first address eats the wall clock, the second one trickles. The
// host is exempted from any HTTP proxy, or the stub would never be asked.
func pwTwoAddressStub(port string) string {
	return "import os as _o, socket as _s, time as _t\n" +
		"for _k in ('no_proxy', 'NO_PROXY'):\n" +
		"    _o.environ[_k] = 'twoaddr.test,' + _o.environ.get(_k, '')\n" +
		"_gai = _s.getaddrinfo\n" +
		"def _two(host, *a, **k):\n" +
		"    if host == 'twoaddr.test':\n" +
		"        return [(_s.AF_INET, _s.SOCK_STREAM, 6, '', ('127.0.0.2', " + port + ")), (_s.AF_INET, _s.SOCK_STREAM, 6, '', ('127.0.0.1', " + port + "))]\n" +
		"    return _gai(host, *a, **k)\n" +
		"_s.getaddrinfo = _two\n" +
		"_conn = _s.socket.connect\n" +
		"def _hang(self, addr):\n" +
		"    if addr[0] == '127.0.0.2':\n" +
		"        _t.sleep(60)\n" +
		"    return _conn(self, addr)\n" +
		"_s.socket.connect = _hang\n"
}

// pwNoLeftoverAlarm fails the node (exit 3) when an interval timer is still
// armed at its exit: an alarm must never outlive the exchange it bounds.
const pwNoLeftoverAlarm = "import atexit as _ae, os as _os, signal as _sg, sys as _sy\n" +
	"def _no_leftover():\n" +
	"    if _sg.getitimer(_sg.ITIMER_REAL)[0] > 0:\n" +
	"        _sy.stderr.write('LEFTOVER ALARM\\n'); _sy.stderr.flush(); _os._exit(3)\n" +
	"_ae.register(_no_leftover)\n"

// TestProdWatch_TheWallClockSurvivesAnAddressThatHangs: a host with two
// addresses, the first dropping SYNs and the second trickling its answer —
// the wall clock is spent in the first connect, and it still fires: its
// exception is not an OSError, which the connect loop would swallow before
// trying the next address unbounded.
func TestProdWatch_TheWallClockSurvivesAnAddressThatHangs(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, node := range []string{"probe_http", "resolve_release", "poll_loki", "poll_prom", "poll_sentry"} {
		node := node
		t.Run(node, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			raw := pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60))
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
			base := "http://twoaddr.test:" + port
			h.writeConfig(t, func(cfg map[string]any) {
				cfg["grafana"] = map[string]any{"base_url": base, "loki_uid": "loki", "prometheus_uid": "prom"}
				cfg["loki"].(map[string]any)["queries"] = map[string]any{"errors": "errors-q"}
				cfg["release"] = map[string]any{"source": "health_field", "health_url": base + "/health", "field": "version"}
				cfg["probes"] = []map[string]any{{"id": "api", "url": base + "/health", "expect_status": 200, "severity": "critical", "timeout_secs": 2}}
				cfg["sentry"] = map[string]any{"base_url": base, "org": "org", "project": "proj", "deadline_secs": 10}
			})
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
			plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
			if err != nil {
				t.Fatalf("plan: %v %s", err, stderr)
			}
			in := map[string]any{"allow_private": true, "timeout_secs": 2}
			switch node {
			case "probe_http":
				in["probes"] = plan["probes"]
			case "resolve_release":
				in["release"] = plan["release"]
			case "poll_loki":
				in["grafana"], in["loki"], in["scratch_dir"] = plan["grafana"], plan["loki"], h.scratch
			case "poll_prom":
				in["grafana"], in["prometheus"] = plan["grafana"], plan["prometheus"]
			case "poll_sentry":
				in["sentry"], in["scratch_dir"] = plan["sentry"], h.scratch
			}
			start := time.Now()
			out, stderr, err := runPyWhole(t, h.ws, pwTwoAddressStub(port)+pwSub(t, pwTool(t, wf, node).Script, in, vars, secrets))
			took := time.Since(start)
			if err != nil {
				t.Fatalf("%s: %v %s", node, err, stderr)
			}
			if took > 18*time.Second {
				t.Fatalf("%s: the second address trickled on unbounded, %v (its wall clock was spent in the first connect): %v", node, took, out)
			}
			// The failure must be the wall clock's, not a refused connection:
			// poll_sentry's is its deadline, the chassis' an exchange timeout.
			byClock := strings.Contains(fmt.Sprint(out), "ExchangeTimeout")
			if node == "poll_sentry" {
				walk, _ := out["walk"].(map[string]any)
				byClock = strings.Contains(fmt.Sprint(out["errors"]), "DeadlineReached") || walk["deadline_hit"] == true
			}
			if !byClock {
				t.Fatalf("%s: stopped in %v, but not by its wall clock: %v", node, took, out)
			}
			if node == "probe_http" && strings.Contains(fmt.Sprint(out), "ok:true") {
				t.Fatalf("a probe that never got an answer reported ok: %v", out)
			}
		})
	}
}

// TestProdWatch_NoAlarmOutlivesItsExchange: every node that arms a wall clock
// disarms it once the exchange ends — a leftover alarm would fire later, in
// the middle of a page or after the walk, as a spurious timeout.
func TestProdWatch_NoAlarmOutlivesItsExchange(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, node := range []string{"probe_http", "resolve_release", "poll_loki", "poll_prom", "poll_sentry"} {
		node := node
		t.Run(node, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.lines.Store([]pwLine{{TS: nsAgo(time.Minute), Line: "ERROR x", Container: "api", Q: "errors-q"}})
			h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}})
			h.writeConfig(t, func(cfg map[string]any) {
				cfg["sentry"] = map[string]any{"base_url": h.srv.URL, "org": "org", "project": "proj", "environment": "preprod"}
			})
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
			plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
			if err != nil {
				t.Fatalf("plan: %v %s", err, stderr)
			}
			in := map[string]any{"allow_private": true, "timeout_secs": 5}
			switch node {
			case "probe_http":
				in["probes"] = plan["probes"]
			case "resolve_release":
				in["release"] = plan["release"]
			case "poll_loki":
				in["grafana"], in["loki"], in["scratch_dir"] = plan["grafana"], plan["loki"], h.scratch
			case "poll_prom":
				in["grafana"], in["prometheus"] = plan["grafana"], plan["prometheus"]
			case "poll_sentry":
				in["sentry"], in["scratch_dir"] = plan["sentry"], h.scratch
			}
			if _, stderr, err := runPyWhole(t, h.ws, pwNoLeftoverAlarm+pwSub(t, pwTool(t, wf, node).Script, in, vars, secrets)); err != nil {
				t.Fatalf("%s: %v %s", node, err, stderr)
			}
		})
	}
}

// TestProdWatch_ASlowButSteadyGrafanaAnswerIsRead: a large Prometheus answer
// arriving slowly but steadily (about 600 KB at 100 KB/s, fetch timeout 5 s)
// is read whole: a Grafana exchange's wall clock is six fetch timeouts — the
// socket timeout already bounds a stall, the wall clock only a trickle.
func TestProdWatch_ASlowButSteadyGrafanaAnswerIsRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/json")
		head := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"pad":"`
		tail := `"},"value":[1700000000,"0"]}]}}`
		_, _ = w.Write([]byte(head))
		chunk := strings.Repeat("x", 10*1024)
		for i := 0; i < 60; i++ { // 600 KB, 10 KB every 100 ms
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		_, _ = w.Write([]byte(tail))
	}))
	t.Cleanup(slow.Close)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["grafana"] = map[string]any{"base_url": slow.URL, "loki_uid": "loki", "prometheus_uid": "prom"}
	})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v %s", err, stderr)
	}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_prom").Script, map[string]any{
		"grafana": plan["grafana"], "prometheus": plan["prometheus"], "timeout_secs": 5, "allow_private": true}, vars, secrets))
	if err != nil {
		t.Fatalf("poll_prom: %v %s", err, stderr)
	}
	if out["ok"] != true {
		t.Fatalf("a slow but steady answer (6 s, fetch timeout 5 s) was not read: %v", out)
	}
}
