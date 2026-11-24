package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// fakeDLQSource replays a stream's state and its messages for the DLQ
// pager, recording every sequence the pager probes. A get of sequence 0
// answers what a real broker answers: a bad request (API 10003), not
// message-not-found.
type fakeDLQSource struct {
	first uint64
	last  uint64
	msgs  map[uint64]*jetstream.RawStreamMsg
	gets  []uint64
}

func (f *fakeDLQSource) Info(ctx context.Context, opts ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	return &jetstream.StreamInfo{
		State: jetstream.StreamState{FirstSeq: f.first, LastSeq: f.last},
	}, nil
}

func (f *fakeDLQSource) GetMsg(ctx context.Context, seq uint64, opts ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	f.gets = append(f.gets, seq)
	if seq == 0 {
		return nil, &jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeBadRequest, Description: "bad request"}
	}
	m, ok := f.msgs[seq]
	if !ok {
		return nil, jetstream.ErrMsgNotFound
	}
	return m, nil
}

func dlqSeqs(msgs []DLQMessage) []uint64 {
	seqs := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		seqs = append(seqs, m.Seq)
	}
	return seqs
}

// A DLQ stream that never stored a message (fresh install, freshly
// migrated NATS, every new pool) reports first_seq = last_seq = 0. The
// list must answer an empty page from the state alone — no probe of
// sequence 0, which the broker refuses with a bad request instead of
// message-not-found (#3084).
func TestListDLQNeverWrittenStreamIsEmpty(t *testing.T) {
	src := &fakeDLQSource{first: 0, last: 0}
	msgs, next, err := listDLQFrom(context.Background(), src, 0, 10)
	if err != nil {
		t.Fatalf("list on a never-written stream: %v", err)
	}
	if msgs == nil {
		t.Fatal("list on a never-written stream returned a nil slice, want empty non-nil (the API must encode [])")
	}
	if len(msgs) != 0 {
		t.Fatalf("list on a never-written stream returned %d messages, want 0", len(msgs))
	}
	if next != 0 {
		t.Fatalf("next = %d, want 0", next)
	}
	if len(src.gets) != 0 {
		t.Fatalf("the pager probed sequences %v; a never-written stream must be answered from the state alone", src.gets)
	}
}

// Paging walks stored sequences in order, skips the holes left by
// DiscardDLQ, clamps the cursor to the stream's first sequence and stops
// at the limit with the next cursor.
func TestListDLQPagesOverHoles(t *testing.T) {
	// Stored slots 3..7; 4 and 6 were discarded.
	src := &fakeDLQSource{
		first: 3,
		last:  7,
		msgs: map[uint64]*jetstream.RawStreamMsg{
			3: {Subject: "runs.dlq", Sequence: 3, Data: []byte("a")},
			5: {Subject: "runs.dlq", Sequence: 5, Data: []byte("b")},
			7: {Subject: "runs.dlq", Sequence: 7, Data: []byte("c")},
		},
	}

	// Cursor 0 clamps to the stream's first sequence (3): page one walks
	// 3 (hit), 4 (hole), 5 (hit) and stops at the limit of 2.
	msgs, next, err := listDLQFrom(context.Background(), src, 0, 2)
	if err != nil {
		t.Fatalf("page from cursor 0: %v", err)
	}
	if got := dlqSeqs(msgs); !reflect.DeepEqual(got, []uint64{3, 5}) {
		t.Fatalf("page from cursor 0 seqs = %v, want [3 5]", got)
	}
	if next != 6 {
		t.Fatalf("page from cursor 0 next = %d, want 6", next)
	}

	// An explicit cursor below first clamps to the same page.
	msgs, next, err = listDLQFrom(context.Background(), src, 2, 2)
	if err != nil {
		t.Fatalf("page from cursor 2: %v", err)
	}
	if got := dlqSeqs(msgs); !reflect.DeepEqual(got, []uint64{3, 5}) {
		t.Fatalf("page from cursor 2 seqs = %v, want [3 5] (cursor clamps to first)", got)
	}
	if next != 6 {
		t.Fatalf("page from cursor 2 next = %d, want 6", next)
	}

	// Resuming at the returned cursor walks 6 (hole) and 7 (hit), then
	// exhausts: next 0.
	msgs, next, err = listDLQFrom(context.Background(), src, 6, 2)
	if err != nil {
		t.Fatalf("page from cursor 6: %v", err)
	}
	if got := dlqSeqs(msgs); !reflect.DeepEqual(got, []uint64{7}) {
		t.Fatalf("page from cursor 6 seqs = %v, want [7]", got)
	}
	if next != 0 {
		t.Fatalf("page from cursor 6 next = %d, want 0 (exhausted)", next)
	}
}

