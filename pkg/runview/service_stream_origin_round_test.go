package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newWrappedStreamService builds a passive Service over wrapped, a test
// wrapper around the native store in dir, and stops it on cleanup.
func newWrappedStreamService(t *testing.T, dir string, wrapped store.RunStore) *Service {
	t.Helper()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive(), WithStore(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc
}

// transientAdmissionStore fails LoadRun with a transient OS error while
// armed, and runs onArmedLoad (once) inside the second armed load. It
// schedules the failure; the admission policy stays in the source.
type transientAdmissionStore struct {
	*store.FilesystemRunStore
	armed       atomic.Bool
	loads       atomic.Int32
	onArmedLoad func() error
	once        sync.Once
	hookErr     error
}

func (s *transientAdmissionStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.armed.Load() {
		if s.loads.Add(1) == 2 && s.onArmedLoad != nil {
			s.once.Do(func() { s.hookErr = s.onArmedLoad() })
			return s.FilesystemRunStore.LoadRun(ctx, id)
		}
		return nil, fmt.Errorf("store: load run %s: %w", id, &os.PathError{Op: "open", Path: "run.json", Err: syscall.EMFILE})
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

// collectLogStream drains a log subscription to its end.
func collectLogStream(t *testing.T, sub runstream.LogSubscription) (string, []error) {
	t.Helper()
	var b strings.Builder
	var errs []error
	chunks, errc := sub.Chunks(), sub.Errors()
	deadline := time.After(15 * time.Second)
	for chunks != nil || errc != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			b.Write(c.Data)
		case err, ok := <-errc:
			if !ok {
				errc = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatal("log subscription never ended")
		}
	}
	return b.String(), errs
}

// C1: a transient error loading the run at log admission does not refuse a
// legitimate bound subscription: the producer retries the admission, reads
// nothing before it, and serves the original's log once admitted. A
// permanent refusal met while retrying ends it with that refusal.
func TestStreamSourceLogAdmissionRetriesTransientErrors(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor=%t", successor), func(t *testing.T) {
			dir := t.TempDir()
			native, err := store.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			wrap := &transientAdmissionStore{FilesystemRunStore: native}
			svc := newWrappedStreamService(t, dir, wrap)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runs", "r", "run.log"), []byte("hello A\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			if successor {
				wrap.onArmedLoad = func() error {
					return replaceOriginRun(context.Background(), native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0)
				}
			} else {
				wrap.onArmedLoad = func() error { wrap.armed.Store(false); return nil }
			}
			wrap.armed.Store(true)
			sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
			if err != nil {
				t.Fatalf("a transient admission error refused a legitimate log subscription: %v", err)
			}
			defer sub.Close()
			got, errs := collectLogStream(t, sub)
			if wrap.hookErr != nil {
				t.Fatal(wrap.hookErr)
			}
			if wrap.loads.Load() < 2 {
				t.Fatalf("admission loads = %d, want the retry", wrap.loads.Load())
			}
			if !hasError(errs, syscall.EMFILE) {
				t.Fatalf("errors = %v, want the transient failure as a warning", errs)
			}
			if successor {
				if got != "" || !hasError(errs, store.ErrRunDataIdentityChanged) {
					t.Fatalf("successor admitted: log %q, errors %v", got, errs)
				}
				return
			}
			if got != "hello A\n" {
				t.Fatalf("log = %q, want the original's", got)
			}
			for _, e := range errs {
				if runstream.IsPermanentError(e) {
					t.Fatalf("permanent error on the admitted original: %v", e)
				}
			}
		})
	}
}

// finalReplayRaceStore replaces the run inside the second LoadEventsRange: the
// final replay of the terminal catch-up, after the run was read terminal.
type finalReplayRaceStore struct {
	store.RunStore
	native  *store.FilesystemRunStore
	ranges  atomic.Int32
	hookErr atomic.Value
}

func (s *finalReplayRaceStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	if s.ranges.Add(1) == 2 {
		if err := replaceOriginRun(context.Background(), s.native, id, "B", store.LegacyValueFormat, store.RunStatusFinished, 2); err != nil {
			s.hookErr.Store(err)
		}
	}
	return s.RunStore.LoadEventsRange(ctx, id, from, to, limit)
}

