package model

import (
	"crypto/rand"
	"maps"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// authoredSecrets is one rendering's table of the `{{secrets.NAME}}`
// references the AUTHOR wrote in an action's params: the only text of an
// argument that ever becomes a secret.
//
// A placeholder cannot carry that provenance. `__ITERION_SECRET_X__` is
// deterministic, so data spells it as well as the author does: a model
// writes it into an output, two inputs join into it, a JSON escape decodes
// into it once the argument is read. So each authored reference renders to a
// marker drawn from crypto/rand for this one rendering, which no data can
// spell: the marker never reaches a model, an event, a log or a refusal, and
// the next rendering draws another. Once the argument is narrowed, leaf
// resolves each marker for the transport — the value, through the `params`
// grant, on an HTTP call; the reference's own text (the placeholder) on a
// native transport — and nothing else: a placeholder that arrives through
// data stays the text it is, whatever its secret grants.
//
// A marker spells nothing JSON reads: letters, digits and underscores, an
// underscore first so no escape (`\u`, `\n`) can take its opening bytes, and
// no JSON literal. A narrowed argument carries it whole, in a string; a
// marker outside a JSON string fails the decode, and a key holding one is
// refused (leaves).
type authoredSecrets struct {
	guard     *secretguard.Guard
	transport actionSecrets
	// byName is the marker of each name referenced, drawn on first use.
	byName map[string]string
	// text is each marker's canonical text, secretRefText's reading of its
	// name: what the reference renders to on every other surface.
	text map[string]string
	// order is the markers in the order they were drawn.
	order []string
	// leafR and shownR are the two readings of the markers drawn so far,
	// rebuilt on first use after a draw.
	leafR, shownR *strings.Replacer
}

// authoredMarkerPrefix opens every marker. It is no secret: what no data can
// spell is the random part.
const authoredMarkerPrefix = "__ITERION_AUTHORED_"

func newAuthoredSecrets(guard *secretguard.Guard, transport actionSecrets) *authoredSecrets {
	return &authoredSecrets{guard: guard, transport: transport, byName: map[string]string{}, text: map[string]string{}}
}

// marker is the template layer's rendering of an authored `{{secrets.NAME}}`
// (resolveTemplateWith's secretRef): the marker standing for name in this
// rendering, drawn on its first reference.
func (a *authoredSecrets) marker(name string) string {
	if m, ok := a.byName[name]; ok {
		return m
	}
	m := authoredMarkerPrefix + rand.Text() + "__"
	a.byName[name] = m
	a.text[m] = secretRefText(a.guard, name)
	a.order = append(a.order, m)
	a.leafR, a.shownR = nil, nil
	return m
}

// replacers builds the two readings of the markers drawn so far: leaf, what
// the transport receives in a marker's place, and shown, its canonical text.
// The value is materialised for one marker's own text at a time, never for
// the string around it: a placeholder data put beside an authored reference
// is not the author's.
func (a *authoredSecrets) replacers() (leaf, shown *strings.Replacer) {
	if a.leafR != nil || len(a.order) == 0 {
		return a.leafR, a.shownR
	}
	leafPairs := make([]string, 0, 2*len(a.order))
	shownPairs := make([]string, 0, 2*len(a.order))
	for _, m := range a.order {
		text := a.text[m]
		shownPairs = append(shownPairs, m, text)
		if a.transport == secretsMaterialized {
			text = a.guard.MaterializeFor(secretsink.Params, text)
		}
		leafPairs = append(leafPairs, m, text)
	}
	a.leafR, a.shownR = strings.NewReplacer(leafPairs...), strings.NewReplacer(shownPairs...)
	return a.leafR, a.shownR
}

// leaf is what the transport receives for a string of a narrowed argument:
// each marker resolved, every other byte as it is.
func (a *authoredSecrets) leaf(s string) string {
	if r, _ := a.replacers(); r != nil {
		return r.Replace(s)
	}
	return s
}

// shown is s with each marker as its canonical text — the form a refusal
// quotes.
func (a *authoredSecrets) shown(s string) string {
	if _, r := a.replacers(); r != nil {
		return r.Replace(s)
	}
	return s
}

// holds reports whether s carries a marker of this rendering.
func (a *authoredSecrets) holds(s string) bool {
	if !strings.Contains(s, authoredMarkerPrefix) {
		return false
	}
	for _, m := range a.order {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// outsideString reports whether text holds a marker where JSON reads syntax
// rather than a string: read with every marker as `0`, the text decodes. A
// marker inside a string changes nothing a decode reads, so the text then
// fails for its own reason.
func (a *authoredSecrets) outsideString(text string) bool {
	if !a.holds(text) {
		return false
	}
	pairs := make([]string, 0, 2*len(a.order))
	for _, m := range a.order {
		pairs = append(pairs, m, "0")
	}
	_, ok := decodeJSONValue(strings.TrimSpace(strings.NewReplacer(pairs...).Replace(text)))
	return ok
}

// leaves resolves every string of v through leaf and refuses a map key that
// holds a marker: a key is sent as written, so a secret there would reach
// the transport as its marker. The refused key is returned as found, and
// keys are read in sorted order so the one named does not depend on map
// iteration.
func (a *authoredSecrets) leaves(v any) (out any, refusedKey string, ok bool) {
	switch x := v.(type) {
	case string:
		return a.leaf(x), "", true
	case map[string]any:
		m := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if a.holds(k) {
				return nil, k, false
			}
			nv, bad, ok := a.leaves(x[k])
			if !ok {
				return nil, bad, false
			}
			m[k] = nv
		}
		return m, "", true
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			nv, bad, ok := a.leaves(e)
			if !ok {
				return nil, bad, false
			}
			l[i] = nv
		}
		return l, "", true
	}
	return v, "", true
}

// markerKey is the first key of v — maps and slices walked, keys in sorted
// order — that holds a marker, "" when none does. A key data carries is not
// one: only this rendering's markers name the author.
func (a *authoredSecrets) markerKey(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if a.holds(k) {
				return k
			}
			if r := a.markerKey(x[k]); r != "" {
				return r
			}
		}
	case []any:
		for _, e := range x {
			if r := a.markerKey(e); r != "" {
				return r
			}
		}
	}
	return ""
}

// leafText resolves the markers of a text argument — a string or untyped
// declaration, or a key the operation does not declare — each in place, the
// text sent as written. One refusal: the text reads as JSON and a KEY holds
// a marker. A key is sent as written on every declaration, so a secret may
// not stand in one anywhere; the refused key is returned as found.
func (a *authoredSecrets) leafText(text string) (out, refusedKey string, ok bool) {
	if a.holds(text) {
		if decoded, isJSON := decodeJSONValue(strings.TrimSpace(text)); isJSON {
			if k := a.markerKey(decoded); k != "" {
				return "", k, false
			}
		}
	}
	return a.leaf(text), "", true
}
