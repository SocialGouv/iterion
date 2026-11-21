package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type fatalEventSubscription struct {
	events chan []*store.Event
	errs   chan error
	closes int
}

func (s *fatalEventSubscription) Events() <-chan []*store.Event { return s.events }
func (s *fatalEventSubscription) Errors() <-chan error          { return s.errs }
func (s *fatalEventSubscription) Close() error                  { s.closes++; return nil }

type fatalLogSubscription struct {
	chunks chan runstream.LogChunk
	errs   chan error
	closes int
}

func (s *fatalLogSubscription) Chunks() <-chan runstream.LogChunk { return s.chunks }
func (s *fatalLogSubscription) Errors() <-chan error              { return s.errs }
func (s *fatalLogSubscription) Close() error                      { s.closes++; return nil }

func TestRunStreamCompletionWire(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			errs []error
			code string
		}{
			{"identity", []error{store.ErrRunDataIdentityChanged}, "failed"},
			{"missing", []error{store.ErrRunNotFound}, "failed"},
			{"joined", []error{errors.Join(context.Canceled, store.ErrValueFormatChanged)}, "failed"},
			{"corruption", []error{store.ErrEventsCorrupted}, "corruption"},
			{"warning", []error{errors.New("retry")}, ""},
			{"cancel", []error{context.Canceled}, ""},
			{"nil", []error{nil}, ""},
		} {
			t.Run(fmt.Sprintf("logs%v/%s", logs, tc.name), func(t *testing.T) {
				c := newRunConn(&Server{}, nil, "r")
				errs := make(chan error, len(tc.errs))
				for _, e := range tc.errs {
					errs <- e
				}
				close(errs)
				if logs {
					s := &fatalLogSubscription{chunks: make(chan runstream.LogChunk, 1), errs: errs}
					s.chunks <- runstream.LogChunk{Offset: 7, Data: []byte("bytes")}
					close(s.chunks)
					c.pumpLogs(s)
					if s.closes != 1 {
						t.Fatal(s.closes)
					}
				} else {
					s := &fatalEventSubscription{events: make(chan []*store.Event, 1), errs: errs}
					s.events <- []*store.Event{{RunID: "r", Seq: 7}}
					close(s.events)
					c.pumpEvents(s)
					if s.closes != 1 {
						t.Fatal(s.closes)
					}
				}
				if len(c.sendCh) != 2 {
					t.Fatalf("want data and one completion, got %d", len(c.sendCh))
				}
				var data, completion runWSEnvelope
				if e := json.Unmarshal(<-c.sendCh, &data); e != nil {
					t.Fatal(e)
				}
				if e := json.Unmarshal(<-c.sendCh, &completion); e != nil {
					t.Fatal(e)
				}
				wantData, wantEnd := wsTypeEvent, wsTypeTerminated
				if logs {
					wantData, wantEnd = wsTypeLogChunk, wsTypeLogTerminated
				}
				if data.Type != wantData {
					t.Fatal(data.Type)
				}
				if tc.code == "" {
					if completion.Type != wantEnd {
						t.Fatal(completion.Type)
					}
					return
				}
				if completion.Type != wsTypeError {
					t.Fatalf("permanent rendered %s", completion.Type)
				}
				var payload wsErrorPayload
				if e := json.Unmarshal(completion.Payload, &payload); e != nil {
					t.Fatal(e)
				}
				wantCode := "event_stream_failed"
				if logs {
					wantCode = "log_stream_failed"
				} else if tc.code == "corruption" {
					wantCode = "events_corrupted"
				}
				if payload.Code != wantCode || payload.Message == "" || completion.AckID != "" {
					t.Fatalf("bad error %+v %+v", completion, payload)
				}
			})
		}
	}
}
