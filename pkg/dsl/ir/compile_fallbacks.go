package ir

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// compileFallbacks lowers a node's `fallbacks:` block into the IR's
// ordered route list (ADR-087). Declaration order is the try order and
// is preserved verbatim.
//
// Values are carried RAW — `${VAR}` refs are resolved at run time, like
// every other backend/model/provider field — so the compiler never
// collapses a route an environment would have filled in.
func compileFallbacks(decls []*ast.FallbackDecl) []Fallback {
	if len(decls) == 0 {
		return nil
	}
	out := make([]Fallback, 0, len(decls))
	for _, d := range decls {
		if d == nil {
			continue
		}
		fb := Fallback{
			Name:     strings.TrimSpace(d.Name),
			Backend:  strings.TrimSpace(d.Backend),
			Model:    strings.TrimSpace(d.Model),
			Provider: strings.TrimSpace(d.Provider),
			Metered:  d.Metered,
			Action:   strings.ToLower(strings.TrimSpace(d.Action)),
			When:     strings.TrimSpace(d.When),
		}
		for _, on := range d.On {
			if t := strings.TrimSpace(on); t != "" {
				fb.On = append(fb.On, t)
			}
		}
		out = append(out, fb)
	}
	return out
}