// A purge moves FirstSeq past LastSeq (every stored message is gone, the
// sequences behind are burned). Same contract as a never-written stream.
func TestListDLQPurgedStream(t *testing.T) {
	src := &fakeDLQSource{first: 8, last: 7}
	msgs, next, err := listDLQFrom(context.Background(), src, 0, 10)
	if err != nil {
		t.Fatalf("list on a purged stream: %v", err)
	}
	if msgs == nil {
		t.Fatal("list on a purged stream returned a nil slice, want empty non-nil")
	}
	if len(msgs) != 0 {
		t.Fatalf("list on a purged stream returned %d messages, want 0", len(msgs))
	}
	if next != 0 {
		t.Fatalf("next = %d, want 0", next)
	}
	if len(src.gets) != 0 {
		t.Fatalf("the pager probed sequences %v; a purged stream must be answered from the state alone", src.gets)
	}
}

// dlqWitnessPools records the pools TestListPoolDLQOnFreshPool created,
// so the cleanup check after it can assert their streams were actually
// deleted. Empty when the witness never ran (skipped, or filtered out by
// -run), which skips the check too — leftovers of earlier runs on a
// long-lived broker must not false-red it.
var dlqWitnessPools []string

// Witness on a real broker: a pool's DLQ stream straight out of the
// reconciler (never written) lists as empty, lists its first parked
// message as Seq 1, and empties again after the discard. This replays
// the #3084 shape — without the first==0 floor the first list dies on
// the broker's bad request for sequence 0.
func TestListPoolDLQOnFreshPool(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping DLQ list witness (CI: nats-conformance job)")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	pool := "dlqpool" + suffix[len(suffix)-4:]
	if !queue.ValidPoolName(pool) {
		t.Fatalf("test pool name %q must pass ValidPoolName", pool)
	}
	dlqWitnessPools = append(dlqWitnessPools, pool)

	nc, err := natsgo.Connect(uri, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// Cleanup deletes ONLY the two streams this test created — the names
	// derive from a pool unique to this run. The deletes run while the
	// connection is still open and the close comes last: a stream delete
	// over a closed connection can never succeed, and the broker holds the
	// stream forever — MaxAge expires messages, never streams.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range []string{PoolStreamName(pool), PoolDLQStreamName(pool)} {
			if err := js.DeleteStream(cleanupCtx, name); err != nil {
				t.Logf("cleanup: delete stream %s: %v", name, err)
			}
		}
		nc.Close()
	})

	cfg := Config{MaxAge: time.Hour, DLQMaxAge: time.Hour, StreamReplicas: 1, Logger: iterlog.Nop()}
	if err := ensurePoolSchema(ctx, js, cfg, pool); err != nil {
		t.Fatalf("reconciler pool schema: %v", err)
	}

	conn := &Conn{nc: nc, js: js, logger: iterlog.Nop()}

	// Fresh pool: the DLQ lists empty, without probing sequence 0.
	msgs, next, err := conn.ListPoolDLQ(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("list on a fresh pool's DLQ: %v", err)
	}
	if msgs == nil || len(msgs) != 0 || next != 0 {
		t.Fatalf("fresh pool DLQ list = (%v, next %d), want (empty, 0)", msgs, next)
	}

	// One parked message lists as Seq 1.
	if _, err := js.Publish(ctx, PoolDLQSubject(pool), []byte(`{"run_id":"r1"}`)); err != nil {
		t.Fatalf("publish parked message: %v", err)
	}
	msgs, _, err = conn.ListPoolDLQ(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("list after a park: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Seq != 1 {
		t.Fatalf("list after a park = %v (seqs %v), want one row with Seq 1", msgs, dlqSeqs(msgs))
	}

	// Discarding it empties the list again (FirstSeq moves past LastSeq).
	if err := conn.DiscardPoolDLQ(ctx, pool, 1); err != nil {
		t.Fatalf("discard: %v", err)
	}
	msgs, next, err = conn.ListPoolDLQ(ctx, pool, 0, 10)
	if err != nil {
		t.Fatalf("list after a discard: %v", err)
	}
	if msgs == nil || len(msgs) != 0 || next != 0 {
		t.Fatalf("list after a discard = (%v, next %d), want (empty, 0)", msgs, next)
	}
}

// The witness's cleanup must actually delete the two streams it created.
// Deleting them through a connection a deferred Close already shut can
// never succeed ("nats: connection closed"), and the streams then sit on
// the broker forever: MaxAge expires messages, never streams. Runs after
// TestListPoolDLQOnFreshPool (source order) and asserts only the pools
// that witness created, so leftovers of earlier runs on a long-lived
// broker don't false-red it.
func TestListPoolDLQCleanupDeletesWitnessStreams(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping DLQ cleanup check (CI: nats-conformance job)")
	}
	if len(dlqWitnessPools) == 0 {
		t.Skip("witness TestListPoolDLQOnFreshPool did not run (filtered out) — nothing to assert")
	}
	nc, err := natsgo.Connect(uri, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, pool := range dlqWitnessPools {
		for _, name := range []string{PoolStreamName(pool), PoolDLQStreamName(pool)} {
			if _, err := js.Stream(ctx, name); err == nil {
				t.Errorf("stream %s still exists after the witness run — its cleanup did not delete it", name)
			} else if !errors.Is(err, jetstream.ErrStreamNotFound) {
				t.Errorf("cleanup check: stream %s: %v", name, err)
			}
		}
	}
}
