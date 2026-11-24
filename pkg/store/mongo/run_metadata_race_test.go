package mongo

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
)

func TestRunMetadataPublicWireGuard(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"metadata", "acquisition", "receipt"} {
		for _, mode := range []string{"changed", "future", "malformed", "same-format"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, base := valueFormatMongo(t, true)
				ctx, cancel := context.WithTimeout(base, 20*time.Second)
				defer cancel()
				r, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2, Status: store.RunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				expected := mongoExecutionExpected(r)
				reservation := store.OutputCorrectionReservation{ID: "paid", ExecutionEpoch: r.ExecutionEpoch, ScopeKey: r.ID, StartedAt: time.Now()}
				if kind == "receipt" {
					if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, store.RunExecutionAuthority{RunID: r.ID, Epoch: r.ExecutionEpoch}, reservation); e != nil || !ok {
						t.Fatal(ok, e)
					}
				}
				started, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				var armed atomic.Bool
				armed.Store(true)
				writer := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
					if (e.CommandName == "update" || e.CommandName == "findAndModify") && armed.CompareAndSwap(true, false) {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				}})
				done := make(chan error, 1)
				go func() {
					var e error
					switch kind {
					case "metadata":
						e = writer.SetRunnerVersion(ctx, r.ID, "writer")
					case "acquisition":
						var ok bool
						_, ok, e = writer.AcquireRunExecutionIf(ctx, r.ID, expected, store.ExecutionStart)
						if e == nil && !ok {
							e = errors.New("positive acquisition lost")
						}
					case "receipt":
						var ok bool
						ok, e = writer.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(reservation), storetest.UsageV2Receipt(t, "wire", storetest.UsageV2Delegate(3, "test", 1, 1, true)))
						if e == nil && !ok {
							e = errors.New("positive receipt lost")
						}
					}
					done <- e
				}()
				select {
				case <-started:
				case e := <-done:
					t.Fatalf("no wire barrier: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var want error
				switch mode {
				case "changed":
					_, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"format_version": 1}})
					want = store.ErrValueFormatChanged
				case "future":
					_, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"format_version": 99}})
					want = store.ErrUnsupportedValueFormat
				case "malformed":
					_, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"format_version": bson.A{2}}})
					want = store.ErrInvalidValueFormat
				case "same-format":
					// Acquisition keeps its expected version: a pure same-format field update
					// proves the format guard does not invent an epoch/version requirement.
					if kind == "acquisition" {
						_, err = s.runs.UpdateOne(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"name": "concurrent"}})
					} else {
						err = s.SetRunLLMIdle(ctx, r.ID, nil)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
				if err != nil {
					t.Fatal(err)
				}
				unblock()
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				after, e := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
				if e != nil {
					t.Fatal(e)
				}
				if want != nil {
					if !errors.Is(err, want) || !bytes.Equal(before, after) {
						t.Fatalf("wire guard err=%v want=%v changed=%t", err, want, !bytes.Equal(before, after))
					}
				} else if err != nil || bytes.Equal(before, after) {
					t.Fatalf("same-format control err=%v changed=%t", err, !bytes.Equal(before, after))
				}
			})
		}
	}
}
