package connectors_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// Jira Cloud's enhanced search, as Atlassian documents it:
//
//   - GET /rest/api/3/search/jql with `jql`, `fields`, `maxResults` and
//     `nextPageToken` (https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-search/#api-rest-api-3-search-jql-get,
//     operationId searchAndReconsileIssuesUsingJql in swagger-v3.v3.json).
//   - The answer is `{issues, nextPageToken, isLast}`; the token is absent on
//     the last page, and Jira "may return fewer items per page" than asked.
//   - Without `fields`, an issue carries its `id` only.
//   - An unbounded JQL is refused.
//   - A date in JQL: a QUOTED value is read in the searching user's profile
//     time zone, in `yyyy/MM/dd HH:mm`, `yyyy-MM-dd HH:mm`, `yyyy/MM/dd` or
//     `yyyy-MM-dd`; an UNQUOTED number is milliseconds after the epoch
//     (https://support.atlassian.com/jira-software-cloud/docs/jql-fields/#Updated).
//   - AND binds tighter than OR, and parentheses group.
//
// The double reads the JQL subset a poller of this package sends —
// `project = KEY`, `labels = NAME`, `updated >= VALUE`, AND, OR, parentheses
// and one ORDER BY on `updated` — evaluates it over its own issues, and
// answers anything else with Jira's 400, so a package that writes another
// query is answered as Jira would answer it rather than echoed.

const (
	jiraSite       = "https://acme.atlassian.net"
	jiraSearchPath = "/rest/api/3/search/jql"
	// jiraPageCap is the page size the double answers whatever maxResults
	// asks: Jira may return fewer, and a scan of three or more issues then
	// takes several pages, which is how the walk's nextPageToken is
	// witnessed.
	jiraPageCap = 2
)

var jiraWitnessCred = exec.Credential{SchemeID: "basic", Username: "alice@acme.example", Password: "ATATT-witness-token"}

func init() {
	// The JQL's ORDER BY updated ASC: Jira answers oldest first.
	registerPoll("jira.issue.changed", pollDouble{order: spec.PollOrderOldestFirst, cases: []pollCase{
		{
			name:   "project-filter",
			query:  map[string]any{"jql": "project = OPS"},
			cred:   jiraWitnessCred,
			vendor: func(t *testing.T) pollVendor { return newJiraVendor() },
		},
		{
			// An OR in the operator's filter: unless the filter is grouped,
			// AND binds the checkpoint to its last term only, and the first
			// term's issues come back on every tick whatever their age.
			name:   "or-filter",
			query:  map[string]any{"jql": "project = OPS OR labels = sources"},
			cred:   jiraWitnessCred,
			vendor: func(t *testing.T) pollVendor { return newJiraVendor() },
		},
	}})
}

type jiraIssue struct {
	id, key, project, projectName, summary, status, issueType, priority, assignee string
	labels                                                                        []string
	created, updated                                                              time.Time
}

type jiraVendor struct {
	// tz is the searching user's profile time zone, which a quoted JQL date
	// is read in (Europe/Paris in November).
	tz     *time.Location
	issues []*jiraIssue
	tokens map[string]jiraToken
	issued []string
	next   int
	// served are the issues answered since the last checkTick.
	served []*jiraIssue
}

// clock is the instant the double searches at: just after its latest
// change, as a search right after the vendor wrote it would be.
func (v *jiraVendor) clock() time.Time {
	var latest time.Time
	for _, is := range v.issues {
		if is.updated.After(latest) {
			latest = is.updated
		}
	}
	return latest.Add(time.Minute)
}

type jiraToken struct {
	jql    string
	offset int
}

func jiraInstant(s string) time.Time {
	at, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		panic(err)
	}
	return at
}

