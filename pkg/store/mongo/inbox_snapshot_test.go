package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/event"
	"sync"
	"testing"
	"time"
)

func TestInboxReadersRetainObservedBirth(t *testing.T) {
	for _, format := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			r, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "inbox", WorkflowName: "wf", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			bind := func(id, birth string) context.Context {
				t.Helper()
				b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: id, Format: format, Protocol: r.DataProtocol, BirthID: birth})
				if e != nil {
					t.Fatal(e)
				}
				c, e := store.WithRunDataBinding(ctx, b)
				if e != nil {
					t.Fatal(e)
				}
				return c
			}
			current := bind(r.ID, r.BirthID)
			if err = s.AppendQueuedMessage(current, r.ID, store.QueuedUserMessage{ID: "original", Text: "secret for current birth"}); err != nil {
				t.Fatal(err)
			}
			readers := map[string]func(context.Context, string) ([]store.QueuedUserMessage, error){"pending": s.LoadPendingQueuedMessages, "list": s.ListQueuedMessages}
			cancelled, cancel := context.WithCancel(current)
			cancel()
			for name, read := range readers {
				t.Run(name, func(t *testing.T) {
					for _, tc := range []struct {
						name string
						ctx  context.Context
						want error
					}{{"stale", bind(r.ID, r.BirthID+"-retired"), store.ErrRunDataIdentityChanged}, {"foreign", bind("other", r.BirthID), store.ErrRunDataIdentityChanged}, {"cancelled", cancelled, context.Canceled}} {
						t.Run(tc.name, func(t *testing.T) {
							rows, e := read(tc.ctx, r.ID)
							if !errors.Is(e, tc.want) || len(rows) != 0 {
								t.Fatalf("want no payload and %v; rows=%d err=%v", tc.want, len(rows), e)
							}
						})
					}
					rows, e := read(current, r.ID)
					if e != nil || len(rows) != 1 || rows[0].ID != "original" {
						t.Fatalf("current reader denied: %v %v", rows, e)
					}
				})
			}
		})
	}
}
func TestInboxReadCannotAdoptReplacementBetweenHeaderAndPayload(t *testing.T) {
	for _, kind := range []string{"pending", "list"} {
		t.Run(kind, func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			r, e := s.CreateRun(ctx, "race-inbox", "wf", nil)
			if e != nil {
				t.Fatal(e)
			}
			b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: store.LegacyValueFormat, BirthID: r.BirthID})
			if e != nil {
				t.Fatal(e)
			}
			bound, e := store.WithRunDataBinding(ctx, b)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.AppendQueuedMessage(bound, r.ID, store.QueuedUserMessage{ID: "original", Text: "original"}); e != nil {
				t.Fatal(e)
			}
			held, release := make(chan struct{}), make(chan struct{})
			var once, unblock sync.Once
			defer unblock.Do(func() { close(release) })
			reader := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(c context.Context, e *event.CommandStartedEvent) {
				if e.CommandName == "find" && e.Command.Lookup("find").StringValue() == colUserMessages {
					once.Do(func() {
						close(held)
						select {
						case <-release:
						case <-c.Done():
						}
					})
				}
			}})
			reader.userMessages = reader.db.Collection(colUserMessages)
			type outcome struct {
				rows []store.QueuedUserMessage
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				var rows []store.QueuedUserMessage
				var err error
				if kind == "pending" {
					rows, err = reader.LoadPendingQueuedMessages(bound, r.ID)
				} else {
					rows, err = reader.ListQueuedMessages(bound, r.ID)
				}
				done <- outcome{rows, err}
			}()
			select {
			case <-held:
			case got := <-done:
				t.Fatalf("did not reach payload barrier: %v", got.err)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			if e = s.DeleteRun(ctx, r.ID); e != nil {
				t.Fatal(e)
			}
			if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			next, e := s.CreateRun(ctx, r.ID, "replacement", nil)
			if e != nil {
				t.Fatal(e)
			}
			if next.BirthID == r.BirthID {
				t.Fatal("birth did not rotate")
			}
			if e = s.AppendQueuedMessage(ctx, r.ID, store.QueuedUserMessage{ID: "successor", Text: "successor"}); e != nil {
				t.Fatal(e)
			}
			unblock.Do(func() { close(release) })
			got := <-done
			if got.err != nil || len(got.rows) != 1 || got.rows[0].ID != "original" {
				t.Fatalf("snapshot must retain original row: rows=%v err=%v", got.rows, got.err)
			}
			t.Log("snapshot retained original message across delete/prune/recreate")
		})
	}
}
