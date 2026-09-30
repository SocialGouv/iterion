package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The fake Sentry is deliberately LESS kind than the real one (24.11.1, the
// sources this was written against): counts are strings, dates end in Z and
// sometimes carry microseconds, shortId/substatus/lastSeen may be null, a
// level may be `sample`, the payload's firstSeen is an EVENT time that differs
// from the processing time the `firstSeen:>=` filter reads, a list page may be
// short while `results="true"`, the Link URLs point at a foreign host, there
// is no X-Hits header, the permalink is hostile, and every list call must
// carry the parameters the bot promises (a call without `query` is refused,
// as the real default query would silently hide low-priority issues).

const pwSentryToken = "sentry-test-token-0123456789abcdef"

// pwSentryOff: the Sentry lane's node inputs of a tick whose config has no
// sentry section. A test that drives leak_scan, decide or notify alone and is
// not about Sentry gets these (pwSub fills them); tick() and the Sentry tests
// pass their own.
var pwSentryOff = map[string]any{
	"sentry": map[string]any{"enabled": false}, "sentry_ok": true, "sentry_truncated": false,
	"sentry_errors": []any{}, "sentry_walk": map[string]any{}, "sentry_issues": 0, "sentry_file": "",
}

type pwSentryAct struct {
	Type string
	At   time.Time
}

type pwSentryIssue struct {
	ID             string
	ShortID        *string
	Title, Culprit string
	Level          string
	Status         string
	Substatus      *string
	FirstProcessed time.Time // groupenvironment.first_seen: what `firstSeen:>=` filters on
	LastSeen       time.Time // zero: null
	Count, Users   int
	Meta           map[string]any
	Env            string
	Acts           []pwSentryAct
	Raw            map[string]string // payload fields sent verbatim as JSON: a lone surrogate, a count in foreign digits
}

type pwSentryCall struct {
	Path string
	Q    url.Values
	At   time.Time
}

type pwSentry struct {
	mu             sync.Mutex
	issues         map[string]*pwSentryIssue
	envs           map[string]bool
	calls          []pwSentryCall
	fail           map[string][]int // endpoint kind → statuses answered first, in order
	shortFirstPage bool             // page 1 of every list drops its last issue while results="true"
	badCursor      bool
	pageSize       int
	delay          time.Duration // added to every list call (the deadline test)
	redirectTo     string        // the project lookup answers 302 to this URL
	failBody       string        // the body of a failNext answer (server text the lane must withhold)
	dateOffset     time.Duration // the Date header is the server's clock shifted by this
	rawDate        string        // the Date header, verbatim (a broken server or proxy)
	noLink         bool          // list pages carry no Link header (a proxy that strips it)
}

func strp(s string) *string { return &s }

func (s *pwSentry) put(i *pwSentryIssue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i.Env == "" {
		i.Env = "preprod"
	}
	if i.Status == "" {
		i.Status = "unresolved"
	}
	if i.Level == "" {
		i.Level = "error"
	}
	cp := *i
	s.issues[i.ID] = &cp
}

func (s *pwSentry) edit(id string, f func(i *pwSentryIssue)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.issues[id])
}

func (s *pwSentry) failNext(kind string, statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[kind] = append(s.fail[kind], statuses...)
}

func (s *pwSentry) callsTo(kind string) []pwSentryCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pwSentryCall
	for _, c := range s.calls {
		if pwSentryKind(c.Path, c.Q) == kind {
			out = append(out, c)
		}
	}
	return out
}

func pwSentryKind(path string, q url.Values) string {
	switch {
	case strings.HasSuffix(path, "/activities/"):
		return "activities"
	case strings.Contains(path, "/environments/"):
		return "env"
	case strings.HasPrefix(path, "/api/0/projects/"):
		return "project"
	case len(q["group"]) > 0:
		return "tracked"
	default:
		return "list"
	}
}