func newJiraVendor() *jiraVendor {
	return &jiraVendor{tz: time.FixedZone("CET", 3600), tokens: map[string]jiraToken{}, issues: []*jiraIssue{
		{id: "10062", key: "OPS-128", project: "OPS", projectName: "Operations", summary: "Webhook drops a delivery whose signature header repeats",
			status: "In Progress", issueType: "Bug", priority: "High", assignee: "7120201f9c4e4a2b8f3d6c5e7a0b1d2c", labels: []string{"sources", "webhooks"},
			created: jiraInstant("2026-11-17T09:12:41.000+0000"), updated: jiraInstant("2026-11-18T08:41:07.512+0000")},
		{id: "10071", key: "OPS-131", project: "OPS", projectName: "Operations", summary: "Poller re-reads the overlap window after a restart",
			status: "To Do", issueType: "Task", priority: "Medium", labels: []string{},
			created: jiraInstant("2026-11-18T08:52:19.000+0000"), updated: jiraInstant("2026-11-18T08:52:19.000+0000")},
		{id: "10075", key: "OPS-133", project: "OPS", projectName: "Operations", summary: "Checkpoint survives a takeover",
			status: "To Do", issueType: "Task", priority: "Low", labels: []string{"pollers"},
			created: jiraInstant("2026-11-18T09:01:00.000+0000"), updated: jiraInstant("2026-11-18T09:30:00.000+0000")},
		// Another project's issue, updated last: `project = OPS` must not
		// deliver it, `labels = sources` does.
		{id: "10080", key: "ENG-7", project: "ENG", projectName: "Engineering", summary: "Sources catalogue lists pollers",
			status: "Done", issueType: "Bug", priority: "High", labels: []string{"sources"},
			created: jiraInstant("2026-11-18T09:02:00.000+0000"), updated: jiraInstant("2026-11-18T09:31:00.000+0000")},
	}}
}

// revise transitions OPS-128 a quarter of an hour after the last scan.
// OPS-133 is left alone and stays within the overlap of the checkpoint.
func (v *jiraVendor) revise() (changed, untouched string) {
	v.issues[0].status = "In Review"
	v.issues[0].updated = jiraInstant("2026-11-18T09:45:12.250+0000")
	return "10062", "10075"
}

func (v *jiraVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != jiraSearchPath {
		jiraError(w, http.StatusNotFound, "no such resource: "+r.Method+" "+r.URL.Path)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != jiraWitnessCred.Username || pass != jiraWitnessCred.Password {
		jiraError(w, http.StatusUnauthorized, "Client must be authenticated to access this resource.")
		return
	}
	q := r.URL.Query()
	jql := q.Get("jql")
	offset := 0
	if tok := q.Get("nextPageToken"); tok != "" {
		state, known := v.tokens[tok]
		if !known || state.jql != jql {
			jiraError(w, http.StatusBadRequest, "The provided next page token is invalid or expired.")
			return
		}
		offset = state.offset
	}
	parsed, err := parseJiraJQL(jql, v.tz, v.clock())
	if err != nil {
		jiraError(w, http.StatusBadRequest, err.Error())
		return
	}
	var hits []*jiraIssue
	for _, is := range v.issues {
		if parsed.where.matches(is) {
			hits = append(hits, is)
		}
	}
	switch parsed.orderBy {
	case "updated ASC":
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].updated.Before(hits[j].updated) })
	case "updated DESC":
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].updated.After(hits[j].updated) })
	case "":
		// No order asked: the double answers newest-created first, an order
		// a checkpoint cannot rely on.
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].created.After(hits[j].created) })
	default:
		jiraError(w, http.StatusBadRequest, fmt.Sprintf("the double orders by updated only, not %q", parsed.orderBy))
		return
	}
	size := jiraPageCap
	if m, err := strconv.Atoi(q.Get("maxResults")); err == nil && m > 0 && m < size {
		size = m
	}
	end := min(offset+size, len(hits))
	fields := jiraFieldSet(q.Get("fields"))
	issues := []any{}
	for _, is := range hits[offset:end] {
		issues = append(issues, v.render(is, fields))
		v.served = append(v.served, is)
	}
	page := map[string]any{"issues": issues}
	if end < len(hits) {
		v.next++
		tok := fmt.Sprintf("Cg%04d%s", v.next, base64.RawURLEncoding.EncodeToString([]byte(jql))[:8])
		v.tokens[tok] = jiraToken{jql: jql, offset: end}
		v.issued = append(v.issued, tok)
		page["nextPageToken"] = tok
		page["isLast"] = false
	} else {
		page["isLast"] = true
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	_ = json.NewEncoder(w).Encode(page)
}

// jiraFieldSet reads `fields`: nil for "every field", else the names asked.
func jiraFieldSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "*all" || f == "*navigable" {
			return nil
		}
		if f != "" {
			out[f] = true
		}
	}
	return out
}