// C2: a refusal of the terminal catch-up's final replay fails the
// subscription instead of ending it clean (which the WS reads as
// `terminated`), and no successor event is delivered.
func TestStreamSourceFinalCatchUpReplayRefusal(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrap := &finalReplayRaceStore{RunStore: native, native: native}
	svc := newWrappedStreamService(t, dir, wrap)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeEvents(boundOriginCtx(t, svc, "r"), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got, errs := drainOriginSubscription(t, sub)
	if e := wrap.hookErr.Load(); e != nil {
		t.Fatal(e)
	}
	if wrap.ranges.Load() < 2 {
		t.Fatalf("replays = %d, want the final replay reached", wrap.ranges.Load())
	}
	for _, ev := range got {
		if ev.NodeID == "B" {
			t.Fatalf("successor event delivered: %+v", ev)
		}
	}
	if !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("errors = %v: the final replay's refusal was not published, the subscription ended clean", errs)
	}
}

// logRangeRaceStore runs before (once) inside the first ReadRunLogRange,
// then forwards the read with the caller's context.
type logRangeRaceStore struct {
	*store.FilesystemRunStore
	before  func() error
	once    sync.Once
	reached atomic.Bool
	hookErr error
}

func (s *logRangeRaceStore) ReadRunLogRange(ctx context.Context, id string, from, until int64) ([]byte, error) {
	s.reached.Store(true)
	s.once.Do(func() {
		if s.before != nil {
			s.hookErr = s.before()
		}
	})
	return s.FilesystemRunStore.ReadRunLogRange(ctx, id, from, until)
}

// C3: on a live buffer whose ring evicted the head, the persisted read that
// fills [from, ring start) is made under the bound context; its permanent
// refusal fails the stream before any ring byte is shipped.
func TestStreamSourceEvictedRingRangeRefusal(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrap := &logRangeRaceStore{FilesystemRunStore: native}
	svc := newWrappedStreamService(t, dir, wrap)
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
		t.Fatal(err)
	}
	buf, _ := svc.prepareRunLog(t.Context(), "r")
	defer svc.dropRunLog("r", buf)
	if _, err := buf.Write([]byte(strings.Repeat("H", 64*1024))); err != nil {
		t.Fatal(err)
	}
	if _, err := buf.Write([]byte(strings.Repeat("f", runLogRingCap))); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	wrap.before = func() error {
		return replaceOriginRun(context.Background(), native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0)
	}
	sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// The buffer stays open, so a stream that survived the refusal would
	// tail it forever: the first chunk is the verdict.
	var errs []error
	chunks, errc := sub.Chunks(), sub.Errors()
	deadline := time.After(15 * time.Second)
	for chunks != nil || errc != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			t.Fatalf("%d bytes shipped at offset %d after the persisted range was refused", len(c.Data), c.Offset)
		case err, ok := <-errc:
			if !ok {
				errc = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatal("log subscription never ended")
		}
	}
	if wrap.hookErr != nil {
		t.Fatal(wrap.hookErr)
	}
	if !wrap.reached.Load() {
		t.Fatal("the evicted range was never read from the persisted log")
	}
	if !hasError(errs, store.ErrRunDataIdentityChanged) {
		t.Fatalf("errors = %v, want the identity refusal", errs)
	}
}