func pwSentryTime(t time.Time, micro bool) any {
	if t.IsZero() {
		return nil
	}
	if micro {
		return t.UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func (s *pwSentry) payload(i *pwSentryIssue, env string) map[string]any {
	last := i.LastSeen
	if env != "" && i.Env != env {
		last = time.Time{} // the by-id answer of an issue without events in that environment
	}
	var short, sub any
	if i.ShortID != nil {
		short = *i.ShortID
	}
	if i.Substatus != nil && i.Status != "resolved" {
		sub = *i.Substatus // resolving clears it, as the real server does
	}
	meta := i.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	p := map[string]any{
		"id": i.ID, "shortId": short, "title": i.Title, "culprit": i.Culprit, "level": i.Level, "status": i.Status,
		"substatus": sub, "issueCategory": "error", "issueType": "error", "priority": "low", "isUnhandled": true,
		// An EVENT time a minute before the processing time: the bot must never classify on it.
		"firstSeen": pwSentryTime(i.FirstProcessed.Add(-time.Minute), true), "lastSeen": pwSentryTime(last, false),
		"count": strconv.Itoa(i.Count), "userCount": i.Users, "metadata": meta,
		"permalink": "https://evil.invalid/share/issue/" + i.ID + "/",
		"project":   map[string]any{"id": "63", "slug": "proj"},
	}
	for k, v := range i.Raw {
		p[k] = json.RawMessage(v)
	}
	return p
}

func (h *pwHarness) mountSentry(mux *http.ServeMux) {
	s := &pwSentry{issues: map[string]*pwSentryIssue{}, envs: map[string]bool{"preprod": true}, fail: map[string][]int{}, pageSize: 100}
	h.sentry = s
	serve := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pwSentryToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		kind := pwSentryKind(r.URL.Path, q)
		s.mu.Lock()
		s.calls = append(s.calls, pwSentryCall{Path: r.URL.Path, Q: q, At: time.Now()})
		if s.dateOffset != 0 {
			w.Header().Set("Date", time.Now().Add(s.dateOffset).UTC().Format(http.TimeFormat))
		}
		if s.rawDate != "" {
			w.Header().Set("Date", s.rawDate)
		}
		if st := s.fail[kind]; len(st) > 0 {
			s.fail[kind] = st[1:]
			body := s.failBody
			s.mu.Unlock()
			w.WriteHeader(st[0])
			_, _ = w.Write([]byte(body))
			return
		}
		delay := s.delay
		redirect := s.redirectTo
		s.mu.Unlock()
		if redirect != "" && kind == "project" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		notFound := func() {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "The requested resource does not exist"})
		}
		switch kind {
		case "project":
			if r.URL.Path != "/api/0/projects/org/proj/" {
				notFound()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "63", "slug": "proj", "name": "proj"})
			return
		case "env":
			name, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/api/0/projects/org/proj/environments/"), "/"))
			s.mu.Lock()
			ok := s.envs[name]
			s.mu.Unlock()
			if !ok {
				notFound()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "5", "name": name, "isHidden": false})
			return
		case "activities":
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/0/organizations/org/issues/"), "/activities/")
			s.mu.Lock()
			i, ok := s.issues[id]
			var acts []map[string]any
			if ok {
				sorted := append([]pwSentryAct(nil), i.Acts...)
				sort.Slice(sorted, func(a, b int) bool { return sorted[a].At.After(sorted[b].At) })
				for _, a := range sorted {
					acts = append(acts, map[string]any{"type": a.Type, "dateCreated": pwSentryTime(a.At, true), "data": map[string]any{}})
				}
			}
			s.mu.Unlock()
			if !ok {
				notFound()
				return
			}
			if acts == nil {
				acts = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"activity": acts})
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if q.Get("project") != "63" || q.Get("statsPeriod") != "14d" || q.Get("collapse") != "stats" || q.Get("limit") != "100" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "fake: unexpected parameters " + q.Encode()})
			return
		}
		env := q.Get("environment")
		s.mu.Lock()
		defer s.mu.Unlock()
		if env != "" && !s.envs[env] {
			notFound()
			return
		}
		var sel []*pwSentryIssue
		if ids := q["group"]; len(ids) > 0 {
			for _, id := range ids {
				if i, ok := s.issues[id]; ok {
					sel = append(sel, i)
				}
			}
			out := []map[string]any{}
			for _, i := range sel {
				out = append(out, s.payload(i, env))
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		query, has := q["query"]
		if !has {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "fake: a list call without query would get the default priority filter"})
			return
		}
		switch {
		case strings.HasPrefix(query[0], "is:unresolved firstSeen:>="):
			since, err := time.Parse(time.RFC3339, strings.TrimPrefix(query[0], "is:unresolved firstSeen:>="))
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, i := range s.issues {
				if i.Status == "unresolved" && (env == "" || i.Env == env) && !i.FirstProcessed.Before(since) {
					sel = append(sel, i)
				}
			}
		case query[0] == "is:unresolved substatus:[regressed,escalating]":
			for _, i := range s.issues {
				if i.Status == "unresolved" && (env == "" || i.Env == env) && i.Substatus != nil && (*i.Substatus == "regressed" || *i.Substatus == "escalating") {
					sel = append(sel, i)
				}
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "fake: unknown query " + query[0]})
			return
		}
		switch q.Get("sort") {
		case "new":
			sort.Slice(sel, func(a, b int) bool {
				if !sel[a].FirstProcessed.Equal(sel[b].FirstProcessed) {
					return sel[a].FirstProcessed.After(sel[b].FirstProcessed)
				}
				return sel[a].ID < sel[b].ID
			})
		case "date":
			sort.Slice(sel, func(a, b int) bool {
				if !sel[a].LastSeen.Equal(sel[b].LastSeen) {
					return sel[a].LastSeen.After(sel[b].LastSeen)
				}
				return sel[a].ID < sel[b].ID
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		offset := 0
		if c := q.Get("cursor"); c != "" {
			parts := strings.Split(c, ":")
			if len(parts) != 3 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			offset, _ = strconv.Atoi(parts[1])
		}
		size := s.pageSize
		end := offset + size
		if end > len(sel) {
			end = len(sel)
		}
		page := []*pwSentryIssue{}
		if offset < len(sel) {
			page = sel[offset:end]
		}
		if s.shortFirstPage && offset == 0 && len(page) == size && size > 1 {
			page = page[:size-1] // an auto-resolved issue dropped after pagination
		}
		more := end < len(sel)
		next := "0:" + strconv.Itoa(end) + ":0"
		if s.badCursor {
			next = "../../x"
		}
		if !s.noLink {
			w.Header().Set("Link", fmt.Sprintf(`<https://evil.invalid/api/0/organizations/org/issues/?cursor=0:0:1>; rel="previous"; results="false"; cursor="0:0:1", `+
				`<https://evil.invalid/api/0/organizations/org/issues/?cursor=%s>; rel="next"; results="%t"; cursor="%s"`, next, more, next))
		}
		out := []map[string]any{}
		for _, i := range page {
			out = append(out, s.payload(i, env))
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	mux.HandleFunc("/api/0/projects/", serve)
	mux.HandleFunc("/api/0/organizations/", serve)
}

func (h *pwHarness) maxAlerts() int {
	if n := h.alertCap.Load(); n > 0 {
		return int(n)
	}
	return 20
}

// setMaxPerLane sets tick()'s max_alerts_per_lane (0 folds every alert of a
// minting lane into the note of its kind).
func (h *pwHarness) setMaxPerLane(n int) { h.laneCap.Store(int64(n) + 1) }

func (h *pwHarness) maxPerLane() int {
	if n := h.laneCap.Load(); n > 0 {
		return int(n) - 1
	}
	return 5
}

func (h *pwHarness) maxMsgChars() int {
	if n := h.msgChars.Load(); n > 0 {
		return int(n)
	}
	return 14000
}

// sentryOnly: a config whose only lane is Sentry (plus the healthy sink), so
// a test sees the Sentry lane's alerts and nothing else.
func sentryOnly(h *pwHarness, mod func(s map[string]any)) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		delete(cfg, "grafana")
		delete(cfg, "loki")
		delete(cfg, "prometheus")
		delete(cfg, "probes")
		delete(cfg, "release")
		s := map[string]any{"base_url": h.srv.URL, "org": "org", "project": "proj", "environment": "preprod",
			"min_level": "error", "overlap_minutes": 60}
		if mod != nil {
			mod(s)
		}
		cfg["sentry"] = s
	}
}

// sentryAlerts returns decide's Sentry alerts as "state:shortid:severity".
func sentryAlerts(outs map[string]map[string]any) []string {
	var got []string
	for _, a := range outs["decide"]["alerts"].([]any) {
		m := a.(map[string]any)
		if m["kind"] != "sentry" && m["kind"] != "sentry_leak" {
			continue
		}
		ev, _ := m["evidence"].(map[string]any)
		id := fmt.Sprint(ev["short_id"])
		if m["kind"] == "sentry_leak" {
			id = "leak-" + fmt.Sprint(m["title_arg"])
		}
		got = append(got, fmt.Sprint(m["state"], ":", id, ":", m["severity"]))
	}
	sort.Strings(got)
	return got
}

func sentryTick(t *testing.T, h *pwHarness, wf *ir.Workflow) map[string]map[string]any {
	t.Helper()
	outs := h.tick(t, wf, false)
	if outs["poll_sentry"] == nil {
		t.Fatal("poll_sentry did not run")
	}
	return outs
}

func sentryIncident(t *testing.T, h *pwHarness, id string) map[string]any {
	t.Helper()
	inc, _ := h.state(t)["incidents"].(map[string]any)
	rec, _ := inc["sentry:"+id].(map[string]any)
	return rec
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProdWatch_SentryBootstrapIsSilentAndArms: the first tick records every
// issue it reads as backlog and posts nothing — backlog is not news — and
// arms the cursor with the lane's identity; poll_sentry really ran.
func TestProdWatch_SentryBootstrapIsSilentAndArms(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "101", ShortID: strp("PROJ-1"), Title: "ValueError: boom", FirstProcessed: now.Add(-10 * time.Minute), LastSeen: now.Add(-time.Minute), Count: 3})
	h.sentry.put(&pwSentryIssue{ID: "102", ShortID: strp("PROJ-2"), Title: "KeyError: x", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-2 * time.Minute), Count: 50,
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-5 * time.Minute)}}})
	outs := sentryTick(t, h, wf)
	if got := sentryAlerts(outs); len(got) != 0 {
		t.Fatalf("the bootstrap posted Sentry alerts: %v", got)
	}
	if len(h.sentry.callsTo("list")) == 0 {
		t.Fatal("the bootstrap never listed issues")
	}
	st := h.state(t)
	cur, _ := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	if cur == nil || cur["armed_at"] == "" || cur["since"] == "" {
		t.Fatalf("no armed Sentry cursor after the bootstrap: %v", st["cursors"])
	}
	id := cur["identity"].(map[string]any)
	if id["project"] != "proj" || id["environment"] != "preprod" || id["min_level"] != nil {
		t.Fatalf("the cursor does not carry the lane identity: %v", id)
	}
	for _, sid := range []string{"101", "102"} {
		rec := sentryIncident(t, h, sid)
		if rec == nil || rec["backlog"] != true || rec["alerted"] == true {
			t.Fatalf("issue %s is not recorded as silent backlog: %v", sid, rec)
		}
	}
	if rec := sentryIncident(t, h, "102"); rec["transition_at"] == nil {
		t.Fatalf("the bootstrap did not record the regression it saw: %v", rec)
	}
	// Nothing changed: the second tick posts nothing either.
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a steady tick after the bootstrap posted: %v", got)
	}
}