func (v *jiraVendor) render(is *jiraIssue, fields map[string]bool) map[string]any {
	if fields != nil && (len(fields) == 0 || (len(fields) == 1 && fields["id"])) {
		return map[string]any{"id": is.id}
	}
	user := func(accountID string) any {
		if accountID == "" {
			return nil
		}
		return map[string]any{"self": jiraSite + "/rest/api/3/user?accountId=" + accountID, "accountId": accountID, "active": true, "accountType": "atlassian"}
	}
	all := map[string]any{
		"summary":   is.summary,
		"status":    map[string]any{"name": is.status, "self": jiraSite + "/rest/api/3/status/1"},
		"issuetype": map[string]any{"name": is.issueType, "subtask": false},
		"project":   map[string]any{"key": is.project, "name": is.projectName, "self": jiraSite + "/rest/api/3/project/" + is.project},
		"labels":    is.labels,
		"priority":  map[string]any{"name": is.priority},
		"assignee":  user(is.assignee),
		"reporter":  user("5b10ac8d82e05b22cc7d4ef5"),
		"created":   is.created.Format("2006-01-02T15:04:05.000-0700"),
		"updated":   is.updated.Format("2006-01-02T15:04:05.000-0700"),
	}
	got := map[string]any{}
	for k, val := range all {
		if fields == nil || fields[k] {
			got[k] = val
		}
	}
	return map[string]any{
		"expand": "renderedFields,names,schema,operations,editmeta,changelog,versionedRepresentations",
		"id":     is.id, "key": is.key, "self": jiraSite + "/rest/api/3/issue/" + is.id,
		"fields": got,
	}
}

func jiraError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errorMessages": []string{msg}, "warningMessages": []string{}})
}

func (v *jiraVendor) checkTick(t *testing.T, tick int, cp *spec.PollCheckpoint, reqs []recordedRequest) {
	t.Helper()
	served := v.served
	v.served = nil
	if len(reqs) == 0 {
		t.Fatalf("tick %d sent no request", tick)
	}
	first, err := url.ParseQuery(reqs[0].RawQuery)
	if err != nil {
		t.Fatalf("tick %d: the query string does not parse: %v", tick, err)
	}
	jql := first.Get("jql")
	parsed, err := parseJiraJQL(jql, v.tz, v.clock())
	if err != nil {
		t.Fatalf("tick %d: Jira refuses the JQL %q: %v", tick, jql, err)
	}
	if parsed.orderBy != "updated ASC" {
		t.Errorf("tick %d: the JQL %q orders by %q, not updated ASC — a scan's last page would not hold its newest revisions", tick, jql, parsed.orderBy)
	}
	since := parsed.where.since()
	switch {
	case cp == nil && !slices.Equal(since, []int64{v.clock().Add(-24 * time.Hour).UnixMilli()}):
		// The first tick has no checkpoint and reads the last day alone: a
		// project's whole history would be walked one page bound per tick
		// before the poller primes.
		t.Errorf("tick %d: the first tick's JQL %q reads updated >= %v, want the last day alone", tick, jql, since)
	case cp != nil && !slices.Contains(since, cp.At.UnixMilli()):
		t.Errorf("tick %d: the JQL %q reads updated >= %v, and the checkpoint is %d ms (%s)", tick, jql, since, cp.At.UnixMilli(), cp.At.UTC().Format(time.RFC3339Nano))
	}
	// The checkpoint must bound the WHOLE filter: every issue a later tick
	// is answered was updated at or after it.
	if cp != nil {
		for _, is := range served {
			if is.updated.Before(cp.At) {
				t.Errorf("tick %d: Jira answered %s, updated %s, before the checkpoint %s — the JQL %q lets part of the filter escape it",
					tick, is.key, is.updated.UTC().Format(time.RFC3339), cp.At.UTC().Format(time.RFC3339), jql)
			}
		}
	}
	if tick == 0 && len(reqs) < 2 {
		t.Errorf("tick 0 sent %d request(s) for %d issues at %d a page — the walk never followed nextPageToken", len(reqs), len(served), jiraPageCap)
	}
	for i, r := range reqs {
		q, _ := url.ParseQuery(r.RawQuery)
		tok := q.Get("nextPageToken")
		switch {
		case i == 0 && tok != "":
			t.Errorf("tick %d: the first page carries nextPageToken %q", tick, tok)
		case i > 0 && !slices.Contains(v.issued, tok):
			t.Errorf("tick %d page %d: nextPageToken %q is not one Jira issued (%v)", tick, i, tok, v.issued)
		}
	}
}

