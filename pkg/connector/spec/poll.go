package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// The two ways a trigger's events reach iterion (ADR-168 §5). A webhook
// trigger is matched by the vendor's event name on a delivery the ingress
// authenticated; a polled trigger is read by its own poller, which calls one
// of the package's operations on an interval.
const (
	DeliveryWebhook = "webhook"
	DeliveryPoll    = "poll"
)

// The namespaces a `poll:` block's expressions read (ADR-168 §9). `query`
// is what the operator typed for this poller, `cursor` the checkpoint the
// last complete scan left; `response` is the operation's decoded answer and
// `item` one element of it.
const (
	NamespaceQuery    = "query"
	NamespaceCursor   = "cursor"
	NamespaceResponse = "response"
	NamespaceItem     = "item"
)

// The cursor kinds. A timestamp is ordered and re-read with an overlap; an
// opaque cursor is a vendor token handed back as is.
const (
	CursorTimestamp = "timestamp"
	CursorOpaque    = "opaque"
)

// TriggerPoll is how a polled trigger reads its events: one read operation
// of the same package, called on an interval with parameters built from the
// operator's query and the last checkpoint.
type TriggerPoll struct {
	// Operation is the id of the read operation the poller calls.
	Operation string `yaml:"operation" json:"operation"`
	// Params maps an operation parameter key to an expression over `query`
	// and `cursor`.
	Params map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	// Query declares the values an operator types for one poller (a JQL, a
	// feed URL). Their digest is part of the poller's identity.
	Query []PollQueryField `yaml:"query,omitempty" json:"query,omitempty"`
	// Items locates the items in the response of an operation the package
	// does not paginate. A paginated operation's items are its pagination's
	// own, so Items is refused there: one place says where the items are.
	Items string `yaml:"items,omitempty" json:"items,omitempty"`
	// ItemID is an item's identity, ItemVersion its revision. An item seen
	// at (id, version) is never delivered twice. Without ItemVersion the
	// revision is the digest of the item itself.
	ItemID      string `yaml:"item_id" json:"item_id"`
	ItemVersion string `yaml:"item_version,omitempty" json:"item_version,omitempty"`
	// Cursor is the checkpoint a complete scan advances. Without one every
	// scan reads the whole result and the seen set alone removes repeats.
	Cursor   *PollCursor  `yaml:"cursor,omitempty" json:"cursor,omitempty"`
	Interval PollInterval `yaml:"interval" json:"interval"`
}

