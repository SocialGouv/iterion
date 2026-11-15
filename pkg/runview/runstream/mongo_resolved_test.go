package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type resolvedSourceBlob struct{ nopBlob }

func (b resolvedSourceBlob) PutArtifactForFormat(c context.Context, _ int, r, n string, v int, d []byte) error {
	return b.PutArtifact(c, r, n, v, d)
}
func (b resolvedSourceBlob) GetArtifactForFormat(c context.Context, _ int, r, n string, v int) ([]byte, error) {
	return b.GetArtifact(c, r, n, v)
}
func (b resolvedSourceBlob) ListArtifactVersionsForFormat(c context.Context, _ int, r, n string) ([]int, error) {
	return b.ListArtifactVersions(c, r, n)
}
func (b resolvedSourceBlob) DeleteRunArtifactsForFormat(c context.Context, _ int, r string) error {
	return b.DeleteRun(c, r)
}
func (b resolvedSourceBlob) DeleteRunArtifacts(c context.Context, r string) error {
	return b.DeleteRun(c, r)
}
func (b resolvedSourceBlob) RunObjectsForFormat(int) (blob.RunObjects, error) { return b, nil }
func (b resolvedSourceBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return b, nil
}
func (b resolvedSourceBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return b, nil
}

func resolvedMongoFixture(t *testing.T) (*mongostore.Store, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires local Mongo replica-set fixture")
	}
	ctx := store.WithIdentity(t.Context(), "stream-tenant", "reader")
	st, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("stream_resolved_%d", time.Now().UnixNano()), Blob: resolvedSourceBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = st.DB().Drop(c)
		_ = st.Close(c)
	})
	return st, ctx
}
func resolvedBirth(t *testing.T, st *mongostore.Store, ctx context.Context, format store.ValueFormat, protocol store.RunDataProtocol) (context.Context, *store.Run) {
	t.Helper()
	r, err := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: format, DataProtocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunValueFormat(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: format, Protocol: protocol, BirthID: r.BirthID})
	if err != nil {
		t.Fatal(err)
	}
	bound, err = store.WithRunDataBinding(bound, binding)
	if err != nil {
		t.Fatal(err)
	}
	return bound, r
}
func takeResolvedEvent(t *testing.T, sub EventSubscription) *store.Event {
	t.Helper()
	select {
	case rows, ok := <-sub.Events():
		if !ok || len(rows) != 1 {
			t.Fatalf("event rows %+v open%v", rows, ok)
		}
		return rows[0]
	case err := <-sub.Errors():
		t.Fatalf("event error %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("event timeout")
	}
	return nil
}
func takeResolvedLog(t *testing.T, sub LogSubscription) LogChunk {
	t.Helper()
	select {
	case row, ok := <-sub.Chunks():
		if !ok {
			t.Fatal("log closed")
		}
		return row
	case err := <-sub.Errors():
		t.Fatalf("log error %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("log timeout")
	}
	return LogChunk{}
}

func TestMongoResolvedReplayTailAndSourceClose(t *testing.T) {
	t.Parallel()
	for _, protocol := range []store.RunDataProtocol{0, 1} {
		for _, format := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", protocol, format), func(t *testing.T) {
				if protocol == store.IncarnationRunData {
					if err := store.RequireRunDataStorage(protocol); err != nil {
						if !errors.Is(err, store.ErrRunDataCapability) {
							t.Fatal(err)
						}
						t.Skip("incarnation admission OFF")
					}
				}
				st, ctx := resolvedMongoFixture(t)
				bound, r := resolvedBirth(t, st, ctx, format, protocol)
				data := map[string]any{"marker": "seed"}
				if format == 2 {
					data["huge"] = json.Number("1e400")
				}
				if _, err := st.AppendEvent(bound, r.ID, store.Event{Data: data}); err != nil {
					t.Fatal(err)
				}
				if err := st.AppendRunLog(bound, r.ID, 0, []byte("seed")); err != nil {
					t.Fatal(err)
				}
				var resolutions atomic.Int64
				src := NewMongoResolved(func(c context.Context, id string) (MongoSelection, error) {
					resolutions.Add(1)
					return st.ResolveStreamSelection(c, id)
				}, quietLogger())
				defer src.Close()
				// Subscribe unbound: initial selection must capture the authorized metadata.
				events, err := src.SubscribeEvents(ctx, r.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer events.Close()
				logs, err := src.SubscribeLogs(ctx, r.ID, 1)
				if err != nil {
					t.Fatal(err)
				}
				defer logs.Close()
				e := takeResolvedEvent(t, events)
				if e.Data["marker"] != "seed" {
					t.Fatal(e)
				}
				if format == 2 && e.Data["huge"] != json.Number("1e400") {
					t.Fatal("exact replay lost", e.Data)
				}
				l := takeResolvedLog(t, logs)
				if l.Offset != 1 || string(l.Data) != "eed" {
					t.Fatal(l)
				}
				data["marker"] = "live"
				if _, err = st.AppendEvent(bound, r.ID, store.Event{Data: data}); err != nil {
					t.Fatal(err)
				}
				if err = st.AppendRunLog(bound, r.ID, 4, []byte("tail")); err != nil {
					t.Fatal(err)
				}
				e = takeResolvedEvent(t, events)
				if e.Data["marker"] != "live" {
					t.Fatal(e)
				}
				if format == 2 && e.Data["huge"] != json.Number("1e400") {
					t.Fatal("exact live lost", e.Data)
				}
				l = takeResolvedLog(t, logs)
				if l.Offset != 4 || string(l.Data) != "tail" {
					t.Fatal(l)
				}
				closed := make(chan struct{})
				go func() { _ = src.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("Source.Close did not join")
				}
				if _, err = src.SubscribeEvents(ctx, r.ID, 0); !errors.Is(err, errMongoSourceClosed) {
					t.Fatalf("subscribe after close %v", err)
				}
				if _, err = st.LoadRun(bound, r.ID); err != nil {
					t.Fatal("source closed borrowed store", err)
				}
				if resolutions.Load() != 2 {
					t.Fatalf("selection count %d", resolutions.Load())
				}
			})
		}
	}
}