func (v *jiraVendor) wantOutputs(tick int) map[string]map[string]any {
	a := v.issues[0]
	want := map[string]map[string]any{
		a.id: {
			"subject": a.summary, "subject_state": a.status, "repo": "OPS", "thread": a.key,
			"labels": a.labels, "issue_key": a.key, "issue_id": a.id, "issue_type": a.issueType,
			"project_key": "OPS", "project_name": "Operations", "issue_url": jiraSite + "/rest/api/3/issue/" + a.id,
			"priority": a.priority, "assignee_account_id": a.assignee,
			"created_at": a.created.Format("2006-01-02T15:04:05.000-0700"),
			"updated_at": a.updated.Format("2006-01-02T15:04:05.000-0700"),
		},
	}
	if tick == 0 {
		b := v.issues[1]
		want[b.id] = map[string]any{"issue_key": b.key, "subject_state": b.status, "labels": b.labels}
	}
	return want
}

// jiraRefusal is Jira's own message for a query it refuses, answered as a
// 400.
type jiraRefusal string

func (r jiraRefusal) Error() string { return string(r) }

// jiraJQL is what the double read from a JQL.
type jiraJQL struct {
	where   jiraNode
	orderBy string
}

// jiraNode is one node of a JQL WHERE clause: a conjunction or disjunction
// of kids, or one clause.
type jiraNode struct {
	op    string // "AND", "OR", or "" for a clause
	kids  []jiraNode
	field string
	value string
	ms    int64
}

func (n jiraNode) matches(is *jiraIssue) bool {
	switch n.op {
	case "AND":
		for _, k := range n.kids {
			if !k.matches(is) {
				return false
			}
		}
		return true
	case "OR":
		for _, k := range n.kids {
			if k.matches(is) {
				return true
			}
		}
		return false
	}
	switch n.field {
	case "project":
		return is.project == n.value
	case "labels":
		return slices.Contains(is.labels, n.value)
	case "updated":
		return is.updated.UnixMilli() >= n.ms
	}
	return false
}

// since lists every `updated >=` bound in the clause, wherever it sits.
func (n jiraNode) since() []int64 {
	if n.op == "" {
		if n.field == "updated" {
			return []int64{n.ms}
		}
		return nil
	}
	var out []int64
	for _, k := range n.kids {
		out = append(out, k.since()...)
	}
	return out
}

// bounded reports whether the clause restricts the search to something, as
// Jira requires.
func (n jiraNode) bounded() bool { return n.op != "" || n.field != "" }

// parseJiraJQL reads the subset the double answers, the way Jira reads it.
func parseJiraJQL(jql string, tz *time.Location, now time.Time) (jiraJQL, error) {
	var out jiraJQL
	s := strings.TrimSpace(jql)
	upper := strings.ToUpper(s)
	switch i := strings.LastIndex(upper, "ORDER BY "); {
	case i == 0:
		out.orderBy, s = strings.Join(strings.Fields(s[len("ORDER BY "):]), " "), ""
	case i > 0 && s[i-1] == ' ':
		out.orderBy, s = strings.Join(strings.Fields(s[i+len("ORDER BY "):]), " "), strings.TrimSpace(s[:i])
	}
	toks, err := jiraTokens(s)
	if err != nil {
		return out, err
	}
	p := &jiraParser{toks: toks, tz: tz, now: now}
	if len(toks) > 0 {
		if out.where, err = p.or(); err != nil {
			return out, err
		}
		if p.i != len(toks) {
			return out, jiraRefusal(fmt.Sprintf("Error in the JQL Query: Expecting either 'OR' or 'AND' but got '%s'.", toks[p.i]))
		}
	}
	if !out.where.bounded() {
		return out, jiraRefusal("Unbounded JQL queries are not allowed here. Please add a search restriction to your query.")
	}
	return out, nil
}

