package ir

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVarExpansionStopsTraversal(t *testing.T) {
	t.Parallel()
	for _, container := range []any{
		[]any{"a", []any{"b", "c"}},
		map[string]any{"a": "a", "b": []any{"b", "c"}},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		out, err := expandJSONLeavesContext(ctx, container, func(string) (string, error) {
			calls++
			cancel()
			return "unused", nil
		})
		cancel()
		if out != nil || !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("walk continued after cancellation: %v, %v, calls=%d", out, err, calls)
		}
	}
}

func TestVarExpansionPropagatesErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("expansion failed")
	for _, container := range []any{[]any{"a", "b"}, map[string]any{"a": "a", "b": "b"}} {
		calls := 0
		out, err := expandJSONLeavesContext(t.Context(), container, func(string) (string, error) { calls++; return "unused", want })
		if out != nil || !errors.Is(err, want) || calls != 1 {
			t.Fatalf("expansion error lost: %v, %v, calls=%d", out, err, calls)
		}
	}
}

func TestVarExpansionStopsParser(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"${A}", "$A "} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		raw := prefix + strings.Repeat("x", 10000)
		out, visited, err := expandWithDefaultContext(ctx, raw, func(string) string { calls++; cancel(); return "unused" }, expandPolicy{})
		cancel()
		if out != "" || !errors.Is(err, context.Canceled) || calls != 1 || visited > len(prefix) {
			t.Fatalf("parser continued after cancellation: len=%d err=%v calls=%d visited=%d", len(out), err, calls, visited)
		}
	}
}
