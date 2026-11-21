package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func pidContextFixture(t *testing.T, format ValueFormat) (*FilesystemRunStore, context.Context) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := CreateRunWithValueFormat(t.Context(), s, RunCreateSpec{ID: "r", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := BindRunValueFormat(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WritePIDFileContext(ctx, "r", 17); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestContextPIDOperationsRetainOriginal(t *testing.T) {
	ops := map[string]func(ContextPIDStore, context.Context) error{
		"read": func(s ContextPIDStore, ctx context.Context) error {
			_, err := s.ReadPIDFileContext(ctx, "r")
			return err
		},
		"write":  func(s ContextPIDStore, ctx context.Context) error { return s.WritePIDFileContext(ctx, "r", 29) },
		"remove": func(s ContextPIDStore, ctx context.Context) error { return s.RemovePIDFileContext(ctx, "r") },
		"compare-remove": func(s ContextPIDStore, ctx context.Context) error {
			_, err := s.RemovePIDFileIfContext(ctx, "r", 17)
			return err
		},
	}
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		for _, scenario := range []string{"unbound", "wrong-id", "cancelled", "replacement", "future", "missing-parent"} {
			for name, op := range ops {
				t.Run(fmt.Sprintf("%d/%s/%s", format, scenario, name), func(t *testing.T) {
					s, ctx := pidContextFixture(t, format)
					var want error
					switch scenario {
					case "unbound":
						ctx, want = t.Context(), ErrValueFormatCapability
					case "wrong-id":
						var err error
						ctx, err = WithRunValueFormat(t.Context(), RunValueFormat{RunID: "other", Format: format})
						if err != nil {
							t.Fatal(err)
						}
						want = ErrValueFormatChanged
					case "cancelled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
						want = context.Canceled
					case "future":
						if err := os.WriteFile(s.runJSONPath("r"), []byte(`{"id":"r","format_version":99}`), 0600); err != nil {
							t.Fatal(err)
						}
						want = ErrUnsupportedValueFormat
					case "replacement", "missing-parent":
						if err := s.DeleteRun(t.Context(), "r"); err != nil {
							t.Fatal(err)
						}
						if _, err := s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
						if scenario == "replacement" {
							next := LegacyValueFormat
							if format == LegacyValueFormat {
								next = ExactValueFormat
							}
							r, err := CreateRunWithValueFormat(t.Context(), s, RunCreateSpec{ID: "r", Format: next})
							if err != nil {
								t.Fatal(err)
							}
							bound, err := BindRunValueFormat(t.Context(), r)
							if err != nil {
								t.Fatal(err)
							}
							if err := s.WritePIDFileContext(bound, "r", 17); err != nil {
								t.Fatal(err)
							}
							want = ErrValueFormatChanged
						} else {
							// The legacy wrapper may create an orphan sidecar. A bound
							// invocation must not read, alter or remove that sidecar.
							if err := s.WritePIDFile("r", 17); err != nil {
								t.Fatal(err)
							}
							want = ErrRunNotFound
							if format == ExactValueFormat {
								want = ErrValueFormatChanged
							}
						}
					}
					before, err := os.ReadFile(s.PIDFilePath("r"))
					if err != nil {
						t.Fatal(err)
					}
					port := AsContextPIDStore(decorator{decorator{s}})
					if port == nil {
						t.Fatal("contextual PID capability hidden by Unwrap")
					}
					if err := op(port, ctx); !errors.Is(err, want) {
						t.Fatalf("PID_ORIGINAL_LOST: got %v, want %v", err, want)
					}
					after, err := os.ReadFile(s.PIDFilePath("r"))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatalf("PID_REFUSAL_MUTATED: %q, %v", after, err)
					}
				})
			}
		}
	}
}

func TestContextPIDOwnedCleanup(t *testing.T) {
	for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := pidContextFixture(t, format)
			if got, err := s.ReadPIDFileContext(ctx, "r"); err != nil || got != 17 {
				t.Fatalf("read %d: %v", got, err)
			}
			if changed, err := s.RemovePIDFileIfContext(ctx, "r", 11); err != nil || changed {
				t.Fatalf("PID_CAS_REQUIRED: %v, %v", changed, err)
			}
			if got, err := s.ReadPIDFileContext(ctx, "r"); err != nil || got != 17 {
				t.Fatalf("replacement lost %d: %v", got, err)
			}
			if changed, err := s.RemovePIDFileIfContext(ctx, "r", 17); err != nil || !changed {
				t.Fatalf("owned removal %v: %v", changed, err)
			}
			if got, err := s.ReadPIDFileContext(ctx, "r"); err != nil || got != 0 {
				t.Fatalf("missing PID %d: %v", got, err)
			}
			if changed, err := s.RemovePIDFileIfContext(ctx, "r", 17); err != nil || changed {
				t.Fatalf("already absent %v: %v", changed, err)
			}
			if err := s.RemovePIDFileContext(ctx, "r"); err != nil {
				t.Fatal(err)
			}
			if err := s.WritePIDFileContext(ctx, "r", 31); err != nil {
				t.Fatal(err)
			}
			if err := s.RemovePIDFileContext(ctx, "r"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContextPIDCapabilityAbsence(t *testing.T) {
	s, _ := pidContextFixture(t, LegacyValueFormat)
	if AsContextPIDStore(opaqueDecorator{s}) != nil || AsContextPIDStore(nil) != nil {
		t.Fatal("absent contextual capability invented")
	}
	if err := s.WritePIDFile("orphan", 41); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadPIDFile("orphan"); err != nil || got != 41 {
		t.Fatalf("legacy orphan %d: %v", got, err)
	}
	if err := s.RemovePIDFile("orphan"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadPIDFile("orphan"); err != nil || got != 0 {
		t.Fatalf("legacy cleanup %d: %v", got, err)
	}
}

func TestContextPIDMalformedPreservesFile(t *testing.T) {
	s, ctx := pidContextFixture(t, LegacyValueFormat)
	for _, raw := range []string{"", "0", "-3", "not-a-pid"} {
		if err := os.WriteFile(s.PIDFilePath("r"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadPIDFileContext(ctx, "r"); err == nil {
			t.Fatalf("malformed read %q succeeded", raw)
		}
		if changed, err := s.RemovePIDFileIfContext(ctx, "r", 17); err == nil || changed {
			t.Fatalf("malformed CAS %q: %v, %v", raw, changed, err)
		}
		after, err := os.ReadFile(s.PIDFilePath("r"))
		if err != nil || string(after) != raw {
			t.Fatalf("malformed file changed: %q, %v", after, err)
		}
	}
}