// TestProdWatch_SentryNewIssuePostsOnceWithItsLink: an issue first processed
// after the arming posts NEW once, with its scrubbed title, its level-mapped
// severity and ONE link under the configured prefix — never the API's
// permalink; a re-read in the overlap posts nothing.
func TestProdWatch_SentryNewIssuePostsOnceWithItsLink(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf) // bootstrap, empty
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "201", ShortID: strp("PROJ-7"), Title: "ZeroDivisionError: division by zero",
		Culprit: "app.billing in total", FirstProcessed: now, LastSeen: now, Count: 4, Users: 2})
	h.sentry.put(&pwSentryIssue{ID: "202", ShortID: strp("PROJ-8"), Title: "RuntimeError: fatal thing", Level: "fatal",
		FirstProcessed: now, LastSeen: now, Count: 1})
	outs := sentryTick(t, h, wf)
	if got, want := sentryAlerts(outs), []string{"new:PROJ-7:medium", "new:PROJ-8:high"}; !eqStrings(got, want) {
		t.Fatalf("new issues: got %v, want %v", got, want)
	}
	body := strings.Join(h.bodies(), "\n---\n")
	link := "[PROJ-7](" + h.srv.URL + "/organizations/org/issues/201/)"
	if !strings.Contains(body, link) {
		t.Fatalf("the message carries no link under the configured prefix (%s):\n%s", link, body)
	}
	if strings.Contains(body, "evil.invalid") {
		t.Fatalf("a URL the API sent reached the channel:\n%s", body)
	}
	if !strings.Contains(body, "ZeroDivisionError: division by zero") || !strings.Contains(body, "app.billing in total") {
		t.Fatalf("the message lacks the scrubbed title or culprit:\n%s", body)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("an issue re-read in the overlap posted again: %v", got)
	}
}