// PollQueryField is one value an operator types for a poller.
type PollQueryField struct {
	ID          string `yaml:"id" json:"id"`
	Type        string `yaml:"type,omitempty" json:"type,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// PollCursor reads the checkpoint off each item.
type PollCursor struct {
	From string `yaml:"from" json:"from"`
	Kind string `yaml:"kind" json:"kind"`
	// Overlap re-reads this much before a timestamp checkpoint, so equal
	// instants and records that become visible late are seen again.
	Overlap string `yaml:"overlap,omitempty" json:"overlap,omitempty"`
}

// PollInterval is the cadence: Default unless an operator asks for another,
// never below Min unless ITERION_CONNECTOR_POLL_MIN_INTERVAL lowers it.
type PollInterval struct {
	Default string `yaml:"default" json:"default"`
	Min     string `yaml:"min" json:"min"`
}

// SampleResponse is a polled trigger's recorded sample: one decoded answer of
// its operation (one page, when the operation paginates).
type SampleResponse struct {
	Response any `json:"response"`
}

var pollQueryTypes = []string{"string", "int", "bool"}

// Polled reports whether the trigger is read by a poller.
func (t Trigger) Polled() bool { return t.Delivery == DeliveryPoll }

// NewPollParamsContext is the context a poll's params are evaluated under:
// the operator's query values and the last checkpoint, under `cursor.value`
// (nil before the first complete scan).
func NewPollParamsContext(query map[string]any, checkpoint any) *expr.Context {
	cursor := map[string]any{"value": checkpoint}
	return &expr.Context{Named: map[string]func([]string) any{
		NamespaceQuery:  func(p []string) any { return descend(query, p) },
		NamespaceCursor: func(p []string) any { return descend(cursor, p) },
	}}
}

// NewPollItemContext is the context an item's identity, version and cursor
// are read under: the whole decoded response and the one item.
func NewPollItemContext(response, item any) *expr.Context {
	return &expr.Context{Named: map[string]func([]string) any{
		NamespaceResponse: func(p []string) any { return descend(response, p) },
		NamespaceItem:     func(p []string) any { return descend(item, p) },
	}}
}

// PollItems extracts the items of one decoded response: the pagination's
// items field for a paginated operation, the poll's Items expression
// otherwise. An answer that holds no list is an error, never an empty scan:
// a scan that read nothing would move no checkpoint and say nothing.
func PollItems(op Operation, poll *TriggerPoll, response any) ([]any, error) {
	if op.Pagination != nil {
		if op.Pagination.ItemsField == "" {
			list, ok := response.([]any)
			if !ok {
				return nil, fmt.Errorf("operation %s paginates a bare array, and the response is %T", op.ID, response)
			}
			return list, nil
		}
		v := descend(response, strings.Split(op.Pagination.ItemsField, "."))
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("operation %s's items field %q holds %T, not a list", op.ID, op.Pagination.ItemsField, v)
		}
		return list, nil
	}
	v, err := evalExpr(poll.Items, NewPollItemContext(response, nil))
	if err != nil {
		return nil, fmt.Errorf("poll.items: %w", err)
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("poll.items %q reads %T, not a list", poll.Items, v)
	}
	return list, nil
}

// PollItemIdentity reads one item's id and version. The version defaults to
// the digest of the item's canonical JSON, so an item that changed is a new
// revision even when the vendor states none.
func PollItemIdentity(poll *TriggerPoll, response, item any) (id, version string, err error) {
	ctx := NewPollItemContext(response, item)
	v, err := evalExpr(poll.ItemID, ctx)
	if err != nil {
		return "", "", fmt.Errorf("poll.item_id: %w", err)
	}
	id, err = pollScalar(v)
	if err != nil || id == "" {
		return "", "", fmt.Errorf("poll.item_id %q reads %v, not a non-empty string or number", poll.ItemID, v)
	}
	if poll.ItemVersion == "" {
		raw, err := json.Marshal(item)
		if err != nil {
			return "", "", fmt.Errorf("digest the item: %w", err)
		}
		sum := sha256.Sum256(raw)
		return id, "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	v, err = evalExpr(poll.ItemVersion, ctx)
	if err != nil {
		return "", "", fmt.Errorf("poll.item_version: %w", err)
	}
	version, err = pollScalar(v)
	if err != nil || version == "" {
		return "", "", fmt.Errorf("poll.item_version %q reads %v, not a non-empty string or number", poll.ItemVersion, v)
	}
	return id, version, nil
}

// pollScalar renders an identity value: a string as is, a number in its
// canonical decimal. Anything else has no stable text and is refused.
func pollScalar(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case float64, int64, int:
		b, err := json.Marshal(x)
		return string(b), err
	}
	return "", fmt.Errorf("%T is not a scalar", v)
}

// pollTimestampLayouts are the instants vendors write: RFC 3339 (with or
// without fractional seconds), Jira's offset without a colon, and the
// RFC 1123 dates of RSS.
var pollTimestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC1123Z,
	time.RFC1123,
}

// ParsePollTimestamp reads a timestamp cursor value.
func ParsePollTimestamp(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("a timestamp cursor reads %T, not a string", v)
	}
	for _, layout := range pollTimestampLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC 3339, Jira or RFC 1123 timestamp", s)
}

// validatePoll refuses a polled trigger no poller could run, and a poll:
// block on a trigger nothing polls.
func validatePoll(p *Package, t Trigger) error {
	id := p.Connector.ID
	where := func(f string, a ...any) error {
		return fmt.Errorf("connector %q: trigger %q: "+f, append([]any{id, t.ID}, a...)...)
	}
	if !t.Polled() {
		if t.Poll != nil {
			// Prevents: a poll block an operator reads as wired and that no
			// poller ever runs.
			return where("declares a poll: block but its delivery is webhook — set delivery: poll")
		}
		return nil
	}
	poll := t.Poll
	if poll == nil {
		return where("delivery: poll declares no poll: block — a poller would not know what to call")
	}
	if t.Event != "" {
		// Prevents: a webhook delivery carrying that event name reaching a
		// trigger its ingress was never meant to feed.
		return where("a polled trigger declares no event — its poller names it, and an event here would let a webhook delivery reach it")
	}
	op, ok := p.Operation(poll.Operation)
	if !ok {
		return where("poll.operation %q is not an operation of this package", poll.Operation)
	}
	if op.Effect != EffectRead {
		// Prevents: a vendor-side effect repeated every interval.
		return where("poll.operation %q has effect %q — a poller repeats its call every interval, so it calls read operations only", op.ID, op.Effect)
	}
	queryIDs := map[string]bool{}
	for _, q := range poll.Query {
		if !identifierRe.MatchString(q.ID) {
			return where("query field %q is not a lowercase identifier", q.ID)
		}
		if queryIDs[q.ID] {
			return where("duplicate query field %q", q.ID)
		}
		queryIDs[q.ID] = true
		if q.Type != "" && !slices.Contains(pollQueryTypes, q.Type) {
			return where("query field %q has type %q, not one of %s", q.ID, q.Type, strings.Join(pollQueryTypes, ", "))
		}
	}
	keys := map[string]bool{}
	for _, prm := range op.Params {
		keys[prm.Key] = true
	}
	for k, src := range poll.Params {
		if !keys[k] {
			return where("poll.params names %q, which operation %s does not declare", k, op.ID)
		}
		if err := checkPollRefs(fmt.Sprintf("poll.params.%s", k), src, queryIDs); err != nil {
			return where("%v", err)
		}
	}
	// The walk writes the pagination's own parameters; any other required
	// parameter must be built here, or every tick fails the same way.
	walked := map[string]bool{}
	if pg := op.Pagination; pg != nil {
		walked[pg.PageParam], walked[pg.SizeParam], walked[pg.CursorParam] = true, true, true
	}
	for _, prm := range op.Params {
		if prm.Required && prm.Default == nil && !walked[prm.Key] {
			if _, ok := poll.Params[prm.Key]; !ok {
				return where("operation %s requires %q, which poll.params does not build", op.ID, prm.Key)
			}
		}
	}
	switch {
	case op.Pagination != nil && poll.Items != "":
		return where("poll.items is set, but operation %s paginates and its pagination already says where the items are", op.ID)
	case op.Pagination == nil && poll.Items == "":
		return where("poll.items is empty, and operation %s does not paginate — nothing says where its items are", op.ID)
	case poll.Items != "":
		if err := checkExpr("poll.items", poll.Items, NamespaceResponse); err != nil {
			return where("%v", err)
		}
	}
	if strings.TrimSpace(poll.ItemID) == "" {
		return where("poll.item_id is empty — an item seen twice would be delivered twice")
	}
	if err := checkExpr("poll.item_id", poll.ItemID, NamespaceResponse, NamespaceItem); err != nil {
		return where("%v", err)
	}
	if poll.ItemVersion != "" {
		if err := checkExpr("poll.item_version", poll.ItemVersion, NamespaceResponse, NamespaceItem); err != nil {
			return where("%v", err)
		}
	}
	if c := poll.Cursor; c != nil {
		if strings.TrimSpace(c.From) == "" {
			return where("poll.cursor.from is empty")
		}
		if err := checkExpr("poll.cursor.from", c.From, NamespaceResponse, NamespaceItem); err != nil {
			return where("%v", err)
		}
		switch c.Kind {
		case CursorTimestamp:
			if c.Overlap != "" {
				if d, err := time.ParseDuration(c.Overlap); err != nil || d < 0 {
					return where("poll.cursor.overlap %q is not a non-negative duration", c.Overlap)
				}
			}
		case CursorOpaque:
			if c.Overlap != "" {
				return where("poll.cursor.overlap applies to a timestamp cursor; an opaque token has no before")
			}
		default:
			return where("poll.cursor.kind %q is not %s or %s", c.Kind, CursorTimestamp, CursorOpaque)
		}
	}
	def, err := time.ParseDuration(poll.Interval.Default)
	if err != nil || def <= 0 {
		return where("poll.interval.default %q is not a positive duration", poll.Interval.Default)
	}
	min, err := time.ParseDuration(poll.Interval.Min)
	if err != nil || min <= 0 {
		return where("poll.interval.min %q is not a positive duration", poll.Interval.Min)
	}
	if min > def {
		return where("poll.interval.min %s is above its default %s", min, def)
	}
	return nil
}

// checkPollRefs checks a params expression reads `query` and `cursor` only,
// and names declared query fields: a typo in a query id reads nil on every
// tick and builds a request the operator never wrote.
func checkPollRefs(where, src string, queryIDs map[string]bool) error {
	if err := checkExpr(where, src, NamespaceQuery, NamespaceCursor); err != nil {
		return err
	}
	ast, err := expr.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	for _, r := range ast.Refs() {
		if r.Namespace == NamespaceQuery && len(r.Path) > 0 && !queryIDs[r.Path[0]] {
			return fmt.Errorf("%s: %q reads query.%s, which the poll's query does not declare", where, src, r.Path[0])
		}
		if r.Namespace == NamespaceCursor && len(r.Path) > 0 && r.Path[0] != "value" {
			return fmt.Errorf("%s: %q reads cursor.%s — the cursor holds value only", where, src, r.Path[0])
		}
	}
	return nil
}

// checkPollSample proves a polled trigger against its recorded response:
// the items are located, every item has an identity and a cursor the
// declared kind can read, and every output, filter and loop guard reads the
// item as the event's payload.
func checkPollSample(p *Package, t Trigger) error {
	id := p.Connector.ID
	var s SampleResponse
	if err := json.Unmarshal(t.SampleBody, &s); err != nil {
		return fmt.Errorf("connector %q: trigger %q: sample %q is not a {response} JSON answer: %w", id, t.ID, t.Sample, err)
	}
	if s.Response == nil {
		return fmt.Errorf("connector %q: trigger %q: sample %q has no response", id, t.ID, t.Sample)
	}
	op, _ := p.Operation(t.Poll.Operation)
	items, err := PollItems(op, t.Poll, s.Response)
	if err != nil {
		return fmt.Errorf("connector %q: trigger %q: the sample's items: %w", id, t.ID, err)
	}
	if len(items) == 0 {
		return fmt.Errorf("connector %q: trigger %q: sample %q holds no item — it proves nothing the trigger reads", id, t.ID, t.Sample)
	}
	for i, item := range items {
		if _, _, err := PollItemIdentity(t.Poll, s.Response, item); err != nil {
			return fmt.Errorf("connector %q: trigger %q: sample item %d: %w", id, t.ID, i, err)
		}
		if c := t.Poll.Cursor; c != nil {
			v, err := evalExpr(c.From, NewPollItemContext(s.Response, item))
			if err != nil || v == nil {
				return fmt.Errorf("connector %q: trigger %q: sample item %d: poll.cursor.from reads nothing (%v)", id, t.ID, i, err)
			}
			if c.Kind == CursorTimestamp {
				if _, err := ParsePollTimestamp(v); err != nil {
					return fmt.Errorf("connector %q: trigger %q: sample item %d: %w", id, t.ID, i, err)
				}
			}
		}
		payload, _ := item.(map[string]any)
		ctx := NewDeliveryContext(payload, nil, nil)
		if err := checkExtraction(id, t, ctx); err != nil {
			return err
		}
	}
	return nil
}
