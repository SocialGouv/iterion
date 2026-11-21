package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestObserveRunDataHeaderFSDoesNotDecodeOrRewritePayload(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "opaque", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"id":"opaque","birth_id":%q,"format_version":1,"data_protocol":1,"inputs":{"n":1e400}}`, r.BirthID))
	if err := os.WriteFile(s.runJSONPath(r.ID), raw, filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(t.Context(), r.ID); err == nil {
		t.Fatal("fixture must fail payload decoding")
	}
	h, err := ObserveRunDataHeader(t.Context(), s, r.ID)
	if err != nil || h.RunID != r.ID || h.BirthID != r.BirthID || h.Protocol != IncarnationRunData {
		t.Fatalf("header=%+v err=%v", h, err)
	}
	after, err := os.ReadFile(s.runJSONPath(r.ID))
	if err != nil || string(after) != string(raw) {
		t.Fatalf("observation rewrote run: %v", err)
	}
}

func TestObserveRunDataHeaderFSRetainsOriginalBirth(t *testing.T) {
	ctx := t.Context()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "run", "wf", nil); err != nil {
		t.Fatal(err)
	}
	h, err := ObserveRunDataHeader(ctx, s, "run")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := WithRunDataBinding(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune %d: %v", n, err)
	}
	successor, err := s.CreateRun(ctx, "run", "replacement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if successor.BirthID == h.BirthID {
		t.Fatal("fixture reused identity")
	}
	if _, err := ObserveRunDataHeader(bound, s, "run"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("adopted successor: %v", err)
	}
	fresh, err := ObserveRunDataHeader(ctx, s, "run")
	if err != nil || fresh.BirthID != successor.BirthID {
		t.Fatalf("fresh observation: %+v %v", fresh, err)
	}
}

func TestObserveRunDataHeaderFSRefusesForeignOrMissingIdentity(t *testing.T) {
	ctx := t.Context()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveRunDataHeader(ctx, s, "absent"); !errors.Is(err, ErrRunNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.runDir("absent")); !os.IsNotExist(err) {
		t.Fatalf("missing observation created a run shell: %v", err)
	}
	if _, err := s.CreateRun(ctx, "run", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.runJSONPath("run"), []byte(`{"id":"foreign"}`), filePerm); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveRunDataHeader(ctx, s, "run"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("accepted foreign row: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ObserveRunDataHeader(cancelled, s, "run"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type payloadOnlyHeaderStore struct {
	RunStore
	read bool
}

func (s *payloadOnlyHeaderStore) LoadRun(context.Context, string) (*Run, error) {
	s.read = true
	return &Run{ID: "run"}, nil
}
func TestObserveRunDataHeaderDoesNotFallBackToPayload(t *testing.T) {
	s := &payloadOnlyHeaderStore{}
	if _, err := ObserveRunDataHeader(t.Context(), s, "run"); !errors.Is(err, ErrRunDataCapability) {
		t.Fatal(err)
	}
	if s.read {
		t.Fatal("metadata observation decoded a payload through fallback")
	}
}
