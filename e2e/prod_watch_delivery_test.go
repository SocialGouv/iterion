package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pwBlackhole accepts every connection, reads the request, and never answers.
func pwBlackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 65536)
				_, _ = c.Read(buf)
				select {
				case <-done:
				case <-time.After(60 * time.Second):
				}
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func pwNotifyIn(alerts []map[string]any, sinks []map[string]any) map[string]any {
	return map[string]any{"alerts": alerts, "overflow_count": 0, "stale_sources": []any{}, "sinks": sinks,
		"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": false, "max_message_chars": 14000, "deliver_by": pwDeliverBy()}
}

// TestProdWatch_AnOptionalSinkThatNeverAnswersHoldsNothingBack: the required
// sinks take every message first, and an optional sink that never answers is
// skipped after its first timeout: the tick is consumed, the required sink got
// everything, twice in a row (no replay).
func TestProdWatch_AnOptionalSinkThatNeverAnswersHoldsNothingBack(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	hooks := pwHooksFile(t, map[string]string{"w1": h.srv.URL + "/hook", "bh": pwBlackhole(t) + "/hooks/x"})
	in := pwNotifyIn(pwProbeAlerts(6), []map[string]any{{"webhook": "w1", "channel": "#ops", "min_severity": "low", "required": true},
		{"webhook": "bh", "channel": "#backup", "min_severity": "low", "required": false}})
	for r := 0; r < 2; r++ {
		n := len(h.bodies())
		start := time.Now()
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, in, nil, map[string]string{"webhooks": hooks}))
		if err != nil || out["consume"] != true {
			t.Fatalf("run %d: an optional sink that never answers held the tick back: %v %v %s", r, out["consume"], err, stderr)
		}
		got := strings.Join(h.bodies()[n:], "\n")
		for k := 0; k < 6; k++ {
			if !strings.Contains(got, fmt.Sprint("`api", k, "`")) {
				t.Fatalf("run %d: the required sink missed api%d", r, k)
			}
		}
		if took := time.Since(start); took > 30*time.Second {
			t.Fatalf("run %d: the optional sink cost %v (one timeout, then skipped)", r, took)
		}
	}
}

// TestProdWatch_ASlowRequiredSinkGetsTheBudgetsTime: a required sink under load
// (4.5 s a post) takes a full tick — the cap's twenty alerts and a note — within
// the time the run's budget keeps for the delivery: consumed, not replayed.
func TestProdWatch_ASlowRequiredSinkGetsTheBudgetsTime(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(4500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	hooks := pwHooksFile(t, map[string]string{"w1": slow.URL + "/hooks/x"})
	in := pwNotifyIn(pwProbeAlerts(20), []map[string]any{{"webhook": "w1", "channel": "#ops", "min_severity": "low", "required": true}})
	in["stale_sources"] = []any{map[string]any{"source": "coverage", "hours": -1, "last_ok": "partial", "reasons": "loki: truncated"}}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, in, nil, map[string]string{"webhooks": hooks}))
	if err != nil || out["consume"] != true {
		t.Fatalf("21 messages to a required sink answering in 4.5 s: not consumed (the tick would replay): %v %v %s", out["consume"], err, lastN(stderr, 300))
	}
}

// TestProdWatch_ASlowOptionalSinkStarvesNothing: an optional sink trickling its
// answers never fails the required one: the tick is consumed.
func TestProdWatch_ASlowOptionalSinkStarvesNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var got atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)
	hooks := pwHooksFile(t, map[string]string{"w1": fast.URL + "/hooks/x", "slow": pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60)) + "/hooks/y"})
	in := pwNotifyIn(pwProbeAlerts(6), []map[string]any{{"webhook": "slow", "channel": "#backup", "min_severity": "low", "required": false},
		{"webhook": "w1", "channel": "#ops", "min_severity": "low", "required": true}})
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, in, nil, map[string]string{"webhooks": hooks}))
	if err != nil || out["consume"] != true || got.Load() != 6 {
		t.Fatalf("an optional sink trickling its answers: want the required one to get 6 of 6 and the tick consumed, got %d, %v %v %s",
			got.Load(), out["consume"], err, lastN(stderr, 300))
	}
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