func TestMongoResolvedPureLifetimeAndErrors(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	src := NewMongoResolved(func(ctx context.Context, _ string) (MongoSelection, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	result := make(chan error, 1)
	go func() { _, err := src.SubscribeEvents(t.Context(), "r", 0); result <- err }()
	<-entered
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	p := NewEventPipe()
	for i := 0; i < 4; i++ {
		p.Warn(errors.New("transient"))
	}
	fatal := errors.Join(context.Canceled, store.ErrRunDataIdentityChanged)
	if !mongoPermanent(fatal) {
		t.Fatal("joined refusal not permanent")
	}
	p.Fail(fatal)
	p.Finish(nil)
	found := false
	for err := range p.Errors() {
		found = found || errors.Is(err, store.ErrRunDataIdentityChanged)
	}
	if !found {
		t.Fatal("fatal dropped")
	}
	if _, err := mongoLogEnd(math.MaxInt64-1, []byte("1234")); !errors.Is(err, store.ErrEventsCorrupted) {
		t.Fatal(err)
	}
	if end, err := mongoLogEnd(math.MaxInt64-4, []byte("1234")); err != nil || end != math.MaxInt64 {
		t.Fatal(end, err)
	}
	if mongoContextErrorOnly(errors.Join(context.Canceled, errors.New("IO"))) {
		t.Fatal("joined IO treated as cancel")
	}
	for _, protocol := range []store.RunDataProtocol{0, 1} {
		for _, format := range []store.ValueFormat{1, 2} {
			ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "r", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: format, Protocol: protocol, BirthID: store.NewRunBirthID()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err = store.WithRunDataBinding(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			legacy := NewMongo(nil, nil, nil, nil)
			if _, err = legacy.SubscribeEvents(ctx, "r", 0); !errors.Is(err, store.ErrRunDataCapability) {
				t.Fatalf("legacy retained binding %v", err)
			}
			legacy.Close()
		}
	}
	formatOnly, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "r", Format: 2})
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewMongo(nil, nil, nil, nil)
	if _, err = legacy.SubscribeLogs(formatOnly, "r", 0); !errors.Is(err, store.ErrValueFormatCapability) {
		t.Fatal(err)
	}
	legacy.Close()
}

// A real retained Store selection must terminate both real Watch producers when
// its strict parent disappears, without replacing the observation on retry.
func TestMongoResolvedMissingOriginalStops(t *testing.T) {
	t.Parallel()
	if err := store.RequireRunDataStorage(store.IncarnationRunData); err != nil {
		if !errors.Is(err, store.ErrRunDataCapability) {
			t.Fatal(err)
		}
		t.Skip("incarnation admission OFF")
	}
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			st, ctx := resolvedMongoFixture(t)
			_, r := resolvedBirth(t, st, ctx, format, store.IncarnationRunData)
			selected, err := st.ResolveStreamSelection(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.DB().Collection("runs").DeleteOne(ctx, bson.M{"_id": r.ID}); err != nil {
				t.Fatal(err)
			}
			src := NewMongoResolved(func(context.Context, string) (MongoSelection, error) { return selected, nil }, quietLogger())
			defer src.Close()
			events, err := src.SubscribeEvents(ctx, r.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			logs, err := src.SubscribeLogs(ctx, r.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			select {
			case _, ok := <-events.Events():
				if ok {
					t.Fatal("unexpected event")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing original events retried")
			}
			select {
			case _, ok := <-logs.Chunks():
				if ok {
					t.Fatal("unexpected log")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing original logs retried")
			}
			for _, errs := range []<-chan error{events.Errors(), logs.Errors()} {
				found := false
				for err := range errs {
					found = found || errors.Is(err, store.ErrRunNotFound)
				}
				if !found {
					t.Fatal("missing original refusal lost")
				}
			}
		})
	}
}
