package connectors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// A polled source's witness: the shipped package, loaded through the real
// loader, driven through the pieces one poller tick runs — the params a tick
// builds (spec.NewPollParamsContext), the executor's walk (exec.CallPaged)
// against a vendor double, the items (spec.PollItems), their identity
// (spec.PollItemIdentity), the checkpoint (spec.ParsePollTimestamp over
// cursor.from) and the outputs read off each item as the event's payload.
//
// The double answers the way the VENDOR documents its API, written from that
// documentation and never rendered from the package's own declaration: it
// parses what the request asks and answers only that, so a package that asks
// the wrong question gets the vendor's answer to it, not its own echo. Two
// ticks run, the second after the double revised one item, so the checkpoint
// a first scan leaves and the revision it must notice are both exercised.
//
// The table is the closure: every polled trigger of every shipped package has
// a double, refused otherwise (TestEveryPolledTriggerIsWitnessed).

// pollDouble is one vendor's answer to a poller.
type pollDouble struct {
	// order is the order the vendor's documentation says it answers the
	// trigger's query in: spec.PollOrderOldestFirst, or empty for none. The
	// trigger's poll.order must say the same — a stopped scan moves its
	// checkpoint on that word — and an answer declared oldest first is
	// checked ascending on every walk.
	order string
	// cases are the connections the trigger is driven through, each against
	// a fresh vendor.
	cases []pollCase
}

// pollCase is one connection to the vendor and what the operator typed for
// its poller.
type pollCase struct {
	name string
	// base is appended to the double's origin to form the connection's
	// instance URL (a feed URL's path and query, or nothing).
	base string
	// query is the poller's query values.
	query map[string]any
	// cred is the credential the connection resolves to.
	cred exec.Credential
	// vendor builds a fresh vendor for the case.
	vendor func(t *testing.T) pollVendor
}

// pollVendor is the double's state across the two ticks.
type pollVendor interface {
	http.Handler
	// revise changes one item as its author would between two ticks, and
	// names the item it changed and one it left untouched (both present in
	// the second tick's answer).
	revise() (changed, untouched string)
	// checkTick judges the requests one tick sent: tick 0 carries no
	// checkpoint, tick 1 the checkpoint the first scan left.
	checkTick(t *testing.T, tick int, cp *spec.PollCheckpoint, reqs []recordedRequest)
	// wantOutputs are output values the vendor knows an item carries,
	// keyed by item id: the outputs read off that item must equal them.
	wantOutputs(tick int) map[string]map[string]any
}

// recordedRequest is what the double received.
type recordedRequest struct {
	Method, Path, RawQuery, RequestURI string
	Header                             http.Header
}

var pollDoubles = map[string]pollDouble{}

func registerPoll(triggerID string, d pollDouble) {
	if _, dup := pollDoubles[triggerID]; dup {
		panic("two poll doubles for " + triggerID)
	}
	pollDoubles[triggerID] = d
}

// recorder keeps the requests a double received, under a lock: the walk is
// sequential today, and a recorder that assumed it would race the day it is
// not.
type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recorder) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, recordedRequest{
			Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery,
			RequestURI: req.RequestURI, Header: req.Header.Clone(),
		})
		r.mu.Unlock()
		h.ServeHTTP(w, req)
	})
}

func (r *recorder) take() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.reqs
	r.reqs = nil
	return out
}

// scan is one tick's outcome.
type scan struct {
	ids      []string
	versions map[string]string
	items    map[string]map[string]any
	maxAt    time.Time
}

// TestAShippedPolledTriggerReadsItsVendor is the positive witness per polled
// trigger.
func TestAShippedPolledTriggerReadsItsVendor(t *testing.T) {
	for _, id := range shippedPackages(t) {
		p, err := overlay.LoadPackage(id)
		if err != nil {
			t.Fatalf("load the shipped package %q: %v", id, err)
		}
		for _, trig := range p.Triggers {
			if !trig.Polled() {
				continue
			}
			d, ok := pollDoubles[trig.ID]
			if !ok {
				// TestEveryPolledTriggerIsWitnessed names it.
				continue
			}
			if trig.Poll.Order != d.order {
				t.Errorf("trigger %s declares poll.order %q, and its vendor answers the query in order %q", trig.ID, trig.Poll.Order, d.order)
			}
			for _, c := range d.cases {
				t.Run(trig.ID+"/"+c.name, func(t *testing.T) {
					drivePoll(t, p, trig, c)
				})
			}
		}
	}
}