// jiraTokens splits a WHERE clause into words, quoted strings, operators and
// parentheses.
func jiraTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case ' ':
			i++
		case '(', ')':
			out = append(out, string(c))
			i++
		case '"', '\'':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return nil, jiraRefusal("Error in the JQL Query: an unterminated quote.")
			}
			out = append(out, s[i:i+j+2])
			i += j + 2
		case '>', '<', '=', '!':
			j := i + 1
			if j < len(s) && s[j] == '=' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" ()\"'<>=!", rune(s[j])) {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, nil
}

type jiraParser struct {
	toks []string
	i    int
	tz   *time.Location
	// now is the search's own instant, which a relative period reads from.
	now time.Time
}

func (p *jiraParser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *jiraParser) or() (jiraNode, error) {
	left, err := p.and()
	if err != nil {
		return left, err
	}
	kids := []jiraNode{left}
	for strings.EqualFold(p.peek(), "OR") {
		p.i++
		right, err := p.and()
		if err != nil {
			return right, err
		}
		kids = append(kids, right)
	}
	if len(kids) == 1 {
		return left, nil
	}
	return jiraNode{op: "OR", kids: kids}, nil
}

func (p *jiraParser) and() (jiraNode, error) {
	left, err := p.unary()
	if err != nil {
		return left, err
	}
	kids := []jiraNode{left}
	for strings.EqualFold(p.peek(), "AND") {
		p.i++
		right, err := p.unary()
		if err != nil {
			return right, err
		}
		kids = append(kids, right)
	}
	if len(kids) == 1 {
		return left, nil
	}
	return jiraNode{op: "AND", kids: kids}, nil
}

func (p *jiraParser) unary() (jiraNode, error) {
	if p.peek() == "(" {
		p.i++
		n, err := p.or()
		if err != nil {
			return n, err
		}
		if p.peek() != ")" {
			return n, jiraRefusal("Error in the JQL Query: Expecting ')' before the end of the query.")
		}
		p.i++
		return n, nil
	}
	if p.i+3 > len(p.toks) {
		return jiraNode{}, jiraRefusal("Error in the JQL Query: Expecting a field name, an operator and a value.")
	}
	field, op, value := strings.ToLower(p.toks[p.i]), p.toks[p.i+1], p.toks[p.i+2]
	p.i += 3
	switch {
	case (field == "project" || field == "labels") && op == "=":
		return jiraNode{field: field, value: strings.Trim(value, `"'`)}, nil
	case field == "updated" && op == ">=":
		ms, err := jiraDate(value, p.tz, p.now)
		if err != nil {
			return jiraNode{}, err
		}
		return jiraNode{field: field, ms: ms}, nil
	}
	return jiraNode{}, jiraRefusal(fmt.Sprintf("Error in the JQL Query: the double reads project =, labels = and updated >= only, not %s %s %s", field, op, value))
}

// jiraPeriod is JQL's relative period: a signed count of weeks, days, hours
// or minutes before the search.
var jiraPeriod = regexp.MustCompile(`^-(\d+)([wdhm])$`)

// jiraDate reads a JQL date value: a quoted date in the profile time zone, an
// unquoted number of epoch milliseconds, or a relative period ("-1d") from
// the search's instant.
func jiraDate(value string, tz *time.Location, now time.Time) (int64, error) {
	if m := jiraPeriod.FindStringSubmatch(strings.Trim(value, `"'`)); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"w": 7 * 24 * time.Hour, "d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
		return now.Add(-time.Duration(n) * unit).UnixMilli(), nil
	}
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		inner := value[1 : len(value)-1]
		for _, layout := range []string{"2006/01/02 15:04", "2006-01-02 15:04", "2006/01/02", "2006-01-02"} {
			if at, err := time.ParseInLocation(layout, inner, tz); err == nil {
				return at.UnixMilli(), nil
			}
		}
		return 0, jiraRefusal(fmt.Sprintf("Date value '%s' for field 'updated' is invalid. Valid formats include: 'yyyy/MM/dd HH:mm', 'yyyy-MM-dd HH:mm', 'yyyy/MM/dd', 'yyyy-MM-dd', or a period format e.g. '-5d', '4w 2d'.", inner))
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, jiraRefusal(fmt.Sprintf("Date value '%s' for field 'updated' is invalid.", value))
	}
	return n, nil
}
