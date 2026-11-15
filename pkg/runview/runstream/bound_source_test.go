package runstream

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// contextDiscardingSource ignores the subscription context entirely: nothing
// it does could honor a binding the context carries.
type contextDiscardingSource struct{}

func (contextDiscardingSource) SubscribeEvents(context.Context, string, int64) (EventSubscription, error) {
	return NewEventPipe(), nil
}
func (contextDiscardingSource) SubscribeLogs(context.Context, string, int64) (LogSubscription, error) {
	return NewLogPipe(), nil
}
func (contextDiscardingSource) Close() error { return nil }

// inheritingFileSource embeds a FileSource, inheriting its endorsement while
// overriding the subscription it would vouch for.
type inheritingFileSource struct{ *FileSource }

func (inheritingFileSource) SubscribeEvents(context.Context, string, int64) (EventSubscription, error) {
	return NewEventPipe(), nil
}

func TestRequireBoundObservationSourceAdmitsOnlyItsReceiver(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := NewFileSource(st, "", nil)
	resolved := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return nil, store.ErrRunNotFound }, nil)
	for name, src := range map[string]Source{"file": file, "mongo-resolved": resolved, "mongo-resolved-metrics": resolved.WithMetrics(nil)} {
		if err := RequireBoundObservationSource(src); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	var typedNil *FileSource
	for name, src := range map[string]Source{
		"nil":               nil,
		"typed-nil":         typedNil,
		"discarding":        contextDiscardingSource{},
		"embedding-wrapper": inheritingFileSource{file},
		"mongo-legacy":      NewMongo(nil, nil, nil, nil),
	} {
		if err := RequireBoundObservationSource(src); !errors.Is(err, store.ErrRunDataCapability) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	if err := file.CheckBoundObservationSource(NewFileSource(st, "", nil)); !errors.Is(err, store.ErrRunDataCapability) {
		t.Errorf("another file source endorsed: %v", err)
	}
	if err := resolved.CheckBoundObservationSource(file); !errors.Is(err, store.ErrRunDataCapability) {
		t.Errorf("Mongo source endorsed a file source: %v", err)
	}
}

// The exported Fail keeps the terminal refusal for both pipe kinds even when
// warnings already filled the error buffer.
func TestPipeFailSurvivesSaturatedWarnings(t *testing.T) {
	t.Parallel()
	events, logs := NewEventPipe(), NewLogPipe()
	for i := 0; i < cap(events.errs)+1; i++ {
		events.Warn(errors.New("transient"))
		logs.Warn(errors.New("transient"))
	}
	events.Fail(store.ErrRunDataIdentityChanged)
	logs.Fail(store.ErrRunDeleted)
	events.Finish(nil)
	logs.Finish(nil)
	var gotEvent, gotLog bool
	for err := range events.Errors() {
		gotEvent = gotEvent || errors.Is(err, store.ErrRunDataIdentityChanged)
	}
	for err := range logs.Errors() {
		gotLog = gotLog || errors.Is(err, store.ErrRunDeleted)
	}
	if !gotEvent || !gotLog {
		t.Fatalf("fatal dropped: events=%v logs=%v", gotEvent, gotLog)
	}
}

// Fail and Warn racing Finish from other goroutines never send on a closed
// channel: the three are serialized. Run under -race, an unserialized send
// against the close is reported even when it does not panic.
func TestPipeReportsRacingFinish(t *testing.T) {
	t.Parallel()
	for i := 0; i < 500; i++ {
		p := NewEventPipe()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); <-start; p.Fail(store.ErrRunDataIdentityChanged) }()
		go func() { defer wg.Done(); <-start; p.Warn(errors.New("transient")) }()
		go func() { defer wg.Done(); <-start; p.Finish(nil) }()
		close(start)
		wg.Wait()
		for range p.Errors() {
		}
	}
}

// Fail after Finish is a no-op: the stream is over, and a late refusal must
// not panic the producer by sending on the closed error channel. Fail after a
// consumer's Close, before Finish, still publishes.
func TestPipeFailAfterFinishIsANoOp(t *testing.T) {
	t.Parallel()
	events, logs := NewEventPipe(), NewLogPipe()
	_ = events.Close()
	events.Fail(store.ErrRunDeleted)
	events.Finish(nil)
	logs.Finish(nil)
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		events.Fail(store.ErrRunDataIdentityChanged)
		logs.Fail(store.ErrRunDataIdentityChanged)
	}()
	if panicked != nil {
		t.Fatalf("Fail after Finish panicked: %v", panicked)
	}
	var got []error
	for err := range events.Errors() {
		got = append(got, err)
	}
	if len(got) != 1 || !errors.Is(got[0], store.ErrRunDeleted) {
		t.Fatalf("errors = %v, want only the refusal published before Finish", got)
	}
}
