package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type readCancellationProbe struct {
	cancel context.CancelFunc
	result *Run
	err    error
	calls  int
}

func (s *readCancellationProbe) LoadRun(context.Context, string) (*Run, error) {
	s.calls++
	s.cancel()
	return s.result, s.err
}
func TestLoadRunWithValueFormatPreservesReadErrorOnCancellation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"business", "wrapped-business", "format-refusal", "format-joined-cancellation", "successful-read"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			business := errors.New("store read failed independently of cancellation")
			original := business
			switch kind {
			case "wrapped-business":
				original = fmt.Errorf("read: %w", business)
			case "format-refusal":
				original = ErrValueFormatChanged
			case "format-joined-cancellation":
				original = errors.Join(ErrValueFormatChanged, context.Canceled)
			case "successful-read":
				original = nil
			}
			reader := &readCancellationProbe{cancel: cancel, err: original, result: &Run{ID: "run", FormatVersion: 1}}
			bound, run, got := LoadRunWithValueFormat(ctx, reader, "run")
			if reader.calls != 1 || bound != nil || run != nil {
				t.Fatalf("partial result: calls=%d ctx=%v run=%v", reader.calls, bound, run)
			}
			if original != nil {
				if got != original {
					t.Fatalf("READ_ERROR_MASKED: original=%v actual=%v", original, got)
				}
			} else if got != context.Canceled {
				t.Fatalf("late successful read cancellation lost: %v", got)
			}
		})
	}
}