func drivePoll(t *testing.T, p *spec.Package, trig spec.Trigger, c pollCase) {
	op, ok := p.Operation(trig.Poll.Operation)
	if !ok {
		t.Fatalf("poll.operation %q is not in the package", trig.Poll.Operation)
	}
	v := c.vendor(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec.wrap(v))
	defer srv.Close()
	// The guarded client refuses a loopback address; the double is one, so
	// its client is marked by hand, as the executor's own tests do.
	e := &exec.Executor{Client: exec.MarkGuarded(srv.Client()), BaseURL: srv.URL + c.base}

	// The request is judged before the answer: what the vendor RECEIVED is
	// the witness, and a vendor that refused it says why only after.
	first, err := pollTick(e, p, op, trig, c, nil)
	v.checkTick(t, 0, nil, rec.take())
	if err != nil {
		t.Fatalf("tick 0: %v", err)
	}
	checkOutputs(t, trig, first, v.wantOutputs(0))

	var cp *spec.PollCheckpoint
	if cur := trig.Poll.Cursor; cur != nil {
		if first.maxAt.IsZero() {
			t.Fatalf("the first scan read %d items and no checkpoint", len(first.ids))
		}
		overlap := time.Duration(0)
		if cur.Overlap != "" {
			var err error
			if overlap, err = time.ParseDuration(cur.Overlap); err != nil {
				t.Fatalf("poll.cursor.overlap %q: %v", cur.Overlap, err)
			}
		}
		cp = &spec.PollCheckpoint{Kind: cur.Kind, At: first.maxAt.Add(-overlap)}
	}
	changed, untouched := v.revise()
	second, err := pollTick(e, p, op, trig, c, cp)
	v.checkTick(t, 1, cp, rec.take())
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	checkOutputs(t, trig, second, v.wantOutputs(1))

	// The revision the seen set keys on: an item the vendor changed is a new
	// version under the same id, and one it left alone is not — or the
	// change is dropped as seen, or the untouched item is delivered again on
	// every tick.
	for _, want := range []string{changed, untouched} {
		if _, ok := first.versions[want]; !ok {
			t.Fatalf("item %q is absent from the first scan (ids %v)", want, first.ids)
		}
		if _, ok := second.versions[want]; !ok {
			t.Fatalf("item %q is absent from the second scan (ids %v)", want, second.ids)
		}
	}
	if first.versions[changed] == second.versions[changed] {
		t.Errorf("item %q changed at the vendor and kept its version %q — the seen set would drop the change", changed, first.versions[changed])
	}
	if first.versions[untouched] != second.versions[untouched] {
		t.Errorf("item %q did not change and its version moved %q → %q — it would be delivered again on every tick", untouched, first.versions[untouched], second.versions[untouched])
	}
}

// pollTick runs one tick as a poller does and returns what it observed, or
// why the tick could not complete.
func pollTick(e *exec.Executor, p *spec.Package, op spec.Operation, trig spec.Trigger, c pollCase, cp *spec.PollCheckpoint) (scan, error) {
	poll := trig.Poll
	kind := ""
	if poll.Cursor != nil {
		kind = poll.Cursor.Kind
	}
	pctx := spec.NewPollParamsContext(c.query, kind, cp)
	params := map[string]any{}
	for k, src := range poll.Params {
		v, err := spec.EvalDelivery(src, pctx)
		if err != nil {
			return scan{}, fmt.Errorf("poll.params.%s: %w", k, err)
		}
		if v != nil {
			params[k] = v
		}
	}
	walked, complete, last, err := e.CallPaged(context.Background(), p, op, params, c.cred)
	if err != nil {
		return scan{}, fmt.Errorf("the walk failed: %w", err)
	}
	if !last.OK() {
		return scan{}, fmt.Errorf("the call failed: %v", last.Err)
	}
	if !complete {
		return scan{}, fmt.Errorf("the scan ended incomplete after %d requests — its checkpoint would never move", last.Requests)
	}
	items := walked
	if op.Pagination == nil {
		if items, err = spec.PollItems(op, poll, last.Data); err != nil {
			return scan{}, fmt.Errorf("poll.items over the answer: %w", err)
		}
	}
	if len(items) == 0 {
		return scan{}, fmt.Errorf("the scan read no item — the double always holds some, so the package asked for nothing")
	}
	out := scan{versions: map[string]string{}, items: map[string]map[string]any{}}
	var previous time.Time
	for i, item := range items {
		payload, ok := item.(map[string]any)
		if !ok {
			return scan{}, fmt.Errorf("item %d is %T, not an object — the event's payload is the item", i, item)
		}
		id, version, err := spec.PollItemIdentity(poll, last.Data, item)
		if err != nil {
			return scan{}, fmt.Errorf("item %d: %w", i, err)
		}
		if _, dup := out.versions[id]; dup {
			return scan{}, fmt.Errorf("two items of one scan share id %q", id)
		}
		out.ids = append(out.ids, id)
		out.versions[id] = version
		out.items[id] = payload
		if cur := poll.Cursor; cur != nil {
			raw, err := spec.EvalDelivery(cur.From, spec.NewPollItemContext(last.Data, item))
			if err != nil || raw == nil {
				return scan{}, fmt.Errorf("item %q: poll.cursor.from reads %v (%v)", id, raw, err)
			}
			if cur.Kind == spec.CursorTimestamp {
				at, err := spec.ParsePollTimestamp(raw)
				if err != nil {
					return scan{}, fmt.Errorf("item %q: %w", id, err)
				}
				if poll.Order == spec.PollOrderOldestFirst && at.Before(previous) {
					return scan{}, fmt.Errorf("item %q at %s follows one at %s, and the poll declares %s", id, at.UTC().Format(time.RFC3339Nano), previous.UTC().Format(time.RFC3339Nano), poll.Order)
				}
				previous = at
				if at.After(out.maxAt) {
					out.maxAt = at
				}
			}
		}
	}
	return out, nil
}

