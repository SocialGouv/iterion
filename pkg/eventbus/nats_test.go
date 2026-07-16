package eventbus

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/SocialGouv/iterion/pkg/trigger"
)

// fakeBroker is an in-process stand-in for the NATS server implementing just
// enough of the wire semantics NATSBus relies on: `<prefix>.>` tail-wildcard
// subject matching and queue-group load-balancing (one delivery per group per
// message). Delivery is synchronous; callbacks are gathered under the lock and
// invoked after release so a handler that re-publishes cannot deadlock.
type fakeBroker struct {
	mu     sync.Mutex
	subs   []*fakeSub
	cursor map[string]int // round-robin position per queue group
}

func newFakeBroker() *fakeBroker { return &fakeBroker{cursor: map[string]int{}} }

type fakeSub struct {
	broker  *fakeBroker
	subject string
	queue   string
	cb      nats.MsgHandler
	active  bool
}

func (s *fakeSub) Unsubscribe() error {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	s.active = false
	return nil
}

func (f *fakeBroker) Publish(subject string, data []byte) error {
	f.deliver(subject, data)
	return nil
}

func (f *fakeBroker) QueueSubscribe(subject, queue string, cb nats.MsgHandler) (natsSubscription, error) {
	s := &fakeSub{broker: f, subject: subject, queue: queue, cb: cb, active: true}
	f.mu.Lock()
	f.subs = append(f.subs, s)
	f.mu.Unlock()
	return s, nil
}

// subjectMatches implements only the `<base>.>` tail wildcard NATSBus uses.
func subjectMatches(pattern, subject string) bool {
	if strings.HasSuffix(pattern, ".>") {
		base := strings.TrimSuffix(pattern, ".>")
		return strings.HasPrefix(subject, base+".")
	}
	return pattern == subject
}

func (f *fakeBroker) deliver(subject string, data []byte) {
	f.mu.Lock()
	// Group matching active subs by queue name.
	groups := map[string][]*fakeSub{}
	var order []string
	for _, s := range f.subs {
		if !s.active || !subjectMatches(s.subject, subject) {
			continue
		}
		if _, seen := groups[s.queue]; !seen {
			order = append(order, s.queue)
		}
		groups[s.queue] = append(groups[s.queue], s)
	}
	// Pick exactly one member per queue group (round-robin), like NATS.
	var targets []*fakeSub
	for _, q := range order {
		members := groups[q]
		idx := f.cursor[q] % len(members)
		f.cursor[q]++
		targets = append(targets, members[idx])
	}
	f.mu.Unlock()

	for _, s := range targets {
		s.cb(&nats.Msg{Subject: subject, Data: data})
	}
}

func mustBus(t *testing.T, b *fakeBroker, opts NATSOptions) *NATSBus {
	t.Helper()
	bus, err := newNATSBus(b, opts)
	if err != nil {
		t.Fatalf("newNATSBus: %v", err)
	}
	return bus
}

func boardEvent(id string) trigger.Event {
	return trigger.Event{ID: id, Source: trigger.SourceBoard, Kind: trigger.KindCardMoved}
}

func TestNATSBus_PublishDecodeAndFilter(t *testing.T) {
	bus := mustBus(t, newFakeBroker(), NATSOptions{})
	var got []trigger.Event
	var mu sync.Mutex
	cancel, err := bus.Subscribe("eval", trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}},
		func(_ context.Context, ev trigger.Event) error {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := bus.Publish(context.Background(), boardEvent("b1")); err != nil {
		t.Fatalf("Publish board: %v", err)
	}
	// A forge event is on a different subject AND filtered out — must not arrive.
	if err := bus.Publish(context.Background(), trigger.Event{ID: "f1", Source: trigger.SourceForge, Kind: "pull_request"}); err != nil {
		t.Fatalf("Publish forge: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].ID != "b1" || got[0].Source != trigger.SourceBoard {
		t.Fatalf("handler got %+v; want exactly the decoded board event b1", got)
	}
}

func TestNATSBus_QueueGroupDeliversOnce(t *testing.T) {
	bus := mustBus(t, newFakeBroker(), NATSOptions{})
	var n atomic.Int64
	inc := func(_ context.Context, _ trigger.Event) error { n.Add(1); return nil }
	// Two subscribers sharing the SAME name = one queue group = one delivery.
	c1, err := bus.Subscribe("evaluator", trigger.Matcher{}, inc)
	if err != nil {
		t.Fatal(err)
	}
	defer c1()
	c2, err := bus.Subscribe("evaluator", trigger.Matcher{}, inc)
	if err != nil {
		t.Fatal(err)
	}
	defer c2()

	if err := bus.Publish(context.Background(), boardEvent("b1")); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("queue group delivered %d times; want exactly 1", got)
	}
}

func TestNATSBus_DistinctNamesEachReceive(t *testing.T) {
	bus := mustBus(t, newFakeBroker(), NATSOptions{})
	var a, b atomic.Int64
	ca, err := bus.Subscribe("a", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error { a.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer ca()
	cb, err := bus.Subscribe("b", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error { b.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer cb()

	if err := bus.Publish(context.Background(), boardEvent("b1")); err != nil {
		t.Fatal(err)
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("distinct groups got a=%d b=%d; want 1 and 1", a.Load(), b.Load())
	}
}

func TestNATSBus_CancelUnsubscribes(t *testing.T) {
	bus := mustBus(t, newFakeBroker(), NATSOptions{})
	var n atomic.Int64
	cancel, err := bus.Subscribe("eval", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error { n.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	cancel() // idempotent
	if err := bus.Publish(context.Background(), boardEvent("b1")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 0 {
		t.Fatalf("handler fired %d times after cancel; want 0", n.Load())
	}
}

func TestNATSBus_UndecodablePayloadDropped(t *testing.T) {
	broker := newFakeBroker()
	bus := mustBus(t, broker, NATSOptions{})
	var n atomic.Int64
	cancel, err := bus.Subscribe("eval", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error { n.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// Inject raw garbage on a matching subject — the decode must fail softly.
	broker.deliver(DefaultSubjectPrefix+".board", []byte("{not valid json"))
	if n.Load() != 0 {
		t.Fatalf("handler fired on undecodable payload; want 0")
	}
}

func TestNATSBus_SubjectFor(t *testing.T) {
	bus := mustBus(t, newFakeBroker(), NATSOptions{SubjectPrefix: "custom.evt."})
	cases := []struct {
		src  trigger.Source
		want string
	}{
		{trigger.SourceBoard, "custom.evt.board"},
		{trigger.SourceRun, "custom.evt.run"},
		{trigger.Source(""), "custom.evt.unknown"},
		{trigger.Source("we.ird"), "custom.evt.unknown"},
	}
	for _, c := range cases {
		if got := bus.subjectFor(trigger.Event{Source: c.src}); got != c.want {
			t.Errorf("subjectFor(%q) = %q; want %q", c.src, got, c.want)
		}
	}
}

func TestNATSBus_ConstructorGuards(t *testing.T) {
	if _, err := NewNATSBus(nil, NATSOptions{}); err == nil {
		t.Error("NewNATSBus(nil) should error")
	}
	bus := mustBus(t, newFakeBroker(), NATSOptions{})
	if _, err := bus.Subscribe("", trigger.Matcher{}, func(context.Context, trigger.Event) error { return nil }); err == nil {
		t.Error("Subscribe with empty name should error (name is the queue group)")
	}
}

// The real *nats.Conn must satisfy the seam through the adapter.
var _ natsConn = realNATSConn{}
