package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
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
// and GraphQL are separate budgets on GitHub), which lane, and which GitHub
// App installation's token authenticated the request (forge.InstallationOf;
// 0 for any other credential).
type forgeTallyKey struct {
	host, api, lane string
	installation    int64
}

// less orders two keys with the same count: by host, API and lane, then
// installation.
func (k forgeTallyKey) less(o forgeTallyKey) bool {
	if a, b := k.host+"\x00"+k.api+"\x00"+k.lane, o.host+"\x00"+o.api+"\x00"+o.lane; a != b {
		return a < b
	}
	return k.installation < o.installation
}

// forgeBudgetKey names one rate-limit budget: an installation's REST or
// GraphQL budget on one host. The lane is no part of it — every lane spends
// the same budget.
type forgeBudgetKey struct {
	host, api    string
	installation int64
}

// forgeBudget is one reading of a budget: what was left, out of how much
// (0 when the forge did not say), when the answer was seen, and when the forge
// said its window resets (zero when it did not).
type forgeBudget struct {
	remaining, limit int64
	seen, reset      time.Time
}

// lowerThan orders two readings of one budget: the lower remaining first, then
// the earlier seen, the earlier reset, the lower limit. The order is total, so
// an hour's lowest reading is the same whatever order concurrent answers
// arrive in.
func (b forgeBudget) lowerThan(o forgeBudget) bool {
	switch {
	case b.remaining != o.remaining:
		return b.remaining < o.remaining
	case !b.seen.Equal(o.seen):
		return b.seen.Before(o.seen)
	case !b.reset.Equal(o.reset):
		return b.reset.Before(o.reset)
	default:
		return b.limit < o.limit
	}
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
// credential, token mints included — and reports each hour's counts at Info,
// followed by the lowest rate-limit budget each GitHub App installation
// reported that hour.
//
// A forge's budget is spent by requests, and nothing else in the process
// counts them: without this line, "how many calls does the sweeper make" is a
// model, not a measurement. It counts ATTEMPTS as the client's transport sees
// them: each redirect hop and each answer whatever its status (a rate-limited
// 403 included), but also an attempt that never reached the forge (a refused
// dial), and not the reconnect-retries net/http performs below it. Close to
// what the forge bills, not identical: a JWT token mint is counted, and does
// not spend the installation's REST budget.
//
// The budget line is what the count cannot say: how close an installation
// came to running out. GitHub reports the budget on every answer, 2xx
// included, so each hour keeps, per installation and API, the lowest remaining
// any answer reported, when it was seen and when that window resets. Only a
// request sent with an installation's token is read: an answer to any other
// credential — a PAT, a user token, the App's own JWT, a bot's forge_token
// binding whose installation the sender cannot name — reports a budget the
// tally cannot name, and a minimum taken across budgets it cannot tell apart
// would describe none of them.
type forgeRequestTally struct {
	now    func() time.Time
	report func(format string, args ...any)
	// start is when counting began: an hour that began before it was only
	// partly seen, and its report says so.
	start time.Time

	mu      sync.Mutex
	hour    time.Time // the hour being counted, truncated
	counts  map[forgeTallyKey]int64
	budgets map[forgeBudgetKey]forgeBudget // each budget's lowest reading this hour
	hosts   map[string]bool                // the hosts named under their own name this hour
}

func newForgeRequestTally(now func() time.Time, report func(format string, args ...any)) *forgeRequestTally {
	return &forgeRequestTally{
		now: now, report: report, start: now().UTC(),
		counts: map[forgeTallyKey]int64{}, budgets: map[forgeBudgetKey]forgeBudget{}, hosts: map[string]bool{},
	}
}

// forgeTallyHour is what one finished hour counted, handed out of the lock to
// be reported.
type forgeTallyHour struct {
	hour    time.Time
	counts  map[forgeTallyKey]int64
	budgets map[forgeBudgetKey]forgeBudget
}

// turnLocked moves the tally to now's hour and hands back the hour it was
// counting when that one is over and saw anything. t.mu is held.
func (t *forgeRequestTally) turnLocked(now time.Time) (done forgeTallyHour) {
	hour := now.Truncate(time.Hour)
	if !hour.After(t.hour) {
		return forgeTallyHour{}
	}
	if len(t.counts) > 0 || len(t.budgets) > 0 {
		done = forgeTallyHour{hour: t.hour, counts: t.counts, budgets: t.budgets}
		t.counts, t.budgets, t.hosts = map[forgeTallyKey]int64{}, map[forgeBudgetKey]forgeBudget{}, map[string]bool{}
	}
	t.hour = hour
	return done
}

// hostLocked is the name host is reported under this hour: its own, unless
// the hour already names forgeTallyMaxHosts others. t.mu is held.
func (t *forgeRequestTally) hostLocked(host string) string {
	if t.hosts[host] {
		return host
	}
	if len(t.hosts) >= forgeTallyMaxHosts {
		return forgeTallyOtherHosts
	}
	t.hosts[host] = true
	return host
}

// observe counts one request. The first request of a new hour reports the
// previous one, so an hour is reported with the next request after it; a
// stopping process reports its last hour through flush, and an hour with no
// request and no answer has nothing to report.
func (t *forgeRequestTally) observe(req *http.Request) {
	installation, _ := forge.InstallationOf(req.Context())
	key := forgeTallyKey{host: req.URL.Host, api: forgeAPIOf(req.URL.Path), lane: forgeLaneOf(req.Context()), installation: installation}
	now := t.now().UTC()

	t.mu.Lock()
	done := t.turnLocked(now)
	key.host = t.hostLocked(key.host)
	t.counts[key]++
	t.mu.Unlock()

	t.reportHour(done, time.Time{})
}

// observeBudget keeps the budget an answer to req reports, when req was sent
// with an installation's token: the hour holds each budget's lowest reading.
// The answer turns the hour like a request does — an answer arriving after
// the hour its request was counted in is a reading of the new hour.
func (t *forgeRequestTally) observeBudget(req *http.Request, hdr http.Header) {
	installation, ok := forge.InstallationOf(req.Context())
	if !ok {
		return
	}
	now := t.now().UTC()
	b, ok := forge.RateLimitBudgetOf(hdr, now)
	if !ok {
		return
	}
	reading := forgeBudget{remaining: b.Remaining, limit: b.Limit, seen: now, reset: b.ResetAt}
	key := forgeBudgetKey{host: req.URL.Host, api: forgeAPIOf(req.URL.Path), installation: installation}

	t.mu.Lock()
	done := t.turnLocked(now)
	key.host = t.hostLocked(key.host)
	if lowest, ok := t.budgets[key]; !ok || reading.lowerThan(lowest) {
		t.budgets[key] = reading
	}
	t.mu.Unlock()

	t.reportHour(done, time.Time{})
}

// flush reports the hour being counted. A stopping process owes it: nothing
// would turn its last hour, and a fleet summing its hourly lines would lose
// every stopping pod's. When the stop falls inside that hour, the line says
// the hour was cut short there; an hour that ended before the stop — the last
// one with requests, after an idle gap — is whole.
func (t *forgeRequestTally) flush() {
	t.mu.Lock()
	now := t.now().UTC()
	done := forgeTallyHour{hour: t.hour, counts: t.counts, budgets: t.budgets}
	t.counts, t.budgets, t.hosts = map[forgeTallyKey]int64{}, map[forgeBudgetKey]forgeBudget{}, map[string]bool{}
	t.mu.Unlock()
	var until time.Time
	if !now.Before(done.hour) && now.Before(done.hour.Add(time.Hour)) {
		until = now
	}
	t.reportHour(done, until)
}

// reportHour reports a finished hour: its count, then its budgets — each line
// only when the hour has something for it.
func (t *forgeRequestTally) reportHour(done forgeTallyHour, until time.Time) {
	if t.report == nil {
		return
	}
	if len(done.counts) > 0 {
		t.report("%s", formatForgeTally(done.hour, t.start, until, done.counts))
	}
	if len(done.budgets) > 0 {
		t.report("%s", formatForgeBudgets(done.hour, t.start, until, done.budgets))
	}
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

// forgeTallyWindow names the hour a line reports. The hour carries its date,
// so a line reported late — after an idle gap — is never read as a later hour.
// An hour that began before counting did says where its count began, and one
// cut short by a stop says where it ended, so a partial hour is never read as
// a whole one.
func forgeTallyWindow(hour, start, until time.Time) string {
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
	return window
}

// formatForgeTally renders one hour: the total, then one entry per
// host/API/installation/lane, largest first. An entry sent with an
// installation's token names it; any other keeps the host/API/lane form.
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
		return lines[i].key.less(lines[j].key)
	})
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		lane := l.key.lane
		if lane == "" {
			lane = "other"
		}
		api := l.key.api
		if l.key.installation != 0 {
			api += " installation " + strconv.FormatInt(l.key.installation, 10)
		}
		parts = append(parts, fmt.Sprintf("%s %s %s=%d", l.key.host, api, lane, l.n))
	}
	return fmt.Sprintf("forge HTTP: %d requests in %s — %s", total, forgeTallyWindow(hour, start, until), strings.Join(parts, ", "))
}

