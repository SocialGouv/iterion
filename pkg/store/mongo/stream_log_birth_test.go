package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func TestMongoLogOriginalBirth(t *testing.T) {
	for _, protocol := range []store.RunDataProtocol{store.HistoricalRunData, store.IncarnationRunData} {
		for _, format := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
			t.Run(fmt.Sprintf("protocol%d-format%d", protocol, format), func(t *testing.T) {
				requireStreamProtocol(t, protocol)
				s, ctx := valueFormatMongo(t, true)
				s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
				create := func() (*store.Run, context.Context) {
					t.Helper()
					r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "same-log", WorkflowName: "wf", Format: format, DataProtocol: protocol})
					if e != nil {
						t.Fatal(e)
					}
					b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: format, BirthID: r.BirthID, Protocol: r.DataProtocol})
					if e != nil {
						t.Fatal(e)
					}
					c, e := store.WithRunDataBinding(ctx, b)
					if e != nil {
						t.Fatal(e)
					}
					return r, c
				}
				a, ca := create()
				if e := s.AppendRunLog(ca, a.ID, 0, []byte("original-A")); e != nil {
					t.Fatal(e)
				}
				if raw, e := s.ReadRunLogRange(ca, a.ID, 0, 0); e != nil || string(raw) != "original-A" {
					t.Fatalf("original baseline %q %v", raw, e)
				}
				if e := s.DeleteRun(ca, a.ID); e != nil {
					t.Fatal(e)
				}
				if n, e := s.PruneDeletionMarkers(ca, time.Now().Add(time.Hour)); e != nil || n != 1 {
					t.Fatalf("prune %d %v", n, e)
				}
				b, cb := create()
				if b.BirthID == a.BirthID {
					t.Fatal("birth reused")
				}
				const sentinel = "successor-B-sentinel"
				if e := s.AppendRunLog(cb, b.ID, 0, []byte(sentinel)); e != nil {
					t.Fatal(e)
				}
				if e := s.AppendRunLog(ca, b.ID, int64(len(sentinel)), []byte("late-A")); !errors.Is(e, store.ErrRunDataIdentityChanged) {
					t.Fatalf("stale append guard %v", e)
				}
				if raw, e := s.ReadRunLogRange(cb, b.ID, 0, 0); e != nil || string(raw) != sentinel {
					t.Fatalf("successor control %q %v", raw, e)
				}
				if protocol == store.HistoricalRunData {
					if raw, e := s.ReadRunLogRange(ctx, b.ID, 0, 0); e != nil || string(raw) != sentinel {
						t.Fatalf("legacy unbound control %q %v", raw, e)
					}
					return
				}
				raw, e := s.ReadRunLogRange(ca, b.ID, 0, 0)
				if !errors.Is(e, store.ErrRunDataIdentityChanged) || len(raw) != 0 {
					t.Errorf("stale range exposed successor: bytes=%q err=%v", raw, e)
				}
				size, e := s.RunLogSize(ca, b.ID)
				if !errors.Is(e, store.ErrRunDataIdentityChanged) || size != 0 {
					t.Errorf("stale size exposed successor: size=%d err=%v", size, e)
				}
			})
		}
	}
}
