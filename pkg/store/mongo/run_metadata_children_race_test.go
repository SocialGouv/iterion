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
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
)

func TestRunMetadataChildrenTransactionRetry(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"tag", "turn"} {
		for _, mode := range []string{"changed", "future", "same-format"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, base := valueFormatMongo(t, true)
				ctx, cancel := context.WithTimeout(base, 20*time.Second)
				defer cancel()
				if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				var armed atomic.Bool
				armed.Store(true)
				writer := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
					if e.CommandName == "update" && e.Command.Lookup("update").StringValue() == colRuns && armed.CompareAndSwap(true, false) {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				}})
				writer.runTags = writer.db.Collection(colRunTags)
				writer.runTurns = writer.db.Collection(colRunTurns)
				done := make(chan error, 1)
				go func() {
					if kind == "tag" {
						done <- writer.SetRunTags(ctx, "r", []string{"new"})
						return
					}
					done <- writer.WriteTurn(ctx, &store.TurnCheckpoint{RunID: "r", NodeID: "n", TextDigest: "new"})
				}()
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("writer missed transaction barrier: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var want error
				var err error
				switch mode {
				case "changed":
					_, err = s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 1}})
					want = store.ErrValueFormatChanged
				case "future":
					_, err = s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}})
					want = store.ErrUnsupportedValueFormat
				case "same-format":
					err = s.SetRunnerVersion(ctx, "r", "concurrent")
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
				if err != nil {
					t.Fatal(err)
				}
				unblock()
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				after, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
				if e != nil {
					t.Fatal(e)
				}
				col := s.runTags
				if kind == "turn" {
					col = s.runTurns
				}
				n, e := col.CountDocuments(ctx, bson.M{"run_id": "r"})
				if e != nil {
					t.Fatal(e)
				}
				if want != nil {
					if !errors.Is(err, want) || !bytes.Equal(before, after) || n != 0 {
						t.Fatalf("err=%v want=%v changed=%t children=%d", err, want, !bytes.Equal(before, after), n)
					}
				} else if err != nil || n != 1 {
					t.Fatal(err, n)
				}
			})
		}
	}
}
