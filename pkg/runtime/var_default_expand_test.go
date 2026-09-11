package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// `${VAR:-default}` is the idiom the DSL uses everywhere — command:, model:,
// timeout:, cursors all honour it through ir.ExpandEnvWithDefault. A `vars:`
// default went through os.Expand instead, which treats the whole
// `VAR:-default` as a variable NAME: unset, so the var silently became the
// empty string.
//
// Silently is the operative word. Nothing failed at compile time; the value
// simply arrived empty wherever it was consumed, and the error surfaced far
// from its cause — in the field, as a security preflight reporting three
// missing scanner versions that were all declared correctly.
func TestVarDefaultHonoursShellStyleFallback(t *testing.T) {
	eng := &Engine{workflow: &ir.Workflow{Vars: map[string]*ir.Var{
		"pinned":     {Name: "pinned", Type: ir.VarString, HasDefault: true, Default: "${PROBE_VAR_UNSET:-8.24.3}"},
		"overridden": {Name: "overridden", Type: ir.VarString, HasDefault: true, Default: "${PROBE_VAR_SET:-fallback}"},
		"literal":    {Name: "literal", Type: ir.VarString, HasDefault: true, Default: "plain"},
		"nested":     {Name: "nested", Type: ir.VarString, HasDefault: true, Default: "${PROBE_VAR_UNSET:-${PROBE_VAR_ALSO_UNSET:-deep}}"},
	}}}
	t.Setenv("PROBE_VAR_SET", "from-env")

	vars := eng.resolveVars(nil)

	for _, tc := range []struct{ name, want string }{
		{"pinned", "8.24.3"},
		{"overridden", "from-env"},
		{"literal", "plain"},
		{"nested", "deep"},
	} {
		if got := vars[tc.name]; got != tc.want {
			t.Errorf("var %q = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The engine-supplied keys must keep resolving through the same expander, and
// must still be overridable by a fallback when they are empty.
func TestVarDefaultKeepsEngineSuppliedKeys(t *testing.T) {
	eng := &Engine{
		workDir: "/repo",
		workflow: &ir.Workflow{Vars: map[string]*ir.Var{
			"ws":     {Name: "ws", Type: ir.VarString, HasDefault: true, Default: "${PROJECT_DIR}"},
			"bundle": {Name: "bundle", Type: ir.VarString, HasDefault: true, Default: "${BUNDLE_DIR:-none}"},
		}},
	}

	vars := eng.resolveVars(nil)

	if got := vars["ws"]; got != "/repo" {
		t.Errorf("PROJECT_DIR = %q, want /repo", got)
	}
	// No bundle on a plain .bot run: the engine resolves BUNDLE_DIR to empty,
	// so the author's fallback is what should reach the workflow.
	if got := vars["bundle"]; got != "none" {
		t.Errorf("BUNDLE_DIR fallback = %q, want none", got)
	}
}
