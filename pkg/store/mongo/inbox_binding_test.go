package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestInboxAbsentHeaderRetainsBinding(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	if e := s.AppendQueuedMessage(ctx, "orphan", store.QueuedUserMessage{ID: "orphan-msg", Text: "orphan"}); e != nil {
		t.Fatal(e)
	}
	bind := func(id, birth string) context.Context {
		b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: id, Format: store.LegacyValueFormat, BirthID: birth})
		if e != nil {
			t.Fatal(e)
		}
		c, e := store.WithRunDataBinding(ctx, b)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	for name, read := range map[string]func(context.Context, string) ([]store.QueuedUserMessage, error){"pending": s.LoadPendingQueuedMessages, "list": s.ListQueuedMessages} {
		t.Run(name, func(t *testing.T) {
			if rows, e := read(ctx, "orphan"); e != nil || len(rows) != 1 {
				t.Fatalf("legitimate unbound orphan refused: %v %v", rows, e)
			}
			if rows, e := read(bind("orphan", ""), "orphan"); e != nil || len(rows) != 1 {
				t.Fatalf("observed legacy absence orphan compatibility refused: %v %v", rows, e)
			}
			for cname, c := range map[string]context.Context{"foreign-absence": bind("foreign", ""), "foreign": bind("foreign", "birth"), "stale": bind("orphan", "retired-birth")} {
				t.Run(cname, func(t *testing.T) {
					rows, e := read(c, "orphan")
					if len(rows) != 0 || !errors.Is(e, store.ErrRunDataIdentityChanged) {
						t.Fatalf("bound call adopted orphan payload: rows=%v err=%v", rows, e)
					}
				})
			}
		})
	}
}