// formatForgeBudgets renders one hour's budgets: for each installation's REST
// or GraphQL budget, the lowest remaining the hour saw, out of how much, when
// it was seen and when its window resets, ordered by host, API and
// installation. A limit or a reset the forge did not report is left out,
// never guessed.
func formatForgeBudgets(hour, start, until time.Time, budgets map[forgeBudgetKey]forgeBudget) string {
	keys := make([]forgeBudgetKey, 0, len(budgets))
	for k := range budgets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.host != b.host {
			return a.host < b.host
		}
		if a.api != b.api {
			return a.api < b.api
		}
		return a.installation < b.installation
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		b := budgets[k]
		level := strconv.FormatInt(b.remaining, 10)
		if b.limit > 0 {
			level += " of " + strconv.FormatInt(b.limit, 10)
		}
		entry := fmt.Sprintf("%s %s installation %d: %s at %s", k.host, k.api, k.installation, level, b.seen.UTC().Format("15:04:05Z"))
		if !b.reset.IsZero() {
			entry += " (resets " + b.reset.UTC().Format("15:04:05Z") + ")"
		}
		parts = append(parts, entry)
	}
	return fmt.Sprintf("forge budget: lowest remaining in %s — %s", forgeTallyWindow(hour, start, until), strings.Join(parts, ", "))
}

// flushForgeTally reports this process's last, partial hour of forge
// requests. Shutdown calls it once the background loops are joined, so the
// sweep's last requests are in it.
func (s *Server) flushForgeTally() {
	s.forgeHTTPClient() // builds the tally if nothing did; orders the read after it
	s.forgeRequests.flush()
}

// countingTransport is the forge HTTP client's transport, counted. It is the
// one place the hourly report reads: the request as it leaves, and the budget
// its answer reports — every answer, a redirect hop's and a rate-limited
// 403's included.
type countingTransport struct {
	base  http.RoundTripper
	tally *forgeRequestTally
}

func (c countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.tally.observe(req)
	resp, err := c.base.RoundTrip(req)
	if err == nil {
		c.tally.observeBudget(req, resp.Header)
	}
	return resp, err
}
