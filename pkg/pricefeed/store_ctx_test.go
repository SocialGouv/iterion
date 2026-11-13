package pricefeed

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The memory twins read under the caller's ctx as a database read does: an
// expired or cancelled ctx answers its error, never the document — the
// fold's per-level deadline is exercised by the twin as it is by Mongo. A
// live ctx answers the document.
func TestMemoryStoresAnswerAnExpiredReadWithItsError(t *testing.T) {
	configs, states := NewMemoryConfigStore(), NewMemoryStateStore()
	if err := configs.Put(context.Background(), PlatformConfigDocID, &ConfigRecord{Feeds: []FeedConfig{{Name: "f", Type: TypeLiteLLM, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := states.Put(context.Background(), StateDocID(PlatformConfigDocID, "f"), &StateRecord{Feed: "f", Status: StateOK}); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"expired", expired, context.DeadlineExceeded},
		{"cancelled", cancelled, context.Canceled},
	} {
		if rec, err := configs.Get(tc.ctx, PlatformConfigDocID); !errors.Is(err, tc.want) || rec != nil {
			t.Errorf("%s: config Get = %v, %v; want %v and no document", tc.name, rec, err, tc.want)
		}
		if rec, err := states.Get(tc.ctx, StateDocID(PlatformConfigDocID, "f")); !errors.Is(err, tc.want) || rec != nil {
			t.Errorf("%s: state Get = %v, %v; want %v and no document", tc.name, rec, err, tc.want)
		}
	}
	if rec, err := configs.Get(context.Background(), PlatformConfigDocID); err != nil || rec == nil {
		t.Errorf("live ctx: config Get = %v, %v; want the document", rec, err)
	}
	if rec, err := states.Get(context.Background(), StateDocID(PlatformConfigDocID, "f")); err != nil || rec == nil {
		t.Errorf("live ctx: state Get = %v, %v; want the document", rec, err)
	}
}
