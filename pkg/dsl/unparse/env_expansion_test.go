package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// `env_expansion:` must survive the .bot round trip the unparser owns —
// `iterion fmt`, the canonical text, the studio's save-back. Dropping the
// line is the silent failure of this property: the body reads the same, the
// node no longer declines, and the engine expands text the author declared
// verbatim with no diff to see.
func TestUnparseEnvExpansion_RoundTrip(t *testing.T) {
	src := `tool emit:
  command: "printf %s ${NAME:-d}"
  env_expansion: none

workflow w:
  entry: emit
  emit -> done
`
	res := parsertest.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}

	out := unparse.Unparse(res.File)
	if !strings.Contains(out, "env_expansion: none") {
		t.Fatalf("unparse output missing env_expansion — `iterion fmt` would delete the declaration:\n%s", out)
	}

	res2 := parsertest.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	if got := res2.File.Tools[0].EnvExpansion; got != "none" {
		t.Fatalf("EnvExpansion = %q after the round trip, want \"none\"", got)
	}
}
