package ir

import (
	"fmt"
	"strings"
	"testing"
)

func TestAsyncInteractionRefusesUnsupportedBackend(t *testing.T) {
	for _, kind := range []string{"agent", "judge"} {
		for _, backend := range []string{"codex", "kimi", "grok", "claw", "claude_code", "pi", "auto", "${ASYNC_BACKEND}"} {
			for _, inherited := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/inherited=%t", kind, backend, inherited), func(t *testing.T) {
					src := kind + " a:\n  model: \"m\"\n  interaction: async\n"
					if !inherited {
						src += "  backend: \"" + backend + "\"\n"
					}
					src += "workflow w:\n  entry: a\n"
					if inherited {
						src += "  default_backend: \"" + backend + "\"\n"
					}
					src += "  a -> done\n"
					cr := compileFile(t, src)
					want := 0
					if backend == "codex" || backend == "kimi" || backend == "grok" {
						want = 1
					}
					if got := countCode(cr, DiagCode("C267")); got != want {
						t.Fatalf("unsupported async backend diagnostics = %d, want %d: %+v", got, want, cr.Diagnostics)
					}
				})
			}
		}
	}
}

func TestAsyncInteractionChecksFallbackAndKeepsOtherModes(t *testing.T) {
	src := "agent a:\n  backend: claude_code\n  model: \"m\"\n  interaction: async\n  fallbacks:\n    backup:\n      backend: codex\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n"
	if cr := compileFile(t, src); countCode(cr, DiagCode("C267")) != 1 {
		t.Fatalf("unsupported async fallback was not refused: %+v", cr.Diagnostics)
	}
	for _, mode := range []string{"none", "human"} {
		if cr := compileFile(t, strings.ReplaceAll(src, "interaction: async", "interaction: "+mode)); countCode(cr, DiagCode("C267")) != 0 {
			t.Fatalf("non-async mode %s was refused: %+v", mode, cr.Diagnostics)
		}
	}
}