// TestProdWatch_SentryUntrackedRegressionPosts: resolved issues never appear
// in an unresolved list, so a regression arrives as an issue the lane never
// tracked, first seen long before the arming. Its activity date decides:
// after the arming it posts; before it (minus the overlap) it is backlog.
func TestProdWatch_SentryUntrackedRegressionPosts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf) // bootstrap, empty
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "301", ShortID: strp("PROJ-30"), Title: "OldBug: back", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: now, Count: 90, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	h.sentry.put(&pwSentryIssue{ID: "302", ShortID: strp("PROJ-31"), Title: "OlderBug: back before install", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: now, Count: 9, Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-3 * time.Hour)}}})
	h.sentry.put(&pwSentryIssue{ID: "303", ShortID: strp("PROJ-32"), Title: "Archived: spiking", Substatus: strp("escalating"),
		FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: now, Count: 900, Acts: []pwSentryAct{{Type: "set_escalating", At: now}}})
	outs := sentryTick(t, h, wf)
	if got, want := sentryAlerts(outs), []string{"escalating:PROJ-32:medium", "regressed:PROJ-30:medium"}; !eqStrings(got, want) {
		t.Fatalf("untracked transitions: got %v, want %v", got, want)
	}
	if rec := sentryIncident(t, h, "302"); rec == nil || rec["backlog"] != true {
		t.Fatalf("a regression dated before the arming is not silent backlog: %v", rec)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a transition posted twice: %v", got)
	}
}

// TestProdWatch_SentryTransitionWithoutANewEventStillPosts: the substatus is
// project-wide and the event count environment-scoped (and it lags the
// Postgres write), so a regression can arrive with no new event in the watched
// environment. The dated transition posts anyway.
func TestProdWatch_SentryTransitionWithoutANewEventStillPosts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "401", ShortID: strp("PROJ-40"), Title: "Bug", FirstProcessed: now.Add(-10 * time.Minute), LastSeen: now.Add(-5 * time.Minute), Count: 5})
	sentryTick(t, h, wf) // bootstrap: tracked as backlog
	h.sentry.edit("401", func(i *pwSentryIssue) {
		i.Substatus = strp("regressed") // same lastSeen, same count
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: time.Now()})
	})
	if got, want := sentryAlerts(sentryTick(t, h, wf)), []string{"regressed:PROJ-40:medium"}; !eqStrings(got, want) {
		t.Fatalf("a regression read without a new event: got %v, want %v", got, want)
	}
}

// TestProdWatch_SentryDeferredAlertsRefire: an alert the per-run cap cuts is
// PENDING and re-emitted until posted, although nothing new happens to the
// issue (it would otherwise be lost: known, not new, no event).
func TestProdWatch_SentryDeferredAlertsRefire(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "501", ShortID: strp("PROJ-50"), Title: "A", FirstProcessed: now, LastSeen: now, Count: 1})
	h.sentry.put(&pwSentryIssue{ID: "502", ShortID: strp("PROJ-51"), Title: "B", FirstProcessed: now, LastSeen: now, Count: 1})
	first := sentryAlerts(sentryTick(t, h, wf))
	second := sentryAlerts(sentryTick(t, h, wf))
	third := sentryAlerts(sentryTick(t, h, wf))
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] || len(third) != 0 {
		t.Fatalf("cap 1 with two new issues: ticks posted %v, %v, %v — want one each, then nothing", first, second, third)
	}
}

// TestProdWatch_SentrySecondRegressionAfterResolvePosts: a posted regression,
// then a resolve (the issue leaves every list), then a second regression — a
// new activity date — posts again; the recorded substatus alone would read it
// as one continuing regression.
func TestProdWatch_SentrySecondRegressionAfterResolvePosts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	t0 := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "601", ShortID: strp("PROJ-60"), Title: "Flaky", Substatus: strp("regressed"),
		FirstProcessed: t0.Add(-9 * 24 * time.Hour), LastSeen: t0, Count: 10, Acts: []pwSentryAct{{Type: "set_regression", At: t0}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:PROJ-60:medium"}) {
		t.Fatalf("first regression: %v", got)
	}
	h.sentry.edit("601", func(i *pwSentryIssue) { i.Status = "resolved" })
	sentryTick(t, h, wf)
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("601", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.LastSeen = t1
		i.Count++
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:PROJ-60:medium"}) {
		t.Fatalf("second regression after a resolve: %v", got)
	}
}

