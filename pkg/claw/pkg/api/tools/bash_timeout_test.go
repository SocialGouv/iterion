package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
)

func TestBashTimeoutSchema(t *testing.T) {
	tool := tools.BashTool()
	p, ok := tool.InputSchema.Properties["timeout_seconds"]
	if !ok || p.Type != "integer" || !strings.Contains(p.Description, "600") || !strings.Contains(p.Description, "30") {
		t.Fatalf("missing bounded timeout contract: %+v", p)
	}
	for _, required := range tool.InputSchema.Required {
		if required == "timeout_seconds" {
			t.Fatal("the timeout must remain optional")
		}
	}
}

func TestBashTimeoutRejectsInvalidInputBeforeSpawn(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"string", "60"}, {"boolean", true}, {"zero", 0},
		{"negative", -1}, {"fraction", 1.5}, {"above_max", 601},
		{"overflow", uint64(math.MaxUint64)}, {"huge_float", math.MaxFloat64},
		{"nan", math.NaN()}, {"infinity", math.Inf(1)},
		{"invalid_number", json.Number("not-a-number")},
		{"non_json_number", json.Number("0x1p2")},
		{"rounded_fraction", json.Number("600.00000000000000001")},
		{"huge_number", json.Number("1e1000000000")},
		{"number_fraction", json.Number("1.5")},
		{"object", map[string]any{}}, {"array", []any{30}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "spawned")
			_, err := tools.ExecuteBashWithEnv(context.Background(), map[string]any{
				"command": `printf started > "$BASH_TIMEOUT_MARKER"`, "timeout_seconds": tc.value,
			}, "", []string{"BASH_TIMEOUT_MARKER=" + marker})
			if err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
				t.Errorf("expected timeout input error, got %v", err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid input spawned a command: %v", err)
			}
		})
	}
}

func TestBashTimeoutPublicAPINumericForms(t *testing.T) {
	for _, value := range []any{1, int64(30), float64(60), json.Number("90"), json.Number("1.0"), json.Number("6e2"), 600} {
		out, err := tools.ExecuteBash(context.Background(), map[string]any{"command": "printf done", "timeout_seconds": value}, "")
		if err != nil || out != "done" {
			t.Fatalf("timeout %v (%T): output=%q error=%v", value, value, out, err)
		}
	}
}

func TestBashExplicitTimeout(t *testing.T) {
	start := time.Now()
	out, err := tools.ExecuteBash(context.Background(), map[string]any{
		"command": "printf ready; sleep 120", "timeout_seconds": 1,
	}, "")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1s") || out != "ready" {
		t.Fatalf("output=%q error=%v", out, err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("one-second timeout took %s", elapsed)
	}
}

func TestBashTimeoutCallerCancellationWins(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				timer := time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
			}
			defer cancel()
			start := time.Now()
			_, err := tools.ExecuteBash(ctx, map[string]any{"command": "sleep 120", "timeout_seconds": 600}, "")
			if !errors.Is(err, want) {
				t.Fatalf("parent must win over 600 seconds: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("parent cancellation took %s", elapsed)
			}
		})
	}
}

func TestBashExtendedTimeoutCancellationKillsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group termination is a Unix contract")
	}
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := tools.ExecuteBashWithEnv(ctx, map[string]any{
		"command":         `(sleep 1; printf survived > "$BASH_TIMEOUT_MARKER") & sleep 120`,
		"timeout_seconds": 600,
	}, "", []string{"BASH_TIMEOUT_MARKER=" + marker})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected parent deadline, got %v", err)
	}
	// An orphan no longer holds the API open thanks to WaitDelay, but that
	// alone is insufficient: it must not perform its delayed effect either.
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a descendant survived cancellation: %v", err)
	}
}

func TestBashExtendedTimeoutAlreadyCancelledDoesNotSpawn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "spawned")
	_, err := tools.ExecuteBashWithEnv(ctx, map[string]any{
		"command": `printf started > "$BASH_TIMEOUT_MARKER"`, "timeout_seconds": 600,
	}, "", []string{"BASH_TIMEOUT_MARKER=" + marker})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected parent cancellation, got %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already cancelled caller spawned a command: %v", err)
	}
}

func TestBashExtendedTimeoutDrainsOutputBeyondLimit(t *testing.T) {
	out, err := tools.ExecuteBash(context.Background(), map[string]any{
		"command":         `printf start; printf err >&2; printf '%0200000d' 0`,
		"timeout_seconds": 600,
	}, "")
	want := "starterr" + strings.Repeat("0", 10000-len("starterr")) + "\n... [output truncated]"
	if err != nil || out != want {
		t.Fatalf("large combined output: length=%d error=%v", len(out), err)
	}
}

// This is a real wall-clock regression test, not a substituted short default:
// before the optional timeout the public API kills this command at 30 seconds.
func TestBashTimeoutExtendsBeyondThirtySeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("real >30-second command")
	}
	start := time.Now()
	out, err := tools.ExecuteBashWithEnv(context.Background(), map[string]any{
		"command": `sleep 31; printf "$BASH_TIMEOUT_PROBE"`, "timeout_seconds": 40,
	}, "", []string{"BASH_TIMEOUT_PROBE=finished-after-default"})
	if err != nil || out != "finished-after-default" {
		t.Fatalf("extended command: output=%q error=%v", out, err)
	}
	if time.Since(start) <= 30*time.Second {
		t.Fatal("the proof did not run beyond the legacy timeout")
	}
}
