package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// pollPackage is the reference package plus two read operations a poller can
// call — a paginated search and an unpaginated feed — and one polled trigger
// per operation, each proven against a recorded response. Each refusal case
// below changes exactly one thing of it.
func pollPackage() *spec.Package {
	p := validPackage()
	search := spec.Operation{
		ID: "probe.issue.search", Resource: "issue", Verb: "search",
		HTTP: spec.HTTPBinding{Method: "GET", Path: "/issues/search"},
		Params: []spec.Param{
			{Key: "jql", Name: "jql", In: spec.InQuery, Type: "string", Required: true},
			{Key: "page", Name: "page", In: spec.InQuery, Type: "integer", Required: true},
			{Key: "limit", Name: "limit", In: spec.InQuery, Type: "integer"},
		},
		Pagination: &spec.Pagination{Style: spec.PageNumber, PageParam: "page", SizeParam: "limit", ItemsField: "issues"},
		Results:    []spec.ResultCase{{Status: 200}},
		Effect:     spec.EffectRead, Deterministic: true,
	}
	feed := spec.Operation{
		ID: "probe.feed.read", Resource: "feed", Verb: "read",
		HTTP:    spec.HTTPBinding{Method: "GET", Path: "/feed"},
		Results: []spec.ResultCase{{Status: 200}},
		Effect:  spec.EffectRead, Deterministic: true,
	}
	p.Ops[0].Operations = append(p.Ops[0].Operations, search, feed)
	p.Triggers = []spec.Trigger{
		{
			ID: "probe.issue.updated", Delivery: spec.DeliveryPoll,
			Poll: &spec.TriggerPoll{
				Operation: "probe.issue.search",
				Params:    map[string]string{"jql": `query.jql + " AND updated >= " + if(cursor.value, cursor.value, "-1d")`},
				Query:     []spec.PollQueryField{{ID: "jql", Required: true}},
				ItemID:    "item.key", ItemVersion: "item.fields.updated",
				Cursor:   &spec.PollCursor{From: "item.fields.updated", Kind: spec.CursorTimestamp, Overlap: "1m"},
				Interval: spec.PollInterval{Default: "5m", Min: "1m"},
			},
			Outputs: []spec.TriggerOutput{
				{Name: "subject", From: "payload.fields.summary"},
				{Name: "issue_key", From: "payload.key"},
			},
			Sample:     "samples/issue-updated.json",
			SampleBody: []byte(`{"response":{"issues":[{"key":"ENG-1","fields":{"summary":"Intake stalls","updated":"2026-11-17T09:10:00.000+0000"}}]}}`),
		},
		{
			ID: "probe.feed.published", Delivery: spec.DeliveryPoll,
			Poll: &spec.TriggerPoll{
				Operation: "probe.feed.read",
				Items:     "response.items", ItemID: "item.id", ItemVersion: "item.version",
				Interval: spec.PollInterval{Default: "15m", Min: "5m"},
			},
			Outputs:    []spec.TriggerOutput{{Name: "subject", From: "payload.title"}},
			Sample:     "samples/feed.json",
			SampleBody: []byte(`{"response":{"items":[{"id":"urn:1","version":"sha256:ab","title":"Release 7.12"}]}}`),
		},
	}
	return p
}

// TestAPolledTriggerValidates is the positive witness: both shapes — a
// paginated operation whose pagination locates the items, an unpaginated one
// whose poll.items does — load, and their samples prove identity, cursor and
// outputs. Without it the refusals below could pass by refusing everything.
func TestAPolledTriggerValidates(t *testing.T) {
	if err := pollPackage().Validate(); err != nil {
		t.Fatalf("the reference polled package must validate: %v", err)
	}
}