// checkOutputs reads every output off every item, as the lane reads a
// payload: each output reads its declared type, each fills on some item, and
// the values the vendor knows an item carries are the ones read.
func checkOutputs(t *testing.T, trig spec.Trigger, s scan, want map[string]map[string]any) {
	t.Helper()
	filled, described := map[string]bool{}, map[string]bool{}
	for _, w := range want {
		for name := range w {
			described[name] = true
		}
	}
	for _, id := range s.ids {
		ctx := spec.NewDeliveryContext(s.items[id], nil, nil)
		for _, o := range trig.Outputs {
			v, err := spec.EvalDelivery(o.From, ctx)
			if err != nil {
				t.Fatalf("item %q: output %s: %v", id, o.Name, err)
			}
			if v == nil {
				// An output the vendor's item carries must read it; one the
				// item does not carry (an RSS entry has no updated) is unset.
				if w, ok := want[id][o.Name]; ok && !emptyValue(w) {
					t.Errorf("item %q: output %s reads nothing, and the vendor's item carries %#v", id, o.Name, w)
				}
				continue
			}
			filled[o.Name] = true
			if !outputIs(v, o.Type) {
				t.Errorf("item %q: output %s reads %T, not a %s", id, o.Name, v, typeOr(o.Type))
			}
			if w, ok := want[id][o.Name]; ok && !reflect.DeepEqual(normalize(v), normalize(w)) {
				t.Errorf("item %q: output %s reads %#v, and the vendor's item carries %#v", id, o.Name, v, w)
			}
		}
		for name := range want[id] {
			if !declaresOutput(trig, name) {
				t.Fatalf("the double expects output %q, which trigger %s does not declare", name, trig.ID)
			}
		}
	}
	for id := range want {
		if _, ok := s.items[id]; !ok {
			t.Fatalf("the double expects item %q, which the scan did not read (ids %v)", id, s.ids)
		}
	}
	for _, o := range trig.Outputs {
		// An output the double describes is judged item by item above; one
		// it does not must read something somewhere, or it reads a field the
		// vendor never sends.
		if !filled[o.Name] && !described[o.Name] {
			t.Errorf("output %s reads nothing on any item the vendor answered", o.Name)
		}
	}
}

// emptyValue reports whether an expected output is nothing at all: absent
// from the vendor's item, so the output is unset.
func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	}
	return false
}

func declaresOutput(trig spec.Trigger, name string) bool {
	for _, o := range trig.Outputs {
		if o.Name == name {
			return true
		}
	}
	return false
}

func typeOr(typ string) string {
	if typ == "" {
		return "string"
	}
	return typ
}

func outputIs(v any, typ string) bool {
	switch typeOr(typ) {
	case "string":
		_, ok := v.(string)
		return ok
	case "list":
		_, ok := v.([]any)
		return ok
	case "map":
		_, ok := v.(map[string]any)
		return ok
	case "bool":
		_, ok := v.(bool)
		return ok
	case "int":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return false
}

// normalize compares two values by their JSON, so a []string the double
// wrote and the []any a decode produced read alike.
func normalize(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(raw)
}

// TestEveryPolledTriggerIsWitnessed closes the table both ways: a shipped
// polled trigger without a double ships a query no vendor ever answered, and
// a double naming no shipped polled trigger witnesses nothing.
func TestEveryPolledTriggerIsWitnessed(t *testing.T) {
	polled := map[string]bool{}
	for _, id := range shippedPackages(t) {
		p, err := overlay.LoadPackage(id)
		if err != nil {
			t.Fatalf("load %q: %v", id, err)
		}
		for _, trig := range p.Triggers {
			if !trig.Polled() {
				continue
			}
			polled[trig.ID] = true
			if _, ok := pollDoubles[trig.ID]; !ok {
				t.Errorf("the shipped polled trigger %q has no vendor double: register one from connectors/poll_%s_test.go", trig.ID, id)
			}
			if trig.Sample == "" {
				t.Errorf("%q: polled trigger %q ships no sample, so nothing proves what it reads", id, trig.ID)
			}
		}
	}
	if len(polled) == 0 {
		t.Fatal("no shipped package declares a polled trigger — the closure would pass by checking nothing")
	}
	for id, d := range pollDoubles {
		if !polled[id] {
			t.Errorf("poll double %q names no shipped polled trigger", id)
		}
		if len(d.cases) == 0 {
			t.Errorf("poll double %q drives no case", id)
		}
	}
}