// TestProdWatch_SentryResolvedNoteOnceStillFollowed: an alerted issue Sentry
// reports resolved gets one note, and stays read by id — unresolved by hand
// (the issue page's button, a bulk action) it is ongoing, in neither list.
func TestProdWatch_SentryResolvedNoteOnceStillFollowed(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "701", ShortID: strp("PROJ-70"), Title: "Fixed soon", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf) // posted new, now tracked
	h.sentry.edit("701", func(i *pwSentryIssue) { i.Status = "resolved" })
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"resolved:PROJ-70:low"}) {
		t.Fatalf("resolved note: %v", got)
	}
	before := len(h.sentry.callsTo("tracked"))
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("the resolved note posted twice: %v", got)
	}
	if after := h.sentry.callsTo("tracked"); len(after) != before+1 || !strings.Contains(fmt.Sprint(after[len(after)-1].Q["group"]), "701") {
		t.Fatalf("a resolved, noted issue is no longer read by id: %d call(s) since", len(after)-before)
	}
}

// TestProdWatch_SentryIdentityChangeRebootstraps: moving the config to another
// environment re-arms the lane (a silent bootstrap) and drops the incidents of
// the old identity instead of judging the new environment's issues against
// them.
func TestProdWatch_SentryIdentityChangeRebootstraps(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "801", ShortID: strp("PROJ-80"), Title: "prod bug", FirstProcessed: now.Add(-time.Hour), LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	h.sentry.envs["staging"] = true
	h.sentry.mu.Unlock()
	h.sentry.put(&pwSentryIssue{ID: "802", ShortID: strp("PROJ-81"), Title: "staging bug", Env: "staging", FirstProcessed: time.Now(), LastSeen: time.Now()})
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["environment"] = "staging" }))
	outs := sentryTick(t, h, wf)
	if got := sentryAlerts(outs); len(got) != 0 {
		t.Fatalf("an identity change posted instead of re-arming: %v", got)
	}
	if sentryIncident(t, h, "801") != nil {
		t.Fatal("the old identity's incident survived the identity change")
	}
	if rec := sentryIncident(t, h, "802"); rec == nil || rec["backlog"] != true {
		t.Fatalf("the new identity's issue is not silent backlog: %v", rec)
	}
	cur := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)
	if cur["identity"].(map[string]any)["environment"] != "staging" {
		t.Fatalf("the cursor kept the old identity: %v", cur)
	}
}

// TestProdWatch_SentryFailedBootstrapWritesNoCursor: a bootstrap whose list
// failed arms nothing — the next tick bootstraps again — and neither does a
// truncated one (the next overlap would re-read its unread part as new).
func TestProdWatch_SentryFailedBootstrapWritesNoCursor(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		// A second lane keeps the tick alive while the Sentry lane fails.
		cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
	})
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "901", ShortID: strp("PROJ-90"), Title: "x", FirstProcessed: now, LastSeen: now})
	h.sentry.failNext("list", 500, 500)
	outs := sentryTick(t, h, wf)
	if outs["poll_sentry"]["ok"] != false {
		t.Fatalf("the failed walk reads ok: %v", outs["poll_sentry"])
	}
	st := h.state(t)
	if cur := st["cursors"].(map[string]any)["sentry"]; cur != nil {
		t.Fatalf("a failed bootstrap armed the cursor: %v", cur)
	}
	if sentryIncident(t, h, "901") != nil {
		t.Fatal("a failed bootstrap recorded an issue")
	}
	// Truncated bootstrap: one page of one issue, two issues there.
	h.sentry.mu.Lock()
	h.sentry.pageSize = 1
	h.sentry.mu.Unlock()
	h.sentry.put(&pwSentryIssue{ID: "902", ShortID: strp("PROJ-91"), Title: "y", FirstProcessed: now, LastSeen: now})
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, func(s map[string]any) { s["max_issues"] = 1 })(cfg)
		cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
	})
	outs = sentryTick(t, h, wf)
	if outs["poll_sentry"]["truncated"] != true {
		t.Fatalf("the capped walk does not say truncated: %v", outs["poll_sentry"])
	}
	if cur := h.state(t)["cursors"].(map[string]any)["sentry"]; cur != nil {
		t.Fatalf("a truncated bootstrap armed: %v", cur)
	}
}

