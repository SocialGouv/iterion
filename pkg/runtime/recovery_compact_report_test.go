package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// compactorExecutor is a NodeExecutor that only exists to answer Compact.
type compactorExecutor struct{ err error }

func (compactorExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (c compactorExecutor) Compact(context.Context, string) error { return c.err }

// plainExecutor implements NodeExecutor and nothing else.
type plainExecutor struct{}

func (plainExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, nil
}

// The engine emits EventNodeRecovery BEFORE it calls tryCompact, so the run
// log already says compact_and_retry was chosen. A compaction that found
// nothing and said nothing therefore left that log reading as a compaction
// that happened — documented as functional, which is worse than absent.
// ErrCompactionUnsupported is not a failure (the retry is legitimate), but
// it is not silence either, and what it names is the key it found empty.
func TestTryCompact_ReportsWhatItFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec NodeExecutor
		want string
	}{
		{
			name: "nothing under the key: reported, not swallowed",
			exec: compactorExecutor{err: fmt.Errorf("claw executor (node %q): nothing to compact under session key %q: %w", "copi", "assistant_conversation", ErrCompactionUnsupported)},
			want: `nothing was compacted for node "copi"`,
		},
		{
			name: "a real compaction failure still warns",
			exec: compactorExecutor{err: errors.New("the store exploded")},
			want: "compact failed",
		},
		{
			name: "an executor with no compaction at all",
			exec: plainExecutor{},
			want: "does not support compaction",
		},
		{
			name: "a compaction that fired says nothing here",
			exec: compactorExecutor{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			e := &Engine{executor: tc.exec, logger: iterlog.New(iterlog.LevelInfo, &logs)}
			e.tryCompact(context.Background(), "copi")
			got := logs.String()
			if tc.want == "" {
				if got != "" {
					t.Fatalf("a compaction that fired logged %q here; its own line belongs to the executor", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("the recovery log does not carry %q: %q", tc.want, got)
			}
			if !strings.Contains(got, "assistant_conversation") && strings.Contains(tc.want, "nothing was compacted") {
				t.Errorf("the reported gap drops the key the executor named: %q", got)
			}
		})
	}
}
