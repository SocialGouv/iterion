package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
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
// last scan left; `response` is the operation's decoded answer and `item`
// one element of it.
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

// PollOrderOldestFirst is the one order a poll may declare its operation
// answers in (poll.order): oldest first, so the pages a scan read before it
// stopped are the oldest of the answer. A timestamp checkpoint may then move
// to the latest instant they hold in full, and an opaque one's last item is
// the newest the scan read.
const PollOrderOldestFirst = "oldest_first"

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
	// Cursor is the checkpoint a complete scan advances, and a stopped one
	// under Order. Without one every scan reads the whole result and the seen
	// set alone removes repeats.
	Cursor *PollCursor `yaml:"cursor,omitempty" json:"cursor,omitempty"`
	// Order is the order the operation answers in: PollOrderOldestFirst, or
	// empty when the package does not say. Declared, a scan of a timestamp
	// cursor that stops before its last page still moves the checkpoint, to
	// the latest instant it read in full; an opaque cursor requires it.
	Order    string       `yaml:"order,omitempty" json:"order,omitempty"`
	Interval PollInterval `yaml:"interval" json:"interval"`
}

// PollQueryField is one value an operator types for a poller.
type PollQueryField struct {
	ID          string `yaml:"id" json:"id"`
	Type        string `yaml:"type,omitempty" json:"type,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	// NotMatching is a regular expression a value must not match: what the
	// package's params would turn into a request the vendor refuses on every
	// tick (a JQL that carries its own ORDER BY). Judged at save.
	NotMatching string `yaml:"not_matching,omitempty" json:"not_matching,omitempty"`
}

// Refuses reports why value is refused by the field's NotMatching, or ""
// when it is not. A pattern that does not compile was refused at load.
func (f PollQueryField) Refuses(value string) string {
	if f.NotMatching == "" {
		return ""
	}
	re, err := regexp.Compile(f.NotMatching)
	if err != nil || !re.MatchString(value) {
		return ""
	}
	return fmt.Sprintf("matches %q, which this field refuses", f.NotMatching)
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

// PollCheckpoint is the checkpoint a tick hands its params, already moved
// back by the cursor's overlap.
type PollCheckpoint struct {
	// Kind is the cursor's kind: CursorTimestamp reads At, CursorOpaque
	// reads Token.
	Kind  string
	At    time.Time
	Token string
}

// cursorFields are what the `cursor` namespace holds per kind. `value` is the
// checkpoint in the form most vendors take — RFC 3339 in UTC, or the token as
// it was read; `epoch_ms` is a timestamp's milliseconds since the epoch, for
// a vendor that reads a bare number in its own clock's terms (Jira's JQL reads
// a quoted date in the searching user's time zone, an unquoted number as
// epoch milliseconds).
var cursorFields = map[string][]string{
	CursorTimestamp: {"value", "epoch_ms"},
	CursorOpaque:    {"value"},
}

// namespace renders the checkpoint as the `cursor` namespace: every field of
// its kind, nil before a scan left a checkpoint.
func (c *PollCheckpoint) namespace(kind string) map[string]any {
	out := map[string]any{}
	for _, f := range cursorFields[kind] {
		out[f] = nil
	}
	if c == nil {
		return out
	}
	switch c.Kind {
	case CursorTimestamp:
		out["value"] = c.At.UTC().Format(time.RFC3339Nano)
		out["epoch_ms"] = strconv.FormatInt(c.At.UnixMilli(), 10)
	case CursorOpaque:
		out["value"] = c.Token
	}
	return out
}

// NewPollParamsContext is the context a poll's params are evaluated under:
// the operator's query values and the last checkpoint, under the `cursor`
// fields of its kind (cursorFields), each nil before a scan left one.
func NewPollParamsContext(query map[string]any, kind string, checkpoint *PollCheckpoint) *expr.Context {
	cursor := checkpoint.namespace(kind)
	return &expr.Context{Named: map[string]func([]string) any{
		NamespaceQuery:  func(p []string) any { return descend(query, p) },
		NamespaceCursor: func(p []string) any { return descend(cursor, p) },
	}}
}

// PollParams evaluates a poll's params for one tick: the operator's typed
// query values and the checkpoint moved back by its overlap. A param that
// evaluates to nil is not sent — the expression said "nothing on this tick"
// (a cursor absent before a scan left one).
func PollParams(poll *TriggerPoll, query map[string]any, checkpoint *PollCheckpoint) (map[string]any, error) {
	kind := ""
	if poll.Cursor != nil {
		kind = poll.Cursor.Kind
	}
	ctx := NewPollParamsContext(query, kind, checkpoint)
	out := map[string]any{}
	for k, src := range poll.Params {
		v, err := evalExpr(src, ctx)
		if err != nil {
			return nil, fmt.Errorf("poll.params.%s: %w", k, err)
		}
		if v != nil {
			out[k] = v
		}
	}
	return out, nil
}

// PollItemCursor reads one item's cursor value (poll.cursor.from) as its
// kind holds it: an instant for a timestamp, the scalar for an opaque token.
func PollItemCursor(poll *TriggerPoll, response, item any) (time.Time, string, error) {
	c := poll.Cursor
	if c == nil {
		return time.Time{}, "", nil
	}
	v, err := evalExpr(c.From, NewPollItemContext(response, item))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("poll.cursor.from: %w", err)
	}
	if c.Kind == CursorTimestamp {
		at, err := ParsePollTimestamp(v)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("poll.cursor.from %q: %w", c.From, err)
		}
		return at, "", nil
	}
	token, err := pollScalar(v)
	if err != nil || token == "" {
		return time.Time{}, "", fmt.Errorf("poll.cursor.from %q reads %v, not a non-empty token (%v)", c.From, v, err)
	}
	return time.Time{}, token, nil
}

// PollOverlap is how far before its checkpoint a timestamp cursor re-reads.
func PollOverlap(poll *TriggerPoll) time.Duration {
	if poll.Cursor == nil || poll.Cursor.Overlap == "" {
		return 0
	}
	d, err := time.ParseDuration(poll.Cursor.Overlap)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// PollIntervals reads a poll's cadence: its default and its floor, both
// validated at load.
func PollIntervals(poll *TriggerPoll) (def, floor time.Duration, err error) {
	def, err = time.ParseDuration(poll.Interval.Default)
	if err != nil {
		return 0, 0, fmt.Errorf("poll.interval.default %q: %w", poll.Interval.Default, err)
	}
	floor, err = time.ParseDuration(poll.Interval.Min)
	if err != nil {
		return 0, 0, fmt.Errorf("poll.interval.min %q: %w", poll.Interval.Min, err)
	}
	return def, floor, nil
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
		return "", "", fmt.Errorf("poll.item_id %q reads %v, not a non-empty string or number (%v)", poll.ItemID, v, err)
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
		return "", "", fmt.Errorf("poll.item_version %q reads %v, not a non-empty string or number (%v)", poll.ItemVersion, v, err)
	}
	return id, version, nil
}

// pollScalar renders an identity value: a string as is, a number in its
// canonical decimal (never an exponent). A float past 2^53 is refused: JSON
// decoding has already rounded it, so two distinct ids would read as one and
// the second item would be dropped as seen.
func pollScalar(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		// 2^53 itself is where two integers first decode to one float.
		if math.Abs(x) >= 1<<53 {
			return "", fmt.Errorf("the number %v is past 2^53, where decoded JSON can no longer tell two ids apart — read a string field", x)
		}
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("%T is not a scalar", v)
}

// instantLayouts are the instants vendors write with a NUMERIC zone:
// RFC 3339 (any fraction), Jira's offset without a colon, and the RFC 822
// family of RSS with one- or two-digit days.
var instantLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC1123Z,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	time.RFC822Z,
	"2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04 -0700",
}

// namedZoneLayouts are the same RFC 822 dates ending in a zone NAME, which
// is read from rfc822Zones and never from time.Parse: Parse gives a name it
// does not know an offset of zero, and a name it does know the offset of the
// process's own location, so "EST" would mean one instant under TZ=UTC and
// another five hours later under TZ=America/New_York.
var namedZoneLayouts = []string{
	"Mon, 02 Jan 2006 15:04:05",
	"Mon, 2 Jan 2006 15:04:05",
	"02 Jan 06 15:04",
	"2 Jan 2006 15:04:05",
	"Mon, 2 Jan 2006 15:04",
}

// rfc822Zones are the zone names RFC 822 §5.1 defines, with their offsets.
var rfc822Zones = map[string]int{
	"UT": 0, "UTC": 0, "GMT": 0, "Z": 0,
	"EST": -5, "EDT": -4, "CST": -6, "CDT": -5,
	"MST": -7, "MDT": -6, "PST": -8, "PDT": -7,
}

// ParseInstant reads an instant a vendor wrote: a numeric zone, or one of
// the zone names RFC 822 defines. Any other zone name is an error, never a
// guess.
func ParseInstant(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range instantLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		head, zone := s[:i], s[i+1:]
		if hours, ok := rfc822Zones[strings.ToUpper(zone)]; ok {
			loc := time.FixedZone(strings.ToUpper(zone), hours*3600)
			for _, layout := range namedZoneLayouts {
				if t, err := time.ParseInLocation(layout, head, loc); err == nil {
					return t, nil
				}
			}
		} else if zone != "" && strings.Trim(zone, "+-0123456789") != "" {
			for _, layout := range namedZoneLayouts {
				if _, err := time.Parse(layout, head); err == nil {
					return time.Time{}, fmt.Errorf("%q names zone %q, which is not one RFC 822 defines (UT, GMT, EST, EDT, CST, CDT, MST, MDT, PST, PDT) — write a numeric offset", s, zone)
				}
			}
		}
	}
	return time.Time{}, fmt.Errorf("%q is not an RFC 3339, Jira or RFC 822 instant", s)
}

// ParsePollTimestamp reads a timestamp cursor value.
func ParsePollTimestamp(v any) (time.Time, error) {
	str, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("a timestamp cursor reads %T, not a string", v)
	}
	return ParseInstant(str)
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
		if q.NotMatching != "" {
			if _, err := regexp.Compile(q.NotMatching); err != nil {
				return where("query field %q: not_matching %q is not a regular expression: %v", q.ID, q.NotMatching, err)
			}
		}
		if q.Type != "" && !slices.Contains(pollQueryTypes, q.Type) {
			return where("query field %q has type %q, not one of %s", q.ID, q.Type, strings.Join(pollQueryTypes, ", "))
		}
	}
	keys := map[string]bool{}
	for _, prm := range op.Params {
		keys[prm.Key] = true
	}
	// The walk, as exec.CallPaged runs it: the page parameter on every call
	// of a page_number or offset walk, the size parameter on every call when
	// the pagination declares a default size, the cursor parameter only from
	// the second page on — each under the key its wire name maps to.
	walked := map[string]bool{}
	owned := map[string]bool{}
	pg := op.Pagination
	if pg != nil {
		// The style itself is judged with the operation (ValidPaginationStyle
		// is the set the walk follows).
		pageKey, cursorKey := ParamKeyForWire(op, pg.PageParam), ParamKeyForWire(op, pg.CursorParam)
		if pageKey != "" {
			owned[pageKey] = true
			if pg.Style != PageCursor {
				walked[pageKey] = true
			}
		}
		if cursorKey != "" {
			owned[cursorKey] = true
		}
		if pg.SizeParam != "" && pg.DefaultSize > 0 {
			walked[ParamKeyForWire(op, pg.SizeParam)] = true
		}
	}
	required := map[string]bool{}
	for _, prm := range op.Params {
		if prm.Required && prm.Default == nil {
			required[prm.Key] = true
		}
	}
	for k, src := range poll.Params {
		if !keys[k] {
			return where("poll.params names %q, which operation %s does not declare", k, op.ID)
		}
		if owned[k] {
			// Prevents: a walk that reads one hand-picked page, reports it
			// incomplete, and so never moves the checkpoint.
			return where("poll.params builds %q, which the pagination walk owns — the walk would read that one page, never complete, and the checkpoint would never move", k)
		}
		cursorKind := ""
		if poll.Cursor != nil {
			cursorKind = poll.Cursor.Kind
		}
		if err := checkPollRefs(fmt.Sprintf("poll.params.%s", k), src, queryIDs, cursorKind); err != nil {
			return where("%v", err)
		}
	}
	for k := range required {
		if _, built := poll.Params[k]; built || walked[k] {
			continue
		}
		if owned[k] {
			return where("operation %s requires %q, which the pagination walk sends only from the second page — its first page cannot be asked for", op.ID, k)
		}
		return where("operation %s requires %q, which poll.params does not build", op.ID, k)
	}
	// Each tick's params, evaluated as the poller will: on the first tick no
	// checkpoint exists and the optional query fields may be unset, later
	// ticks carry one. A required parameter that builds nothing, or an
	// expression that fails, fails every such tick.
	firstQuery := map[string]any{}
	for _, q := range poll.Query {
		if q.Required {
			firstQuery[q.ID] = pollQueryPlaceholder(q.Type)
		}
	}
	ticks := []struct {
		name       string
		checkpoint *PollCheckpoint
	}{{"the first tick (no checkpoint yet, optional query fields unset)", nil}}
	cursorKind := ""
	if poll.Cursor != nil {
		cursorKind = poll.Cursor.Kind
		ticks = append(ticks, struct {
			name       string
			checkpoint *PollCheckpoint
		}{"a later tick (a checkpoint set, optional query fields unset)", pollCursorPlaceholder(poll.Cursor.Kind)})
	}
	for _, tick := range ticks {
		ctx := NewPollParamsContext(firstQuery, cursorKind, tick.checkpoint)
		for k, src := range poll.Params {
			v, err := evalExpr(src, ctx)
			if err != nil {
				return where("poll.params.%s fails on %s: %v", k, tick.name, err)
			}
			if v == nil && required[k] {
				return where("poll.params.%s builds nothing on %s, and operation %s requires it", k, tick.name, op.ID)
			}
		}
	}
	readsCursor := false
	for _, src := range poll.Params {
		if ast, err := expr.Parse(src); err == nil {
			for _, r := range ast.Refs() {
				readsCursor = readsCursor || r.Namespace == NamespaceCursor
			}
		}
	}
	switch {
	case readsCursor && poll.Cursor == nil:
		return where("poll.params read cursor.value, but the poll declares no cursor: block — the value would be nil on every tick")
	case !readsCursor && poll.Cursor != nil:
		return where("the poll declares a cursor: block no param reads — the checkpoint would never narrow a request")
	}
	switch {
	case pg != nil && poll.Items != "":
		return where("poll.items is set, but operation %s paginates and its pagination already says where the items are", op.ID)
	case pg == nil && poll.Items == "":
		return where("poll.items is empty, and operation %s does not paginate — nothing says where its items are", op.ID)
	case poll.Items != "":
		if err := checkExpr("poll.items", poll.Items, NamespaceResponse); err != nil {
			return where("%v", err)
		}
	}
	// What an item's identity, revision and cursor may read. A paginated
	// walk hands back its LAST page as the response, so on such an operation
	// `response` would describe every item by another page's fields.
	itemNS := []string{NamespaceResponse, NamespaceItem}
	if pg != nil {
		itemNS = []string{NamespaceItem}
	}
	if strings.TrimSpace(poll.ItemID) == "" {
		return where("poll.item_id is empty — an item seen twice would be delivered twice")
	}
	if err := checkExpr("poll.item_id", poll.ItemID, itemNS...); err != nil {
		return where("%v", err)
	}
	if poll.ItemVersion != "" {
		if err := checkExpr("poll.item_version", poll.ItemVersion, itemNS...); err != nil {
			return where("%v", err)
		}
	}
	if poll.Order != "" && poll.Order != PollOrderOldestFirst {
		// Prevents: an order a poller would be thought to rely on and does
		// not read — oldest first is the one a checkpoint can rest on.
		return where("poll.order %q is not %s — the one order a poll declares; leave it unset when the operation answers in another", poll.Order, PollOrderOldestFirst)
	}
	if c := poll.Cursor; c != nil {
		if strings.TrimSpace(c.From) == "" {
			return where("poll.cursor.from is empty")
		}
		if err := checkExpr("poll.cursor.from", c.From, itemNS...); err != nil {
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
			if poll.Order != PollOrderOldestFirst {
				// Prevents: a checkpoint at the last item of an answer whose
				// last item is not its newest — the next scan starts behind
				// what it read, or past what it never did.
				return where("poll.cursor.kind opaque takes the last item's token, so its operation must answer oldest first — declare poll.order: %s", PollOrderOldestFirst)
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
	if min < time.Second {
		// Prevents: a vendor called in a busy loop. ITERION_CONNECTOR_POLL_MIN_INTERVAL
		// is the operator's dial on the cadence; a package asks in seconds.
		return where("poll.interval.min %s is below a second — a poll interval is counted in seconds", min)
	}
	if min > def {
		return where("poll.interval.min %s is above its default %s", min, def)
	}
	return nil
}

// pollQueryPlaceholder is a value of a query field's type, standing for what
// an operator typed when the params are evaluated at load.
func pollQueryPlaceholder(typ string) any {
	switch typ {
	case "int":
		// The type a tick hands its params (binding.PollQueryValues).
		return int64(1)
	case "bool":
		return true
	}
	return "q"
}

// pollCursorPlaceholder stands for a checkpoint of the cursor's kind.
func pollCursorPlaceholder(kind string) *PollCheckpoint {
	if kind == CursorTimestamp {
		return &PollCheckpoint{Kind: kind, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	return &PollCheckpoint{Kind: kind, Token: "token"}
}

// checkPollRefs checks a params expression reads `query` and `cursor` only,
// names declared query fields — a typo in a query id reads nil on every tick
// and builds a request the operator never wrote — and reads only the cursor
// fields its kind holds (cursorFields).
func checkPollRefs(where, src string, queryIDs map[string]bool, cursorKind string) error {
	if err := checkExpr(where, src, NamespaceQuery, NamespaceCursor); err != nil {
		return err
	}
	ast, err := expr.Parse(src)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	for _, r := range ast.Refs() {
		if (r.Namespace == NamespaceQuery || r.Namespace == NamespaceCursor) && len(r.Path) == 0 {
			// Prevents: query["typo"] or cursor[expr], which no field check
			// can see — a field is read by its dotted name.
			return fmt.Errorf("%s: %q reads %s by index — read %s.<field> by name", where, src, r.Namespace, r.Namespace)
		}
		if r.Namespace == NamespaceQuery && len(r.Path) > 0 && !queryIDs[r.Path[0]] {
			return fmt.Errorf("%s: %q reads query.%s, which the poll's query does not declare", where, src, r.Path[0])
		}
		if r.Namespace == NamespaceCursor && len(r.Path) > 0 {
			// With no cursor: block, or one of a kind this build does not
			// know, a read of cursor.value is left to the rules that name the
			// missing block or the unknown kind.
			held, known := cursorFields[cursorKind]
			kind := cursorKind
			if !known {
				held, kind = cursorFields[CursorOpaque], "declared"
			}
			if !slices.Contains(held, r.Path[0]) {
				return fmt.Errorf("%s: %q reads cursor.%s — a cursor of kind %s holds %s only", where, src, r.Path[0], kind, strings.Join(held, ", "))
			}
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
	read := map[string]bool{}
	for i, item := range items {
		payload, ok := item.(map[string]any)
		if !ok {
			// Prevents: outputs that all read nil off an item that is not an
			// object — the event's payload is the item.
			return fmt.Errorf("connector %q: trigger %q: sample item %d is %T, not an object — an item is the event's payload", id, t.ID, i, item)
		}
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
			} else if _, err := pollScalar(v); err != nil {
				return fmt.Errorf("connector %q: trigger %q: sample item %d: an opaque cursor must be a string or number: %w", id, t.ID, i, err)
			}
		}
		ctx := NewDeliveryContext(payload, nil, nil)
		if err := checkExtraction(id, t, ctx); err != nil {
			return err
		}
		for _, o := range t.Outputs {
			if v, err := evalExpr(o.From, ctx); err == nil && v != nil {
				read[o.Name] = true
			}
		}
	}
	// Prevents: a misspelt output, which reads nil on every item forever.
	for _, o := range t.Outputs {
		if !read[o.Name] {
			return fmt.Errorf("connector %q: trigger %q: output %q reads nothing on any item of sample %q", id, t.ID, o.Name, t.Sample)
		}
	}
	return nil
}