// TestProdWatch_SentryHandoffIsChecked: leak_scan refuses a missing or
// short Sentry handoff, decide a scan holding another number of issues — a
// resumed run on another runner must never read as an empty, complete walk.
func TestProdWatch_SentryHandoffIsChecked(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	raw := filepath.Join(h.scratch, "sentry_raw-x.jsonl")
	run := func(file string, n int) error {
		_, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
			"raw_file": filepath.Join(h.scratch, "none.jsonl"), "per_query": map[string]any{}, "sentry_file": file,
			"sentry_issues": n, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
		return err
	}
	if err := run(raw, 2); err == nil {
		t.Fatal("leak_scan scored a missing Sentry handoff the lane reported 2 issues for")
	}
	if err := os.WriteFile(raw, []byte(`{"id":"1","title":"a"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(raw, 2); err == nil {
		t.Fatal("leak_scan scored a Sentry handoff holding 1 record where the lane reported 2")
	}
	if err := run(raw, 1); err != nil {
		t.Fatalf("a consistent handoff was refused: %v", err)
	}
	in := map[string]any{}
	for k, v := range pwSentryOff {
		in[k] = v
	}
	in["sentry_issues"] = 2
	_, _, err := pwDecide(t, wf, h, map[string]any{"sentry_issues": []any{map[string]any{"id": "1"}}}, nil, in)
	if err == nil {
		t.Fatal("decide scored signals holding 1 Sentry issue where the lane reported 2")
	}
}

// TestProdWatch_SentryLeakMaskedAndCountedPerSighting: a personal value in an
// issue's title and metadata never leaves the scan raw — not in any node's
// output, the signals, the state, the alert log nor the channel — and the
// class posts as a Sentry leak with a masked sample; a re-read without a new
// event does not count it again.
func TestProdWatch_SentryLeakMaskedAndCountedPerSighting(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	email := "helene.zq7rtx@qz9mail.fr"
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1001", ShortID: strp("PROJ-100"), Title: "UserNotFound: " + email,
		Culprit: "app.users in find", Meta: map[string]any{"type": "UserNotFound", "value": "no user " + email},
		FirstProcessed: now, LastSeen: now, Count: 1})
	outs := sentryTick(t, h, wf)
	if got, want := sentryAlerts(outs), []string{"new:PROJ-100:medium", "new:leak-email:high"}; !eqStrings(got, want) {
		t.Fatalf("leaking issue: got %v, want %v", got, want)
	}
	var leakRec map[string]any
	for _, a := range outs["decide"]["alerts"].([]any) {
		if m := a.(map[string]any); m["kind"] == "sentry_leak" {
			leakRec = m
		}
	}
	if s := fmt.Sprint(leakRec["evidence"]); !strings.Contains(s, "***") {
		t.Fatalf("the Sentry leak alert carries no masked sample: %v", leakRec)
	}
	escaped, _ := json.Marshal(email)
	needles := []string{email, strings.Trim(string(escaped), `"`), email[:8]}
	haystacks := map[string]string{"sinks": strings.Join(h.bodies(), "\n")}
	for id, out := range outs {
		b, _ := json.Marshal(out)
		haystacks["stdout of "+id] = string(b)
		haystacks["stderr of "+id] = h.stderrs[id]
	}
	for _, name := range []string{"state.json", "alertlog.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(h.ws, ".prod-watch", name))
		haystacks[name] = string(b)
	}
	sig, _ := os.ReadFile(outs["leak_scan"]["signals_file"].(string))
	haystacks["signals"] = string(sig)
	for where, text := range haystacks {
		for _, n := range needles {
			if strings.Contains(text, n) {
				t.Fatalf("the planted email (%q) reached %s", n, where)
			}
		}
	}
	rec, _ := h.state(t)["incidents"].(map[string]any)["sentry_leak:email"].(map[string]any)
	count := rec["count"]
	sentryTick(t, h, wf) // re-read, no new event
	rec2, _ := h.state(t)["incidents"].(map[string]any)["sentry_leak:email"].(map[string]any)
	if fmt.Sprint(rec2["count"]) != fmt.Sprint(count) {
		t.Fatalf("a re-read without a new event counted the leak again: %v then %v", count, rec2["count"])
	}
}

// TestProdWatch_SentryTextNeverPersists: an issue's title and culprit reach the
// channel (scrubbed) but never the state or the alert log, which are committed
// to git for good; the incident keeps its short id.
func TestProdWatch_SentryTextNeverPersists(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1101", ShortID: strp("PROJ-110"), Title: "Quixotic Zanzibar failure", Culprit: "app.wombat in frobnicate",
		FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	if body := strings.Join(h.bodies(), "\n"); !strings.Contains(body, "Quixotic Zanzibar failure") {
		t.Fatalf("the title did not reach the channel:\n%s", body)
	}
	for _, name := range []string{"state.json", "alertlog.jsonl"} {
		b, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"Quixotic", "Zanzibar", "wombat", "frobnicate"} {
			if strings.Contains(string(b), n) {
				t.Fatalf("issue text %q persisted in %s", n, name)
			}
		}
		if !strings.Contains(string(b), "PROJ-110") {
			t.Fatalf("%s lost the short id", name)
		}
	}
}

// TestProdWatch_SentryWalkIsStrictAboutTheAPI: a short first page with
// results="true" is not the end, a Link URL on a foreign host is never
// followed (only its cursor), a 429 is retried, and every list call carries
// the promised parameters (the fake refuses otherwise).
func TestProdWatch_SentryWalkIsStrictAboutTheAPI(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_issues"] = 300 }))
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	h.sentry.pageSize = 2
	h.sentry.shortFirstPage = true
	h.sentry.mu.Unlock()
	now := time.Now()
	for i := 0; i < 5; i++ {
		id := strconv.Itoa(1200 + i)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PROJ-" + id), Title: "t" + id, FirstProcessed: now.Add(-time.Duration(i) * time.Second), LastSeen: now})
	}
	h.sentry.failNext("list", 429)
	outs := sentryTick(t, h, wf)
	if outs["poll_sentry"]["ok"] != true {
		t.Fatalf("the walk failed: %v", outs["poll_sentry"])
	}
	// Page 1 dropped its second issue (auto-resolved in the real server); the
	// other four are read across the pages.
	if got := len(sentryAlerts(outs)); got != 4 {
		t.Fatalf("a short page ended the walk: %d new issue(s) posted, want 4 (%v)", got, sentryAlerts(outs))
	}
	for _, c := range h.sentry.callsTo("list") {
		if c.Q.Get("query") == "" || c.Q.Get("environment") != "preprod" || c.Q.Get("statsPeriod") != "14d" {
			t.Fatalf("a list call lacks a promised parameter: %v", c.Q)
		}
	}
	h.sentry.mu.Lock()
	h.sentry.badCursor = true
	h.sentry.mu.Unlock()
	h.sentry.put(&pwSentryIssue{ID: "1299", ShortID: strp("PROJ-1299"), Title: "z", FirstProcessed: time.Now(), LastSeen: time.Now()})
	h.sentry.put(&pwSentryIssue{ID: "1298", ShortID: strp("PROJ-1298"), Title: "z", FirstProcessed: time.Now(), LastSeen: time.Now()})
	h.sentry.put(&pwSentryIssue{ID: "1297", ShortID: strp("PROJ-1297"), Title: "z", FirstProcessed: time.Now(), LastSeen: time.Now()})
	outs = sentryTick(t, h, wf)
	if errs := fmt.Sprint(outs["poll_sentry"]["errors"]); !strings.Contains(errs, "cursor in an unknown format") || strings.Contains(errs, "../") {
		t.Fatalf("a malformed cursor is not a named lane error that withholds it: %v", errs)
	}
}

// TestProdWatch_SentryErrorsAreNamed: a refused token, an environment unknown
// to the project and a refused query are lane errors named in the coverage
// note, never the tick's death while another lane answers; alone, they
// refuse the tick.
func TestProdWatch_SentryErrorsAreNamed(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	withProbe := func(h *pwHarness, mod func(s map[string]any)) func(cfg map[string]any) {
		return func(cfg map[string]any) {
			sentryOnly(h, mod)(cfg)
			cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
		}
	}
	cases := []struct {
		name, want string
		mod        func(h *pwHarness, s map[string]any)
		fail       string
		status     int
	}{
		{name: "token refused", want: "CredentialRefused: Sentry refused the token (HTTP 401)", fail: "project", status: 401},
		{name: "unknown environment", want: "environment 'prepod' is unknown to project org/proj",
			mod: func(h *pwHarness, s map[string]any) { s["environment"] = "prepod" }},
		{name: "query refused", want: "Sentry refused the new-issue list (HTTP 400)", fail: "list", status: 400},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			var mod func(s map[string]any)
			if c.mod != nil {
				mod = func(s map[string]any) { c.mod(h, s) }
			}
			h.writeConfig(t, withProbe(h, mod))
			if c.fail != "" {
				h.sentry.failNext(c.fail, c.status)
			}
			outs := sentryTick(t, h, wf)
			if outs["poll_sentry"]["ok"] != false || !strings.Contains(fmt.Sprint(outs["poll_sentry"]["errors"]), c.want) {
				t.Fatalf("want a lane error naming %q, got %v", c.want, outs["poll_sentry"]["errors"])
			}
			if body := strings.Join(h.bodies(), "\n"); !strings.Contains(body, "coverage this tick was PARTIAL") {
				t.Fatalf("the failure did not reach the coverage note:\n%s", body)
			}
		})
	}
	t.Run("alone it refuses the tick", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		h.sentry.failNext("project", 401)
		vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
		secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
		plan, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		s, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_sentry").Script, map[string]any{"sentry": plan["sentry"],
			"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		leak, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{"raw_file": filepath.Join(h.scratch, "none"),
			"per_query": map[string]any{}, "sentry_file": s["raw_file"], "sentry_issues": s["issues"], "app": plan["app"], "scratch_dir": h.scratch}, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, err := pwDecide(t, wf, h, map[string]any{}, nil, map[string]any{"signals_file": leak["signals_file"],
			"lanes": plan["lanes"], "sentry": plan["sentry"], "sentry_ok": s["ok"], "sentry_truncated": s["truncated"],
			"sentry_errors": s["errors"], "sentry_walk": s["walk"], "sentry_issues": s["issues"], "loki_per_query": map[string]any{}})
		if err == nil || !strings.Contains(stderr, "every configured lane failed") || !strings.Contains(stderr, "sentry") {
			t.Fatalf("a dead Sentry-only tick was not refused by name: err=%v stderr=%s", err, stderr)
		}
	})
}