// Run metadata that does not decode answers the same on every retry: a bound
// log subscription refuses it at admission instead of retrying it as
// transient — a corrupt or truncated run.json on the native store.
func TestStreamSourceLogAdmissionRefusesCorruptMetadata(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"syntax", `{"id": "r", not json`},
		{"truncated", `{"id": "r", "workflow_name": "wf"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, native, _ := newOriginStreamService(t)
			ctx := t.Context()
			if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
				t.Fatal(err)
			}
			bound := boundOriginCtx(t, svc, "r")
			path := filepath.Join(svc.storeDir, "runs", "r", "run.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
			if sub != nil {
				_ = sub.Close()
				t.Fatal("undecodable run metadata was admitted for retry as a transient failure")
			}
			if !errors.Is(err, store.ErrRunMetadataUndecodable) {
				t.Fatalf("SubscribeLogs = %v, want ErrRunMetadataUndecodable", err)
			}
		})
	}
}

// schemaRefusalStore answers LoadRun the way the Mongo store answers a run
// written by a newer schema: a typed undecodable-metadata refusal.
type schemaRefusalStore struct {
	*store.FilesystemRunStore
	armed atomic.Bool
}

func (s *schemaRefusalStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.armed.Load() {
		return nil, fmt.Errorf("store/mongo: run %s schema version 99 unknown, upgrade required: %w", id, store.ErrRunMetadataUndecodable)
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

// A Mongo-style schema refusal is final at log admission too.
func TestStreamSourceLogAdmissionRefusesSchemaRefusal(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrap := &schemaRefusalStore{FilesystemRunStore: native}
	svc := newWrappedStreamService(t, dir, wrap)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	wrap.armed.Store(true)
	sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if sub != nil {
		_ = sub.Close()
		t.Fatal("a schema refusal was admitted for retry as a transient failure")
	}
	if !errors.Is(err, store.ErrRunMetadataUndecodable) {
		t.Fatalf("SubscribeLogs = %v, want the schema refusal", err)
	}
}

// finalReplayFlakyStore makes the FINAL catch-up replay (the second
// LoadEventsRange) fail transiently once, after persisting a straggler
// event only that replay can deliver.
type finalReplayFlakyStore struct {
	*store.FilesystemRunStore
	ranges  atomic.Int32
	hookErr atomic.Value
}

func (s *finalReplayFlakyStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	if s.ranges.Add(1) == 2 {
		r, err := s.LoadRun(context.Background(), id)
		if err == nil {
			var bound context.Context
			if bound, err = store.BindRunValueFormat(context.Background(), r); err == nil {
				_, err = s.AppendEvent(bound, id, store.Event{Type: store.EventRunFinished, NodeID: "straggler"})
			}
		}
		if err != nil {
			s.hookErr.Store(err)
		}
		return nil, fmt.Errorf("store: open events: %w", &os.PathError{Op: "open", Path: "events.jsonl", Err: syscall.EMFILE})
	}
	return s.FilesystemRunStore.LoadEventsRange(ctx, id, from, to, limit)
}

// A transient failure of the final catch-up replay does not end the
// subscription: it keeps tailing, the next poll replays from the returned
// cursor, and the terminal events are delivered before a clean end.
func TestStreamSourceFinalReplayRetriesTransientFailure(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrap := &finalReplayFlakyStore{FilesystemRunStore: native}
	svc := newWrappedStreamService(t, dir, wrap)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1); err != nil {
		t.Fatal(err)
	}
	sub, err := svc.StreamSource().SubscribeEvents(boundOriginCtx(t, svc, "r"), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	got, errs := drainOriginSubscription(t, sub)
	if e := wrap.hookErr.Load(); e != nil {
		t.Fatal(e)
	}
	if wrap.ranges.Load() < 3 {
		t.Fatalf("replays = %d: the final replay was not retried", wrap.ranges.Load())
	}
	straggler := false
	for _, ev := range got {
		straggler = straggler || ev.NodeID == "straggler"
	}
	if !straggler {
		t.Fatalf("the terminal straggler was dropped: %d events delivered, errors %v", len(got), errs)
	}
	if !hasError(errs, syscall.EMFILE) {
		t.Fatalf("errors = %v, want the transient failure as a warning", errs)
	}
	for _, e := range errs {
		if runstream.IsPermanentError(e) {
			t.Fatalf("permanent error on a clean end: %v", e)
		}
	}
}

// catchUpFlakyStore fails LoadRun once with a transient OS error after it is
// armed, then forwards.
type catchUpFlakyStore struct {
	*store.FilesystemRunStore
	armed atomic.Bool
}

func (s *catchUpFlakyStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.armed.CompareAndSwap(true, false) {
		return nil, fmt.Errorf("store: load run %s: %w", id, &os.PathError{Op: "open", Path: "run.json", Err: syscall.EMFILE})
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

// A transient failure of the terminal catch-up's run load is published as a
// warning, and the subscription keeps tailing.
func TestStreamSourceCatchUpWarnsTransientLoad(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrap := &catchUpFlakyStore{FilesystemRunStore: native}
	svc := newWrappedStreamService(t, dir, wrap)
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	bound := boundOriginCtx(t, svc, "r")
	wrap.armed.Store(true)
	sub, err := svc.StreamSource().SubscribeEvents(bound, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err, ok := <-sub.Errors():
			if !ok {
				t.Fatal("the subscription ended on a transient catch-up failure")
			}
			if errors.Is(err, syscall.EMFILE) {
				if runstream.IsPermanentError(err) {
					t.Fatalf("transient failure published as permanent: %v", err)
				}
				return
			}
		case _, ok := <-sub.Events():
			if !ok {
				t.Fatal("the subscription ended on a transient catch-up failure")
			}
		case <-deadline:
			t.Fatal("the transient catch-up failure was never published as a warning")
		}
	}
}
