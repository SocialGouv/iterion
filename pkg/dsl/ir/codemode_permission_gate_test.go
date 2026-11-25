package ir

import (
	"strings"
	"testing"
)

// C358 (#3647): a node that turns codemode on under a permission gate that
// would not let the `codemode` tool itself run. Vigie's anti-injection
// boundary is the shape that surfaced it: `permission: deny` with an
// allow-list naming WebFetch and TodoWrite refuses every program before it
// starts, and nothing said so at compile time.
func codemodeUnderGate(permissionBlock, codemode string) string {
	return `
agent a:
  model: "m"
  backend: "claw"
  codemode: ` + codemode + `

workflow w:
` + permissionBlock + `
  entry: a
  a -> done
`
}

// compilesClean fails the test on any error diagnostic: a silent case that
// does not compile would be silent for the wrong reason.
func compilesClean(t *testing.T, r *CompileResult) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("the fixture does not compile, so its silence proves nothing: %+v", d)
		}
	}
}

func TestCodemodeUnderADenyGateThatDoesNotAllowItWarns(t *testing.T) {
	t.Parallel()
	r := compileFile(t, codemodeUnderGate(`  permission: deny
  allow: ["WebFetch(*)", "TodoWrite"]`, "on"))
	if got := countCode(r, DiagCodemodeRefusedByPermissionGate); got != 1 {
		t.Fatalf("C358 under deny without codemode allowed: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code == DiagCodemodeRefusedByPermissionGate && !strings.Contains(d.Message, "refused before it runs") {
			t.Fatalf("the deny warning does not say what happens to the program: %q", d.Message)
		}
	}
}

func TestCodemodeUnderAnAskGateThatDoesNotAllowItWarns(t *testing.T) {
	t.Parallel()
	r := compileFile(t, codemodeUnderGate(`  permission: ask
  allow: ["WebFetch(*)"]`, "on"))
	if got := countCode(r, DiagCodemodeRefusedByPermissionGate); got != 1 {
		t.Fatalf("C358 under ask without codemode allowed: %d time(s), want 1: %+v", got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code == DiagCodemodeRefusedByPermissionGate && !strings.Contains(d.Message, "pauses the run") {
			t.Fatalf("the ask warning does not say the run pauses: %q", d.Message)
		}
	}
}

// Every shape where the program runs stays silent: the author listed it,
// the gate is off, or codemode is not on as written.
func TestCodemodeTheGateLetsRunDoesNotWarn(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"codemode allowed": codemodeUnderGate(`  permission: deny
  allow: ["WebFetch(*)", "TodoWrite", "codemode"]`, "on"),
		"gate off": codemodeUnderGate(`  permission: off`, "on"),
		"no gate":  codemodeUnderGate(``, "on"),
		"codemode off": codemodeUnderGate(`  permission: deny
  allow: ["WebFetch(*)"]`, "off"),
		"allow everything": codemodeUnderGate(`  permission: deny
  allow: ["*"]`, "on"),
	} {
		t.Run(name, func(t *testing.T) {
			r := compileFile(t, src)
			compilesClean(t, r)
			if got := countCode(r, DiagCodemodeRefusedByPermissionGate); got != 0 {
				t.Fatalf("C358 fired where the program runs: %d time(s): %+v", got, r.Diagnostics)
			}
		})
	}
}

// The node's own allow list replaces the workflow's of the same kind: a node
// that lists codemode runs its programs under a workflow that does not.
func TestANodeAllowListNamingCodemodeSilencesTheWarning(t *testing.T) {
	t.Parallel()
	r := compileFile(t, `
agent a:
  model: "m"
  backend: "claw"
  codemode: on
  allow: ["WebFetch(*)", "codemode"]

workflow w:
  permission: deny
  allow: ["WebFetch(*)"]
  entry: a
  a -> done
`)
	compilesClean(t, r)
	if got := countCode(r, DiagCodemodeRefusedByPermissionGate); got != 0 {
		t.Fatalf("C358 fired on a node whose own allow list names codemode: %+v", r.Diagnostics)
	}
}