// TestProdWatch_SentryPlanGuards: every malformed field of the section is
// refused by name before any network work; a foreign cursor too.
func TestProdWatch_SentryPlanGuards(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	cases := []struct {
		name, want string
		mod        func(s map[string]any)
	}{
		{"base url with a query", "config.sentry.base_url", func(s map[string]any) { s["base_url"] = "https://sentry.example/?x=1" }},
		{"base url with credentials", "config.sentry.base_url", func(s map[string]any) { s["base_url"] = "https://u:p@sentry.example" }},
		{"org not a slug", "config.sentry.org", func(s map[string]any) { s["org"] = "Org With Spaces" }},
		{"project missing", "config.sentry.project", func(s map[string]any) { delete(s, "project") }},
		{"environment with a slash", "config.sentry.environment", func(s map[string]any) { s["environment"] = "a/b" }},
		{"unknown level", "config.sentry.min_level", func(s map[string]any) { s["min_level"] = "loud" }},
		{"bool as integer", "config.sentry.max_issues", func(s map[string]any) { s["max_issues"] = true }},
		{"severity map", "config.sentry.severity", func(s map[string]any) { s["severity"] = map[string]any{"fatal": "extreme"} }},
		{"max severity", "config.sentry.max_severity", func(s map[string]any) { s["max_severity"] = "urgent" }},
		{"overlap as wide as the catch-up", "config.sentry.overlap_minutes", func(s map[string]any) {
			s["overlap_minutes"], s["max_catchup_hours"] = 60, 1
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, c.mod))
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
			if err == nil || !strings.Contains(stderr, c.want) {
				t.Fatalf("want a refusal naming %q, got err=%v stderr=%s", c.want, err, stderr)
			}
		})
	}
	t.Run("foreign cursor", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		h.setState(t, map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{"sentry": map[string]any{
			"identity": map[string]any{"base_url": h.srv.URL}, "armed_at": "x", "since": "y", "at": "z"}}, "incidents": map[string]any{}, "health": map[string]any{}})
		vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
		_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
		if err == nil || !strings.Contains(stderr, "cursors.sentry.identity") {
			t.Fatalf("a foreign Sentry cursor was not refused by name: err=%v stderr=%s", err, stderr)
		}
	})
}

