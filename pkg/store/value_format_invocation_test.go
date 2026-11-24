package store

import (
	"context"
	"errors"
	"testing"
)

func TestBindRunValueFormatPreservesOriginal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy, err := WithRunValueFormat(ctx, RunValueFormat{RunID: "run", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := WithRunValueFormat(ctx, RunValueFormat{RunID: "run", Format: ExactValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		run    *Run
		want   ValueFormat
		err    error
		retain bool
	}{
		{"first legacy zero", ctx, &Run{ID: "run"}, 1, nil, false},
		{"first exact", ctx, &Run{ID: "run", FormatVersion: 2}, 2, nil, false},
		{"bound legacy zero", legacy, &Run{ID: "run"}, 1, nil, true},
		{"bound legacy one", legacy, &Run{ID: "run", FormatVersion: 1}, 1, nil, true},
		{"bound exact metadata", exact, &Run{ID: "run", FormatVersion: 2, Name: "new metadata"}, 2, nil, true},
		{"legacy cannot adopt exact", legacy, &Run{ID: "run", FormatVersion: 2}, 0, ErrValueFormatChanged, false},
		{"exact cannot adopt legacy", exact, &Run{ID: "run", FormatVersion: 1}, 0, ErrValueFormatChanged, false},
		{"child is not parent", legacy, &Run{ID: "child", FormatVersion: 1}, 0, ErrValueFormatChanged, false},
		{"future", ctx, &Run{ID: "run", FormatVersion: 99}, 0, ErrUnsupportedValueFormat, false},
		{"nil run", ctx, nil, 0, ErrInvalidValueFormat, false},
		{"nil context", nil, &Run{ID: "run"}, 0, ErrInvalidValueFormat, false},
		{"canceled", canceled, &Run{ID: "run"}, 0, context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, e := BindRunValueFormat(tc.ctx, tc.run)
			if !errors.Is(e, tc.err) {
				t.Fatalf("bind error=%v want %v", e, tc.err)
			}
			if tc.err != nil {
				if got != nil {
					t.Fatal("refusal returned context")
				}
				return
			}
			if tc.retain && got != tc.ctx {
				t.Fatal("binding was replaced")
			}
			f, e := ValueFormatForWrite(got, tc.run.ID)
			if e != nil || f != tc.want {
				t.Fatalf("format=%d error=%v", f, e)
			}
		})
	}
}

type invocationLoadStore struct {
	RunStore
	load func(context.Context, string) (*Run, error)
}

func (s invocationLoadStore) LoadRun(ctx context.Context, id string) (*Run, error) {
	return s.load(ctx, id)
}

func TestLoadRunWithValueFormatRefusesBeforeAndAfterRead(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before", "during"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			if when == "before" {
				cancel()
			}
			s := invocationLoadStore{load: func(context.Context, string) (*Run, error) {
				calls++
				cancel()
				return &Run{ID: "run", FormatVersion: 1}, nil
			}}
			bound, r, e := LoadRunWithValueFormat(ctx, s, "run")
			if !errors.Is(e, context.Canceled) || bound != nil || r != nil {
				t.Fatalf("cancellation escaped: %v %v %v", bound, r, e)
			}
			want := 0
			if when == "during" {
				want = 1
			}
			if calls != want {
				t.Fatalf("load calls=%d want %d", calls, want)
			}
		})
	}
}

func TestLoadRunWithValueFormatKeepsErrorsAndIdentity(t *testing.T) {
	t.Parallel()
	infra := errors.New("store offline")
	for _, tc := range []struct {
		name    string
		run     *Run
		loadErr error
		want    error
	}{
		{"infra", nil, infra, infra}, {"not found", nil, ErrRunNotFound, ErrRunNotFound},
		{"deleted", nil, ErrRunDeleted, ErrRunDeleted}, {"wrong returned ID", &Run{ID: "another", FormatVersion: 1}, nil, ErrValueFormatChanged},
		{"future", &Run{ID: "run", FormatVersion: 99}, nil, ErrUnsupportedValueFormat},
		{"nil returned", nil, nil, ErrValueFormatChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := invocationLoadStore{load: func(context.Context, string) (*Run, error) { return tc.run, tc.loadErr }}
			ctx, r, e := LoadRunWithValueFormat(context.Background(), s, "run")
			if !errors.Is(e, tc.want) || ctx != nil || r != nil {
				t.Fatalf("result=%v %v %v want %v", ctx, r, e, tc.want)
			}
		})
	}
	t.Run("bound wrong ID before read", func(t *testing.T) {
		t.Parallel()
		ctx, _ := WithRunValueFormat(context.Background(), RunValueFormat{"parent", 1})
		s := invocationLoadStore{load: func(context.Context, string) (*Run, error) { t.Fatal("wrong-ID read reached store"); return nil, nil }}
		_, _, e := LoadRunWithValueFormat(ctx, s, "child")
		if !errors.Is(e, ErrValueFormatChanged) {
			t.Fatal(e)
		}
	})
}
