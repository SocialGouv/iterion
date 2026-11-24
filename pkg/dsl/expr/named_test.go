package expr

import (
	"strings"
	"testing"
)

// namedCtx is a Context with only named namespaces: the shape a connector
// package's expressions are evaluated under (ADR-168 §9).
func namedCtx() *Context {
	payload := map[string]any{"type": "url_verification", "event": map[string]any{"text": "hi"}}
	return &Context{Named: map[string]func([]string) any{
		"payload": func(path []string) any {
			var v any = payload
			for _, seg := range path {
				m, ok := v.(map[string]any)
				if !ok {
					return nil
				}
				v = m[seg]
			}
			return v
		},
	}}
}

func TestNamedNamespaceResolvesThroughItsCallback(t *testing.T) {
	for src, want := range map[string]any{
		`payload.type`:                       "url_verification",
		`payload.event.text`:                 "hi",
		`payload["event"]["text"]`:           "hi",
		`payload.type == "url_verification"`: true,
	} {
		got, err := MustParse(src).Eval(namedCtx())
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got != want {
			t.Fatalf("%s = %v, want %v", src, got, want)
		}
	}
}

// An unknown root under a Context with no Input is an error, never a field
// of the implicit `input` namespace: a connector expression that misspells
// `payload` must fail, not read nothing.
func TestUnknownRootWithoutInputIsAnError(t *testing.T) {
	_, err := MustParse(`paylod.type`).Eval(namedCtx())
	if err == nil || !strings.Contains(err.Error(), `unknown namespace "paylod"`) {
		t.Fatalf("want an unknown-namespace error, got %v", err)
	}
}

// A named namespace never shadows a standard one: `vars` resolves through
// Vars even when a caller also names it.
func TestNamedNamespaceNeverShadowsAStandardOne(t *testing.T) {
	ctx := &Context{
		Vars:  func([]string) any { return "standard" },
		Named: map[string]func([]string) any{"vars": func([]string) any { return "named" }},
	}
	got, err := MustParse(`vars.x`).Eval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "standard" {
		t.Fatalf("vars.x = %v, want the standard namespace's answer", got)
	}
}
