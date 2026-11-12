package sandbox

import (
	"strings"
	"testing"
)

func TestShouldStreamScriptPreservesReservedStdin(t *testing.T) {
	t.Parallel()
	script := strings.Repeat("x", MaxInlineArgBytes+1)
	for _, shell := range []string{"sh", "bash"} {
		if got := ShouldStreamScriptViaStdin([]string{shell, "-c", script}, ExecOpts{}); got != script {
			t.Fatalf("free stdin stopped streaming %s", shell)
		}
		if got := ShouldStreamScriptViaStdin([]string{shell, "-c", script}, ExecOpts{KeepStdinOpen: true}); got != "" {
			t.Fatalf("reserved stdin stolen for %s", shell)
		}
	}
}
