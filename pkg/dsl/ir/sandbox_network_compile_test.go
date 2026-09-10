package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A network block's mode and inherit are refused at compile (C044) with the
// accepted words named — not at the driver's prepare, after `iterion
// validate` said OK. `merge` is the inherit DEFAULT, spelled by omission; the
// word itself is not a value.
func TestSandboxNetworkModeAndInheritAreCheckedAtCompile(t *testing.T) {
	compile := func(network string) []string {
		src := "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    image: \"img\"\n    network:\n" + network + "  a -> done\n"
		pr := parser.Parse("net.bot", src)
		if len(pr.Diagnostics) != 0 {
			t.Fatalf("parse: %v", pr.Diagnostics)
		}
		res := Compile(pr.File)
		var out []string
		for _, d := range res.Diagnostics {
			if d.Severity == SeverityError {
				out = append(out, string(d.Code)+": "+d.Message)
			}
		}
		return out
	}
	if errs := compile("      mode: allowlist\n      inherit: append\n"); len(errs) != 0 {
		t.Fatalf("a valid network block drew %v", errs)
	}
	if errs := compile("      mode: allowlist\n"); len(errs) != 0 {
		t.Fatalf("an omitted inherit (merge) drew %v", errs)
	}
	for _, c := range []struct{ body, want string }{
		{"      mode: allowlist\n      inherit: merge\n", "invalid sandbox.network inherit \"merge\""},
		{"      mode: allowlist\n      inherit: false\n", "invalid sandbox.network inherit \"false\""},
		{"      mode: blocklist\n", "invalid sandbox.network mode \"blocklist\""},
	} {
		errs := compile(c.body)
		if len(errs) != 1 || !strings.HasPrefix(errs[0], "C044: ") || !strings.Contains(errs[0], c.want) {
			t.Errorf("%q: want one C044 containing %q, got %v", c.body, c.want, errs)
		}
	}
}
