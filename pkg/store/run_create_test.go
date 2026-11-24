package store

import (
	"context"
	"errors"
	"testing"
)

type legacyBirthStore struct {
	RunStore
	calls int
}

func (s *legacyBirthStore) CreateRun(_ context.Context, id, w string, in map[string]any) (*Run, error) {
	s.calls++
	return &Run{ID: id, WorkflowName: w, Inputs: in, FormatVersion: 1}, nil
}

type formattedBirthStore struct {
	RunStore
	seen []RunCreateSpec
}

func (s *formattedBirthStore) CreateRunWithValueFormat(_ context.Context, spec RunCreateSpec) (*Run, error) {
	s.seen = append(s.seen, spec)
	return &Run{ID: spec.ID, ParentRunID: spec.ParentRunID, FormatVersion: int(spec.Format)}, nil
}

type birthUnwrapper struct {
	RunStore
	inner RunStore
}

func (s birthUnwrapper) Unwrap() RunStore { return s.inner }
func TestRunCreateCapabilityAndLegacyFallback(t *testing.T) {
	t.Parallel()
	legacy := &legacyBirthStore{}
	if _, e := CreateRunWithValueFormat(t.Context(), legacy, RunCreateSpec{ID: "r"}); e != nil || legacy.calls != 1 {
		t.Fatal(legacy.calls, e)
	}
	for _, spec := range []RunCreateSpec{{ID: "r", Format: 2}, {ID: "r", Format: 99}, {ID: "r", ParentRunID: "p"}, {ID: "r", Status: RunStatusQueued}, {ID: "r", FilePath: "main.bot"}} {
		if _, e := CreateRunWithValueFormat(t.Context(), legacy, spec); e == nil {
			t.Fatal(spec)
		}
	}
	if legacy.calls != 1 {
		t.Fatal("refused birth invoked legacy writer")
	}
	capable := &formattedBirthStore{}
	wrapped := birthUnwrapper{inner: capable}
	spec := RunCreateSpec{ID: "child", ParentRunID: "p", Format: 2, Status: RunStatusQueued, FilePath: "main.bot", BotID: "b"}
	r, e := CreateRunWithValueFormat(t.Context(), wrapped, spec)
	if e != nil || r.ParentRunID != "p" || len(capable.seen) != 1 || capable.seen[0].FilePath != "main.bot" {
		t.Fatal(r, capable.seen, e)
	}
}
func TestRunCreateValidatesBeforeCapability(t *testing.T) {
	t.Parallel()
	s := &formattedBirthStore{}
	for _, spec := range []RunCreateSpec{{Format: 2}, {ID: "r", Format: 99}, {ID: "r", Format: 1, Status: RunStatusFinished}} {
		if _, e := CreateRunWithValueFormat(t.Context(), s, spec); e == nil {
			t.Fatal(spec)
		}
	}
	ctx, e := WithRunValueFormat(t.Context(), RunValueFormat{"parent", 2})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = CreateRunWithValueFormat(ctx, s, RunCreateSpec{ID: "child", Format: 2}); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
	if len(s.seen) != 0 {
		t.Fatal("invalid request reached creator")
	}
}

// Protocol selection cannot fall through either creator capability or the
// historical fallback while the backend payload migration is incomplete.
func TestRunCreateDataProtocolAdmissionStaysClosed(t *testing.T) {
	t.Parallel()
	legacy, capable := &legacyBirthStore{}, &formattedBirthStore{}
	for _, backend := range []RunStore{legacy, capable} {
		for _, protocol := range []RunDataProtocol{IncarnationRunData, -1, 2} {
			want := ErrUnsupportedRunDataProtocol
			if protocol == IncarnationRunData {
				want = ErrRunDataCapability
			}
			_, err := CreateRunWithValueFormat(t.Context(), backend, RunCreateSpec{ID: "r", DataProtocol: protocol})
			if !errors.Is(err, want) {
				t.Fatalf("protocol %d: %v", protocol, err)
			}
		}
	}
	if legacy.calls != 0 || len(capable.seen) != 0 {
		t.Fatal("refused protocol reached a creator")
	}
}

func TestFilesystemDataProtocolBirthRefusesBeforePublication(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := RunCreateSpec{ID: "r", DataProtocol: IncarnationRunData}
	if _, err = s.CreateRunWithValueFormat(t.Context(), spec); !errors.Is(err, ErrRunDataCapability) {
		t.Fatal(err)
	}
	ctx, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: spec.ID, Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := s.BeginRunBirth(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reservation.Unlock() }()
	if _, err = reservation.Publish(ctx, spec); !errors.Is(err, ErrRunDataCapability) {
		t.Fatal(err)
	}
	if _, err = s.LoadRun(t.Context(), spec.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("refusal published a shell: %v", err)
	}
}
