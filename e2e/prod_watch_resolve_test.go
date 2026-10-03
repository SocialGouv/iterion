package e2e

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// pwSlowResolverStub makes getaddrinfo take `delay` seconds for `host` (then
// answer 127.0.0.1:port) — a resolver half dead — and routes it around the
// sandbox's proxy.
func pwSlowResolverStub(host, port, delay string) string {
	return "import os as _o, socket as _s, time as _t\n" +
		"for _k in ('no_proxy', 'NO_PROXY'):\n" +
		"    _o.environ[_k] = '" + host + ",' + _o.environ.get(_k, '')\n" +
		"_gai0 = _s.getaddrinfo\n" +
		"def _slow(host, *a, **k):\n" +
		"    if host == '" + host + "':\n" +
		"        _t.sleep(" + delay + ")\n" +
		"        return [(_s.AF_INET, _s.SOCK_STREAM, 6, '', ('127.0.0.1', " + port + "))]\n" +
		"    return _gai0(host, *a, **k)\n" +
		"_s.getaddrinfo = _slow\n"
}

// TestProdWatch_ASlowResolverIsBoundedByTheNodesClock: getaddrinfo is a C call
// no alarm interrupts; a resolver answering in 30 s fails a health probe of
// timeout_secs 2 in about two seconds, named — never 30 s outside every clock.
func TestProdWatch_ASlowResolverIsBoundedByTheNodesClock(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	// The lane default is 30: the probe's own timeout_secs (2) bounds its lookup.
	in := map[string]any{"probes": []map[string]any{{"id": "api", "url": "http://slowdns.test:9/health", "expect_status": 200,
		"timeout_secs": 2, "severity": "critical"}}, "timeout_secs": 30, "allow_private": true}
	start := time.Now()
	out, stderr, err := runPyWhole(t, h.ws, pwSlowResolverStub("slowdns.test", "9", "30")+pwSub(t, pwTool(t, wf, "probe_http").Script, in, nil, nil))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("probe_http: %v %s", err, stderr)
	}
	if took > 8*time.Second {
		t.Fatalf("a probe of timeout_secs 2 behind a resolver answering in 30 s took %v", took)
	}
	if !strings.Contains(fmt.Sprint(out), "resolving") {
		t.Fatalf("the probe did not fail on its bounded lookup: %v", out)
	}
}

// TestProdWatch_AGrafanaLaneDeadlineCountsItsLookup: a Grafana lane's deadline
// starts with the node — the lookup of its host is inside it: a resolver
// answering in 4 s leaves the lane 6 s of a 10 s deadline, not 10 more.
func TestProdWatch_AGrafanaLaneDeadlineCountsItsLookup(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	raw := pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	start := time.Now()
	out, stderr, err := runPyWhole(t, h.ws, pwSlowResolverStub("slowgraf.test", port, "4")+
		pwSub(t, pwTool(t, wf, "poll_prom").Script, pwPromInputs("http://slowgraf.test:"+port, 10, 1, 5), nil,
			map[string]string{"grafana_token": h.tokenFile}))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("poll_prom: %v %s", err, stderr)
	}
	if took > 12500*time.Millisecond {
		t.Fatalf("a 10 s lane deadline behind a 4 s lookup ran %v: the lookup was outside the deadline", took)
	}
	if !strings.Contains(fmt.Sprint(out["errors"]), "ExchangeTimeout") {
		t.Fatalf("the lane did not stop on its deadline: %v", out["errors"])
	}
}

// TestProdWatch_TheResolverIsTheSameInEveryNode: the bounded resolver is one
// body copied into every node that resolves a name (each script runs alone);
// only its timeout differs.
func TestProdWatch_TheResolverIsTheSameInEveryNode(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "bots", "prod-watch", "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile(`(?s)    ## Name resolution is bounded.*?socket\.getaddrinfo = _bounded_getaddrinfo\n`).FindAll(src, -1)
	if len(blocks) != 7 {
		t.Fatalf("want the resolver in 7 nodes (release, loki, prometheus, sentry, galerts, probes, notify), found %d", len(blocks))
	}
	norm := regexp.MustCompile(`_gai_timeout = socket\.getaddrinfo, \{\}, \[[^\]]*\]|_gai, _gai_hosts, _gai_timeout = socket\.getaddrinfo, \{\}, \[[^\]]*\]`)
	first := norm.ReplaceAllString(string(blocks[0]), "")
	for i, b := range blocks[1:] {
		if norm.ReplaceAllString(string(b), "") != first {
			t.Fatalf("resolver copy %d differs from the first", i+2)
		}
	}
}
