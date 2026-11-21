//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestRunPRFilesystemUnreadableCandidate(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can bypass the real EACCES witness")
	}
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, url := range []string{"pr", "other"} {
			t.Run(fmt.Sprintf("%d/%s", format, url), func(t *testing.T) {
				t.Parallel()
				s, err := New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"a", "z"} {
					input := url
					if id == "a" {
						input = "pr"
					}
					if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: id, Format: format, Inputs: map[string]any{"pr_url": input}}); err != nil {
						t.Fatal(err)
					}
				}
				want := []string{"a"}
				if url == "pr" {
					want = append(want, "z")
				}
				if ids, err := s.ListRunsForPR(t.Context(), "pr"); err != nil || !reflect.DeepEqual(ids, want) {
					t.Fatal("readable control", ids, err)
				}
				dir := s.runDir("z")
				if err = os.Chmod(dir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0700); err != nil {
						t.Error(err)
					}
				})
				if _, err = os.Stat(s.runJSONPath("z")); !errors.Is(err, os.ErrPermission) {
					t.Fatalf("fixture must cause EACCES: %v", err)
				}
				if ids, err := s.ListRunsForPR(t.Context(), "pr"); !errors.Is(err, os.ErrPermission) || ids != nil {
					t.Fatalf("unreadable candidate silently omitted: ids=%v err=%v", ids, err)
				}
				if _, err := s.ListRunsBySourceIssue(t.Context(), "unrelated"); err != nil {
					t.Fatal("best-effort view changed", err)
				}
			})
		}
	}
}

func TestRunPRFilesystemCancellationBoundaries(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		s, err := New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if ids, err := s.ListRunsForPR(t.Context(), "pr"); err != nil || len(ids) != 0 {
			t.Fatal("empty live control", ids, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if ids, err := s.ListRunsForPR(ctx, "pr"); !errors.Is(err, context.Canceled) || ids != nil {
			t.Fatalf("cancelled empty scan reported success: ids=%v err=%v", ids, err)
		}
	})
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, cancelDuringRead := range []bool{false, true} {
			t.Run(fmt.Sprintf("last-read/%d/cancel-%t", format, cancelDuringRead), func(t *testing.T) {
				t.Parallel()
				s, err := New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: format, Inputs: map[string]any{"pr_url": "pr"}}); err != nil {
					t.Fatal(err)
				}
				p := s.runJSONPath("r")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err = syscall.Mkfifo(p, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type result struct {
					ids []string
					err error
				}
				done := make(chan result, 1)
				go func() { ids, e := s.ListRunsForPR(ctx, "pr"); done <- result{ids, e} }()
				// Opening the real FIFO writer proves the final os.ReadFile has entered;
				// cancellation is sequenced by this barrier, not a scheduling delay.
				writer, err := os.OpenFile(p, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if cancelDuringRead {
					cancel()
				}
				if _, err = writer.Write(raw); err != nil {
					_ = writer.Close()
					t.Fatal(err)
				}
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
				got := <-done
				if cancelDuringRead {
					if !errors.Is(got.err, context.Canceled) || got.ids != nil {
						t.Fatalf("cancel during final read returned result: ids=%v err=%v", got.ids, got.err)
					}
				} else if got.err != nil || !reflect.DeepEqual(got.ids, []string{"r"}) {
					t.Fatal("FIFO live control", got.ids, got.err)
				}
			})
		}
	}
}

func TestRunPRFilesystemOrphanAndOrder(t *testing.T) {
	t.Parallel()
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			t.Parallel()
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"z", "a"} {
				r, err := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: id, Format: format, Inputs: map[string]any{"pr_url": "pr"}})
				if err != nil {
					t.Fatal(err)
				}
				r.CreatedAt = time.Unix(1, 0)
				if err = s.SaveRun(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.MkdirAll(filepath.Join(s.root, "runs", "orphan"), 0700); err != nil {
				t.Fatal(err)
			}
			if ids, err := s.ListRunsForPR(t.Context(), "pr"); err != nil || !reflect.DeepEqual(ids, []string{"a", "z"}) {
				t.Fatalf("orphan/order control: ids=%v err=%v", ids, err)
			}
		})
	}
}
