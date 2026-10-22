package ir

import (
	"fmt"
	"strings"
	"testing"
)

func rootVarsFallbackSrc(varsDecl string, routes ...string) string {
	return "dsl: 2\n\nvars:\n" + varsDecl + "\n" + fallbackWorkflow("", strings.Join(routes, "\n"))
}

func rootVarsRoute(name, when string) string {
	return fmt.Sprintf("    %s:\n      model: \"claude-haiku-4-5\"\n      when: %q\n", name, when)
}

func TestFallbackWhenRootVarsSubscriptWarnsWithoutBecomingAnError(t *testing.T) {
	cases := []struct {
		name       string
		when       string
		wantC189   int
		wantC173   int
		wantErrors bool
	}{
		{name: "static unknown key", when: "vars['nope'] == 'skip'", wantC189: 1},
		{name: "static declared key", when: "vars['policy'] == 'skip'", wantC189: 1},
		{name: "computed key", when: "vars[vars.policy] == 'skip'", wantC189: 1},
		{name: "bare vars", when: "vars"},
		{name: "declared dotted key", when: "vars.policy == 'skip'"},
		{name: "unknown dotted key stays C173", when: "vars.polcy == 'skip'", wantC173: 1, wantErrors: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := rootVarsFallbackSrc("  policy: string = \"skip\"\n", rootVarsRoute("gated", tc.when))
			cr := compileFallbackSrc(t, src)
			if got := countCode(cr, DiagRootVarsSubscript); got != tc.wantC189 {
				t.Errorf("C189 count = %d, want %d\ndiagnostics: %v", got, tc.wantC189, cr.Diagnostics)
			}
			if got := countCode(cr, DiagFallbackMalformed); got != tc.wantC173 {
				t.Errorf("C173 count = %d, want %d\ndiagnostics: %v", got, tc.wantC173, cr.Diagnostics)
			}
			if got := cr.HasErrors(); got != tc.wantErrors {
				t.Errorf("HasErrors = %v, want %v\ndiagnostics: %v", got, tc.wantErrors, cr.Diagnostics)
			}
			for _, d := range cr.Diagnostics {
				if d.Code == DiagRootVarsSubscript && d.Severity != SeverityWarning {
					t.Errorf("C189 severity = %s, want warning", d.Severity)
				}
			}
		})
	}
}

func TestFallbackRootVarsSubscriptKeepsC155AndC189ToOnePerGate(t *testing.T) {
	cases := []struct {
		name     string
		varsDecl string
		when     string
	}{
		{
			name:     "string[] default",
			varsDecl: "  tags: string[] = \"a,b\"\n",
			when:     "length(vars['tags']) > 0 || length(vars['tags']) > 1",
		},
		{
			name:     "json default",
			varsDecl: "  doc: json = \"{\\\"enabled\\\": true}\"\n",
			when:     "length(vars['doc']) > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := compileFallbackSrc(t, rootVarsFallbackSrc(tc.varsDecl, rootVarsRoute("gated", tc.when)))
			if got := countCode(cr, DiagVarListDefaultRouting); got != 1 {
				t.Errorf("C155 count = %d, want 1\ndiagnostics: %v", got, cr.Diagnostics)
			}
			if got := countCode(cr, DiagRootVarsSubscript); got != 1 {
				t.Errorf("C189 count = %d, want exactly one when C155 and C173 both inspect the gate\ndiagnostics: %v", got, cr.Diagnostics)
			}
			if cr.HasErrors() {
				t.Errorf("root vars subscript must compile with warnings only: %v", cr.Diagnostics)
			}
		})
	}
}

func TestFallbackRootVarsSubscriptWarnsOnceForEachGate(t *testing.T) {
	src := rootVarsFallbackSrc("  tags: string[] = \"a,b\"\n",
		rootVarsRoute("first", "length(vars['tags']) > 0"),
		rootVarsRoute("second", "length(vars['tags']) > 0"),
	)
	cr := compileFallbackSrc(t, src)
	if got := countCode(cr, DiagVarListDefaultRouting); got != 2 {
		t.Errorf("C155 count = %d, want one per gate\ndiagnostics: %v", got, cr.Diagnostics)
	}
	if got := countCode(cr, DiagRootVarsSubscript); got != 2 {
		t.Errorf("C189 count = %d, want one per gate\ndiagnostics: %v", got, cr.Diagnostics)
	}
}

func TestFallbackRootVarsSubscriptInsideLambda(t *testing.T) {
	cases := []struct {
		name     string
		when     string
		wantC155 int
	}{
		{name: "map body", when: "length(map([1], x => vars['nope'])) > 0"},
		{name: "filter body", when: "length(filter([1], x => vars['nope'] == 'on')) > 0"},
		{name: "reduce body", when: "reduce([1], false, (acc, x) => acc || vars['nope'] == 'on')"},
		{name: "C155 map body", when: "length(map([1], x => vars['tags'])) > 0", wantC155: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := rootVarsFallbackSrc("  policy: string = \"skip\"\n  tags: string[] = \"a,b\"\n", rootVarsRoute("gated", tc.when))
			cr := compileFallbackSrc(t, src)
			if got := countCode(cr, DiagRootVarsSubscript); got != 1 {
				t.Errorf("C189 count = %d, want 1\ndiagnostics: %v", got, cr.Diagnostics)
			}
			if got := countCode(cr, DiagVarListDefaultRouting); got != tc.wantC155 {
				t.Errorf("C155 count = %d, want %d\ndiagnostics: %v", got, tc.wantC155, cr.Diagnostics)
			}
			if cr.HasErrors() {
				t.Errorf("root vars subscript in a lambda must compile with warnings only: %v", cr.Diagnostics)
			}
		})
	}
}
