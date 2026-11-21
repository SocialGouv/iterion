package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRunFilesDeletedBinding(t *testing.T) {
	t.Parallel()
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			t.Parallel()
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: format}); err != nil {
				t.Fatal(err)
			}
			wrong, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "other", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			matching, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ListRunFiles(wrong, "r"); !errors.Is(err, ErrValueFormatChanged) {
				t.Fatal("live wrong binding", err)
			}
			if _, err = s.ListRunFiles(matching, "r"); err != nil {
				t.Fatal("live matching binding", err)
			}
			if err = s.DeleteRun(t.Context(), "r"); err != nil {
				t.Fatal(err)
			}
			for _, ctx := range []context.Context{t.Context(), matching} {
				if rows, err := s.ListRunFiles(ctx, "r"); err != nil || len(rows) != 0 {
					t.Fatal("deleted compatibility", rows, err)
				}
			}
			if rows, err := s.ListRunFiles(wrong, "r"); !errors.Is(err, ErrValueFormatChanged) || rows != nil {
				t.Fatalf("wrong run binding softened after delete: rows=%v err=%v", rows, err)
			}
			cancelled, cancel := context.WithCancel(matching)
			cancel()
			if rows, err := s.ListRunFiles(cancelled, "r"); !errors.Is(err, context.Canceled) || rows != nil {
				t.Fatalf("cancelled deleted read: rows=%v err=%v", rows, err)
			}
		})
	}
}
