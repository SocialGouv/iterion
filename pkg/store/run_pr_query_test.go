package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunPRFilesystemStrictScan(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"future-match", "future-other", "null", "duplicate", "cancelled", "ordered"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx := t.Context()
			for _, id := range []string{"b", "a"} {
				url := "pr"
				if mode == "future-other" && id == "b" {
					url = "elsewhere"
				}
				r, e := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, Format: 2, Inputs: map[string]any{"pr_url": url}})
				if e != nil {
					t.Fatal(e)
				}
				r.CreatedAt = time.Unix(1, 0)
				if e = s.SaveRun(ctx, r); e != nil {
					t.Fatal(e)
				}
			}
			callctx := ctx
			var want error
			if mode == "cancelled" {
				var cancel context.CancelFunc
				callctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			} else if mode != "ordered" {
				body, e := os.ReadFile(s.runJSONPath("b"))
				if e != nil {
					t.Fatal(e)
				}
				replacement := `"format_version": 99`
				want = ErrUnsupportedValueFormat
				if mode == "null" {
					replacement = `"format_version": null`
					want = ErrInvalidValueFormat
				}
				if mode == "duplicate" {
					replacement = `"format_version": 2, "format_version": 2`
					want = ErrInvalidValueFormat
				}
				body = []byte(strings.Replace(string(body), `"format_version": 2`, replacement, 1))
				if e = os.WriteFile(s.runJSONPath("b"), body, 0600); e != nil {
					t.Fatal(e)
				}
			}
			ids, e := s.ListRunsForPR(callctx, "pr")
			if want != nil {
				if !errors.Is(e, want) || ids != nil {
					t.Fatal(ids, e, want)
				}
			} else if e != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
				t.Fatal(ids, e)
			}
			if mode == "future-other" {
				if _, e = s.ListRunsBySourceIssue(ctx, "not-present"); e != nil {
					t.Fatal("other view changed", e)
				}
			}
		})
	}
}