// TestAPollDeclarationNoPollerCouldRunIsRefused: one case per guard, each a
// single change to the reference package, each naming what it refuses.
func TestAPollDeclarationNoPollerCouldRunIsRefused(t *testing.T) {
	search := func(p *spec.Package) *spec.Trigger { return &p.Triggers[0] }
	feed := func(p *spec.Package) *spec.Trigger { return &p.Triggers[1] }
	cases := []struct {
		name   string
		mutate func(*spec.Package)
		want   string
	}{
		{"an unknown delivery", func(p *spec.Package) { search(p).Delivery = "stream" }, `delivery "stream"`},
		{"a poll block on a webhook trigger", func(p *spec.Package) {
			search(p).Delivery = ""
			search(p).Event = "issue_updated"
		}, "delivery is webhook"},
		{"a polled trigger with no poll block", func(p *spec.Package) { search(p).Poll = nil }, "declares no poll: block"},
		{"a polled trigger with an event", func(p *spec.Package) { search(p).Event = "issue_updated" }, "declares no event"},
		{"an unknown operation", func(p *spec.Package) { search(p).Poll.Operation = "probe.issue.find" }, "is not an operation"},
		{"a mutating operation", func(p *spec.Package) {
			p.Ops[0].Operations[1].Effect = spec.EffectUpdate
		}, "calls read operations only"},
		{"a parameter the operation does not declare", func(p *spec.Package) {
			search(p).Poll.Params["since"] = "cursor.value"
		}, `names "since"`},
		{"a required parameter left unbuilt", func(p *spec.Package) { delete(search(p).Poll.Params, "jql") }, `requires "jql"`},
		{"params reading another namespace", func(p *spec.Package) {
			search(p).Poll.Params["jql"] = "payload.jql"
		}, `reads namespace "payload" — this site may read`},
		{"params reading an undeclared query field", func(p *spec.Package) {
			search(p).Poll.Params["jql"] = "query.filter"
		}, "query.filter"},
		{"params reading the cursor beyond its value", func(p *spec.Package) {
			search(p).Poll.Params["jql"] = "cursor.updated"
		}, "holds value only"},
		{"a query field that is not an identifier", func(p *spec.Package) {
			search(p).Poll.Query = append(search(p).Poll.Query, spec.PollQueryField{ID: "Project"})
		}, "not a lowercase identifier"},
		{"a duplicate query field", func(p *spec.Package) {
			search(p).Poll.Query = append(search(p).Poll.Query, spec.PollQueryField{ID: "jql"})
		}, "duplicate query field"},
		{"a query field of an unknown type", func(p *spec.Package) { search(p).Poll.Query[0].Type = "date" }, `type "date"`},
		{"items on a paginated operation", func(p *spec.Package) { search(p).Poll.Items = "response.issues" }, "already says where the items are"},
		{"no items on an unpaginated operation", func(p *spec.Package) { feed(p).Poll.Items = "" }, "nothing says where its items are"},
		{"items reading the item", func(p *spec.Package) { feed(p).Poll.Items = "item.entries" }, `reads namespace "item" — this site may read`},
		{"no item id", func(p *spec.Package) { search(p).Poll.ItemID = "" }, "item_id is empty"},
		{"an item id reading the query", func(p *spec.Package) { search(p).Poll.ItemID = "query.jql" }, `reads namespace "query" — this site may read`},
		{"an item version reading the cursor", func(p *spec.Package) { search(p).Poll.ItemVersion = "cursor.value" }, `reads namespace "cursor" — this site may read`},
		{"a cursor with no from", func(p *spec.Package) { search(p).Poll.Cursor.From = "" }, "cursor.from is empty"},
		{"a cursor of an unknown kind", func(p *spec.Package) { search(p).Poll.Cursor.Kind = "sequence" }, `kind "sequence"`},
		{"a negative overlap", func(p *spec.Package) { search(p).Poll.Cursor.Overlap = "-1m" }, "not a non-negative duration"},
		{"an overlap on an opaque cursor", func(p *spec.Package) { search(p).Poll.Cursor.Kind = spec.CursorOpaque }, "opaque token has no before"},
		{"no default interval", func(p *spec.Package) { search(p).Poll.Interval.Default = "" }, "interval.default"},
		{"a zero minimum", func(p *spec.Package) { search(p).Poll.Interval.Min = "0s" }, "interval.min"},
		{"a minimum above the default", func(p *spec.Package) { search(p).Poll.Interval.Min = "10m" }, "above its default"},
		{"an output reading headers an item has not", func(p *spec.Package) {
			search(p).Outputs[0].From = `headers["X-Event"]`
		}, `reads namespace "headers" — this site may read`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pollPackage()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestAPolledSampleProvesWhatThePollerReads: the sample is a recorded answer,
// and each item in it must yield an identity, a cursor the declared kind can
// read, and the outputs — or the package is refused before a poller runs it.
func TestAPolledSampleProvesWhatThePollerReads(t *testing.T) {
	cases := []struct {
		name, sample, want string
	}{
		{"not a response", `{"payload":{}}`, "has no response"},
		{"no item", `{"response":{"issues":[]}}`, "holds no item"},
		{"items that are not a list", `{"response":{"issues":{"key":"ENG-1"}}}`, "not a list"},
		{"an item with no id", `{"response":{"issues":[{"fields":{"updated":"2026-11-17T09:10:00Z"}}]}}`, "item_id"},
		{"an item whose id is an object", `{"response":{"issues":[{"key":{"n":1},"fields":{"updated":"2026-11-17T09:10:00Z"}}]}}`, "item_id"},
		{"an item with no cursor", `{"response":{"issues":[{"key":"ENG-1","fields":{"updated":"2026-11-17T09:10:00Z"},"x":1}]}}`, ""},
		{"a cursor that is no timestamp", `{"response":{"issues":[{"key":"ENG-1","fields":{"updated":"yesterday"}}]}}`, "is not an RFC 3339"},
		{"an output of the wrong type", `{"response":{"issues":[{"key":"ENG-1","fields":{"summary":7,"updated":"2026-11-17T09:10:00Z"}}]}}`, `output "subject" reads float64`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pollPackage()
			p.Triggers[0].SampleBody = []byte(tc.sample)
			err := p.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a well-formed item must pass: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestAPollItemIdentity: the identity the seen set keys on. A numeric id has
// one canonical text; without item_version the item's own digest is its
// revision, so an item that changed is seen again and one that did not is not.
func TestAPollItemIdentity(t *testing.T) {
	poll := &spec.TriggerPoll{ItemID: "item.id"}
	a := map[string]any{"id": float64(42), "title": "one"}
	id, v1, err := spec.PollItemIdentity(poll, nil, a)
	if err != nil || id != "42" || !strings.HasPrefix(v1, "sha256:") {
		t.Fatalf("identity = %q %q %v", id, v1, err)
	}
	_, same, _ := spec.PollItemIdentity(poll, nil, map[string]any{"title": "one", "id": float64(42)})
	if same != v1 {
		t.Fatalf("the same item in another key order has another revision: %s vs %s", same, v1)
	}
	_, v2, _ := spec.PollItemIdentity(poll, nil, map[string]any{"id": float64(42), "title": "two"})
	if v2 == v1 {
		t.Fatal("a changed item kept its revision — it would never be seen again")
	}
}

// TestParsePollTimestamp: the instants vendors write parse, and one that is
// not an instant is an error, never the zero time a cursor would compare as.
func TestParsePollTimestamp(t *testing.T) {
	for _, s := range []string{
		"2026-11-17T09:10:00Z",
		"2026-11-17T09:10:00.123+01:00",
		"2026-11-17T09:10:00.000+0000",
		"Mon, 17 Nov 2026 09:10:00 +0000",
		"Mon, 17 Nov 2026 09:10:00 GMT",
	} {
		if _, err := spec.ParsePollTimestamp(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, v := range []any{"yesterday", "", float64(1731600000), nil} {
		if _, err := spec.ParsePollTimestamp(v); err == nil {
			t.Errorf("%v parsed as an instant", v)
		}
	}
}

// TestAPolledTriggerLoadsFromItsYAML: the format as an author writes it —
// the documented field names, through the strict decoder, with a feed
// operation and its recorded answer on disk.
func TestAPolledTriggerLoadsFromItsYAML(t *testing.T) {
	p := pollPackage()
	p.Ops[0].Operations[2].HTTP.ResponseFormat = spec.ResponseFeed
	p.Triggers = nil
	dir := t.TempDir()
	if err := spec.Write(dir, p); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeTriggers(t, dir, `schema_version: 1
connector: probe
triggers:
  - id: probe.issue.updated
    delivery: poll
    poll:
      operation: probe.issue.search
      params:
        jql: 'query.jql + " AND updated >= " + if(cursor.value, cursor.value, "-1d")'
      query: [ { id: jql, required: true } ]
      item_id: 'item.key'
      item_version: 'item.fields.updated'
      cursor: { from: 'item.fields.updated', kind: timestamp, overlap: 1m }
      interval: { default: 5m, min: 1m }
    outputs:
      - { name: subject, from: 'payload.fields.summary' }
    sample: samples/issue-updated.json
  - id: probe.feed.published
    delivery: poll
    poll:
      operation: probe.feed.read
      items: 'response.items'
      item_id: 'item.id'
      item_version: 'item.version'
      interval: { default: 15m, min: 5m }
    outputs:
      - { name: subject, from: 'payload.title' }
`)
	writeSample(t, dir, "samples/issue-updated.json", `{"response":{"issues":[{"key":"ENG-1","fields":{"summary":"Intake stalls","updated":"2026-11-17T09:10:00.000+0000"}}]}}`)
	loaded, err := spec.Load(dir)
	if err != nil {
		t.Fatalf("load the polled package from its YAML: %v", err)
	}
	if len(loaded.Triggers) != 2 || !loaded.Triggers[0].Polled() || loaded.Triggers[0].Poll.Cursor.Overlap != "1m" {
		t.Fatalf("triggers = %+v", loaded.Triggers)
	}
	op, _ := loaded.Operation("probe.feed.read")
	if op.HTTP.ResponseFormat != spec.ResponseFeed {
		t.Fatalf("response_format = %q, want feed", op.HTTP.ResponseFormat)
	}
}

func writeSample(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWebhookTrigger: the one lookup both delivery lanes route through. A
// polled trigger is never a delivery's, and an empty event name — a delivery
// with no event header — names nothing, even beside a trigger that declares
// none.
func TestWebhookTrigger(t *testing.T) {
	triggers := []spec.Trigger{
		{ID: "probe.issue.polled", Delivery: spec.DeliveryPoll},
		{ID: "probe.issue.commented", Event: "issue_comment"},
		{ID: "probe.issue.commented_again", Event: "issue_comment"},
	}
	if got, ok := spec.WebhookTrigger(triggers, "issue_comment"); !ok || got.ID != "probe.issue.commented" {
		t.Fatalf("issue_comment = %v %v, want the first webhook trigger declaring it", got, ok)
	}
	if got, ok := spec.WebhookTrigger(triggers, ""); ok {
		t.Fatalf("an unnamed delivery reached %s", got.ID)
	}
	if got, ok := spec.WebhookTrigger(triggers, "push"); ok {
		t.Fatalf("an undeclared event reached %s", got.ID)
	}
	// A polled trigger cannot load with an event, but the lookup holds on
	// its own: one built by hand with the delivery's very name is still the
	// poller's, never the delivery's.
	named := []spec.Trigger{{ID: "probe.issue.polled", Delivery: spec.DeliveryPoll, Event: "issue_comment"}}
	if got, ok := spec.WebhookTrigger(named, "issue_comment"); ok {
		t.Fatalf("a delivery reached the polled trigger %s", got.ID)
	}
	// Nor is a hand-built webhook trigger with no event an unnamed
	// delivery's.
	if got, ok := spec.WebhookTrigger([]spec.Trigger{{ID: "probe.issue.bare"}}, ""); ok {
		t.Fatalf("an unnamed delivery reached %s", got.ID)
	}
}
