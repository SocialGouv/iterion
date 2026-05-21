package model

import (
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// resolveCursorFragments turns an agent/judge cursors block into the
// sorted, ready-to-render `**Name:** fragment` lines that the delegate
// appends under "## Calibration". Returns nil when no fragments apply
// (block missing, disabled, or every setting resolves to an unknown
// cursor — the compile-time validator already warned the operator).
//
// Misses are silently dropped: C083/C084 already fired at compile
// time, and a runtime error would block legitimate runs whose values
// come from `${VAR}` overrides.
func resolveCursorFragments(inv *ir.CursorInvocation, decls map[string]*ir.CursorDef) []string {
	if inv == nil || !inv.Enabled || len(inv.Settings) == 0 {
		return nil
	}
	type resolved struct {
		Key    string
		Prompt string
	}
	var out []resolved
	for _, s := range inv.Settings {
		def, ok := decls[s.Key]
		if !ok {
			continue
		}
		value := ir.ExpandEnvWithDefault(s.Value)
		prompt, ok, _ := ir.ResolveCursorValue(def, value)
		if !ok {
			continue
		}
		out = append(out, resolved{Key: s.Key, Prompt: prompt})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	frags := make([]string, len(out))
	for i, r := range out {
		frags[i] = fmt.Sprintf("**%s:** %s", titleCase(r.Key), r.Prompt)
	}
	return frags
}

// titleCase upper-cases the first byte of a cursor name. Cursor names
// are guaranteed ASCII identifiers by the DSL grammar, so this stays
// out of golang.org/x/text and avoids a rune-slice allocation.
func titleCase(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-('a'-'A')) + s[1:]
}
