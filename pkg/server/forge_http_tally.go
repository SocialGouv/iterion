package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// forgeLaneGateSweeper labels the forge requests the merge-gate sweep sends —
// its reconcile and autofix offers, and whatever those spend on the way.
const forgeLaneGateSweeper = "merge-gate-sweeper"

type forgeLaneKey struct{}

// withForgeLane labels every forge request sent under ctx with lane, for the
// hourly tally (forgeRequestTally).
func withForgeLane(ctx context.Context, lane string) context.Context {
	return context.WithValue(ctx, forgeLaneKey{}, lane)
}

// forgeLaneOf reads the lane withForgeLane put on ctx; "" when none.
func forgeLaneOf(ctx context.Context) string {
	lane, _ := ctx.Value(forgeLaneKey{}).(string)
	return lane
}

// forgeTallyKey is one line of the tally: which forge host, which API (REST
// and GraphQL are separate budgets on GitHub), which lane.
type forgeTallyKey struct {
	host, api, lane string
}

// forgeTallyMaxHosts bounds the distinct hosts one hour names. The host is the
// unbounded part of a key — a self-hosted forge chooses where its redirects
// point — so past the bound a NEW host is counted under forgeTallyOtherHosts;
// a host already named keeps its own entries, whatever lane or API it adds.
const (
	forgeTallyMaxHosts   = 64
	forgeTallyOtherHosts = "(other hosts)"
)

// forgeRequestTally counts the forge HTTP requests sent through the forge
// client (forgeHTTPClient) — every call made with a forge connection's
// credential, token mints included — and reports each hour's counts at Info.
//
// A forge's budget is spent by requests, and nothing else in the process
// counts them: without this line, "how many calls does the sweeper make" is a
// model, not a measurement. It counts ATTEMPTS as the client's transport sees
// them: each redirect hop and each answer whatever its status (a rate-limited
// 403 included), but also an attempt that never reached the forge (a refused
// dial), and not the reconnect-retries net/http performs below it. Close to
// what the forge bills, not identical: a JWT token mint is counted, and does
// not spend the installation's REST budget.
type forgeRequestTally struct {
	now    func() time.Time
	report func(format string, args ...any)
	// start is when counting began: an hour that began before it was only
	// partly seen, and its report says so.
	start time.Time

	mu     sync.Mutex
	hour   time.Time // the hour being counted, truncated
	counts map[forgeTallyKey]int64
	hosts  map[string]bool // the hosts counted under their own name this hour
}

func newForgeRequestTally(now func() time.Time, report func(format string, args ...any)) *forgeRequestTally {
	return &forgeRequestTally{now: now, report: report, start: now().UTC(), counts: map[forgeTallyKey]int64{}, hosts: map[string]bool{}}
}

// observe counts one request. The first request of a new hour reports the
// previous one, so an hour is reported with the next request after it; a
// stopping process reports its last hour through flush, and an hour with no
// request at all has nothing to report.
func (t *forgeRequestTally) observe(req *http.Request) {
	key := forgeTallyKey{host: req.URL.Host, api: forgeAPIOf(req.URL.Path), lane: forgeLaneOf(req.Context())}
	now := t.now().UTC()
	hour := now.Truncate(time.Hour)

	t.mu.Lock()
	var (
		doneHour time.Time
		done     map[forgeTallyKey]int64
	)
	if hour.After(t.hour) {
		if len(t.counts) > 0 {
			doneHour, done = t.hour, t.counts
			t.counts, t.hosts = map[forgeTallyKey]int64{}, map[string]bool{}
		}
		t.hour = hour
	}
	if !t.hosts[key.host] {
		if len(t.hosts) >= forgeTallyMaxHosts {
			key.host = forgeTallyOtherHosts
		} else {
			t.hosts[key.host] = true
		}
	}
	t.counts[key]++
	t.mu.Unlock()

	if done != nil && t.report != nil {
		t.report("%s", formatForgeTally(doneHour, t.start, time.Time{}, done))
	}
}

// flush reports the hour being counted. A stopping process owes it: nothing
// would turn its last hour, and a fleet summing its hourly lines would lose
// every stopping pod's. When the stop falls inside that hour, the line says
// the hour was cut short there; an hour that ended before the stop — the last
// one with requests, after an idle gap — is whole.
func (t *forgeRequestTally) flush() {
	t.mu.Lock()
	now := t.now().UTC()
	hour, counts := t.hour, t.counts
	t.counts, t.hosts = map[forgeTallyKey]int64{}, map[string]bool{}
	t.mu.Unlock()
	if len(counts) == 0 || t.report == nil {
		return
	}
	var until time.Time
	if !now.Before(hour) && now.Before(hour.Add(time.Hour)) {
		until = now
	}
	t.report("%s", formatForgeTally(hour, t.start, until, counts))
}

// forgeAPIOf tells a GraphQL call from a REST one by its endpoint — /graphql
// on github.com, /api/graphql on GitHub Enterprise Server and GitLab — never
// by a suffix, which a REST path can carry (a repository or a file named
// "graphql").
func forgeAPIOf(path string) string {
	p := strings.TrimRight(path, "/")
	if p == "/graphql" || p == "/api/graphql" {
		return "graphql"
	}
	return "rest"
}

// formatForgeTally renders one hour: the total, then one entry per
// host/API/lane, largest first. The hour carries its date, so a line reported
// late — after an idle gap — is never read as a later hour. An hour that began
// before counting did says where its count began, and one cut short by a stop
// says where it ended, so a partial hour is never read as a whole one.
func formatForgeTally(hour, start, until time.Time, counts map[forgeTallyKey]int64) string {
	type line struct {
		key forgeTallyKey
		n   int64
	}
	var (
		lines []line
		total int64
	)
	for k, n := range counts {
		lines = append(lines, line{k, n})
		total += n
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].n != lines[j].n {
			return lines[i].n > lines[j].n
		}
		a, b := lines[i].key, lines[j].key
		return a.host+"\x00"+a.api+"\x00"+a.lane < b.host+"\x00"+b.api+"\x00"+b.lane
	})
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		lane := l.key.lane
		if lane == "" {
			lane = "other"
		}
		parts = append(parts, fmt.Sprintf("%s %s %s=%d", l.key.host, l.key.api, lane, l.n))
	}
	window := "the hour ending " + hour.Add(time.Hour).Format("2006-01-02T15:04Z")
	var partial []string
	if start.After(hour) {
		partial = append(partial, "counted from "+start.Format("15:04:05Z"))
	}
	if !until.IsZero() {
		partial = append(partial, "until "+until.Format("15:04:05Z")+", stopping")
	}
	if len(partial) > 0 {
		window += " (" + strings.Join(partial, ", ") + ")"
	}
	return fmt.Sprintf("forge HTTP: %d requests in %s — %s", total, window, strings.Join(parts, ", "))
}

// flushForgeTally reports this process's last, partial hour of forge
// requests. Shutdown calls it once the background loops are joined, so the
// sweep's last requests are in it.
func (s *Server) flushForgeTally() {
	s.forgeHTTPClient() // builds the tally if nothing did; orders the read after it
	s.forgeRequests.flush()
}

// countingTransport is the forge HTTP client's transport, counted.
type countingTransport struct {
	base  http.RoundTripper
	tally *forgeRequestTally
}

func (c countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.tally.observe(req)
	return c.base.RoundTrip(req)
}