// TestProdWatch_SentryMinLevelFiltersNewTrackingOnly: an issue below the level
// floor is never tracked; an issue already alerted stays observed when its
// latest event drops below the floor (no false "not observed any more").
func TestProdWatch_SentryMinLevelFiltersNewTrackingOnly(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1301", ShortID: strp("PROJ-130"), Title: "just a warning", Level: "warning", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "1302", ShortID: strp("PROJ-131"), Title: "an error", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"new:PROJ-131:medium"}) {
		t.Fatalf("level floor: %v", got)
	}
	if sentryIncident(t, h, "1301") != nil {
		t.Fatal("an issue below min_level is tracked")
	}
	// The alerted error's latest event is now a warning, and it keeps firing.
	h.sentry.edit("1302", func(i *pwSentryIssue) { i.Level = "warning"; i.LastSeen = time.Now().Add(time.Second) })
	st := h.state(t)
	rec := st["incidents"].(map[string]any)["sentry:1302"].(map[string]any)
	rec["last_seen"] = time.Now().Add(-60 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a tracked issue whose level dropped got a note instead of a sighting: %v", got)
	}
}

// TestProdWatch_SentryIdleAlertedIssueGetsOneQuietNote: an alerted issue with
// no new event for quiet_after_hours gets one "not observed any more" note —
// only while the by-id walk was complete.
func TestProdWatch_SentryIdleAlertedIssueGetsOneQuietNote(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1401", ShortID: strp("PROJ-140"), Title: "idle soon", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	st := h.state(t)
	st["incidents"].(map[string]any)["sentry:1401"].(map[string]any)["last_seen"] = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	h.sentry.failNext("tracked", 500, 500)
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a failed by-id walk concluded a quiet note: %v", got)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"quiet:PROJ-140:low"}) {
		t.Fatalf("idle alerted issue: %v", got)
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("the quiet note posted twice: %v", got)
	}
}

// TestProdWatch_SentryDeadlineIsPartialNotDeath: a walk that outlives its
// deadline stops, says so in the coverage note, and the tick goes on.
func TestProdWatch_SentryDeadlineIsPartialNotDeath(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, func(s map[string]any) { s["deadline_secs"] = 10 })(cfg)
		cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
	})
	h.sentry.mu.Lock()
	h.sentry.delay = 6 * time.Second
	h.sentry.mu.Unlock()
	outs := sentryTick(t, h, wf)
	walk, _ := outs["poll_sentry"]["walk"].(map[string]any)
	if walk["deadline_hit"] != true || outs["poll_sentry"]["truncated"] != true {
		t.Fatalf("the deadline did not stop the walk: %v", outs["poll_sentry"])
	}
	if body := strings.Join(h.bodies(), "\n"); !strings.Contains(body, "the deadline passed") {
		t.Fatalf("the coverage note does not name the deadline:\n%s", body)
	}
}

// TestProdWatch_SentryNotifyRendersOnlyItsOwnLink: notify renders a Sentry
// link only when it is exactly <link_prefix><digits>/ — a link to another
// host, a path with more segments, or text around it stays out.
func TestProdWatch_SentryNotifyRendersOnlyItsOwnLink(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	prefix := "https://sentry.example/organizations/org/issues/"
	render := func(link, short string) string {
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
			"alerts": []any{map[string]any{"fingerprint": "sentry:9", "kind": "sentry", "severity": "medium", "state": "new",
				"title_key": "sentry_issue", "title_arg": "t", "detail_key": "sentry_detail_short", "fields": map[string]any{"level": "error", "short_id": short},
				"evidence": map[string]any{"link": link, "short_id": short}, "count": 1, "first_seen": "2026-09-29T00:00:00+00:00"}},
			"overflow_count": 0, "stale_sources": []any{}, "sinks": []any{map[string]any{"webhook": "w1", "channel": "", "min_severity": "low"}},
			"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "sentry": map[string]any{"enabled": true, "link_prefix": prefix},
			"release": "", "release_known": false, "dry_run": true, "max_message_chars": 14000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
		if err != nil {
			t.Fatalf("notify: %v %s", err, stderr)
		}
		return fmt.Sprint(out["messages"])
	}
	if got := render(prefix+"42/", "PROJ-1"); !strings.Contains(got, "[PROJ-1]("+prefix+"42/)") {
		t.Fatalf("the configured link was not rendered: %s", got)
	}
	for _, bad := range []string{"https://evil.example/organizations/org/issues/42/", prefix + "42/x/", prefix + "42/) [x](https://evil.example", prefix + "4a2/"} {
		if got := render(bad, "PROJ-1"); strings.Contains(got, "](") {
			t.Fatalf("a foreign link was rendered (%q): %s", bad, got)
		}
	}
	if got := render(prefix+"42/", "PROJ-1](https://evil.example"); !strings.Contains(got, "[42]("+prefix+"42/)") {
		t.Fatalf("a hostile short id was not replaced by the digits: %s", got)
	}
}
