package workspacetrack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ignorer.Match sits on the hot path of a feature that is ON by default:
// Capture calls it for every entry at every node boundary of every
// in-place run, and Restore's deletion walk calls it again. A regression
// here is measured in seconds per boundary, not microseconds per call —
// it was once 667 µs/path (1129 allocs), which turned a 130 ms walk into
// 7 s on a real repository.
//
// Synthetic rule set and path so the benchmark is portable: ~90 rules of
// the shapes a real .gitignore carries (anchored literals, bare names,
// globs, ** patterns, negations) against a deep path.
func benchIgnorer(b *testing.B) *Ignorer {
	b.Helper()
	ws := b.TempDir()
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "build%d/\n", i)
	}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&sb, "*.tmp%d\n", i)
	}
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&sb, "docs/gen%d/**/out\n", i)
	}
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&sb, "**/cache%d.json\n", i)
	}
	sb.WriteString("!vendor/**/*.exe\n!e2e/fixtures/**/package-lock.json\n")
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte(sb.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	return NewIgnorer(ws)
}

func BenchmarkIgnorerMatch(b *testing.B) {
	ig := benchIgnorer(b)
	const p = "pkg/backend/delegate/pisdk/internal/wire/types/message.go"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ig.Match(p, false)
	}
}
