package configshare

import (
	"fmt"
	"regexp"
	"strings"
)

// categoryPlaceholder is the token a bot's config_share path templates carry
// when the surface is per-category; the mint substitutes the concrete category.
const categoryPlaceholder = "{category}"

// categoryRe is the safe shape for a category key expanded into a dotted path:
// letters/digits/underscore/hyphen only — no dot (would forge an extra path
// segment), no traversal, no whitespace. Matches the config-file category keys
// (feed-watch: "cyber", "a11y", …).
var categoryRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// HasCategoryPlaceholder reports whether any of the given template sets carries
// the {category} placeholder — i.e. the bot's config-share surface is
// per-category, so a mint MUST supply a category.
func HasCategoryPlaceholder(sets ...[]string) bool {
	for _, set := range sets {
		for _, t := range set {
			if strings.Contains(t, categoryPlaceholder) {
				return true
			}
		}
	}
	return false
}

// leafOf returns a path template's last segment — the field name a share
// selects on (e.g. "categories.{category}.feeds" → "feeds").
func leafOf(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 {
		return t[i+1:]
	}
	return t
}

// DeriveGrant expands a bot's declared config-share path templates (manifest
// config_share.editable_paths / visible_paths) for ONE category into the
// concrete (allowed, visible) dotted-path sets a Share pins at mint time. A
// "{category}" placeholder requires a well-formed, non-empty category and is
// substituted; templates without it pass through literally. The returned
// visible set is the union of the expanded editable + visible templates
// (VisiblePaths is a read superset of AllowedPaths), deduped, editable first.
//
// selectedFields narrows the grant to a SUBSET of the bot's declared editable
// fields (by leaf name — least privilege per share): empty = the full declared
// editable surface; otherwise only the named fields, and every name MUST match
// a declared editable leaf (else ErrValidation). A non-selected editable field
// is neither writable nor visible (it drops out of both sets), keeping a
// feeds-only editor blind to the editorial prompt.
//
// All failures wrap ErrValidation so the mint maps them to 400. The output is
// still run through ValidatePaths by the mint — DeriveGrant resolves the
// surface; ValidatePaths enforces the literal/no-overlap/no-forbidden rules.
func DeriveGrant(editable, visible []string, category string, selectedFields ...string) (allowed []string, visibleOut []string, err error) {
	if HasCategoryPlaceholder(editable, visible) {
		if category == "" {
			return nil, nil, fmt.Errorf("%w: category required for this bot's config-share surface", ErrValidation)
		}
		if !categoryRe.MatchString(category) {
			return nil, nil, fmt.Errorf("%w: invalid category %q (letters, digits, _ and - only)", ErrValidation, category)
		}
	}
	// Narrow the editable templates to the selected fields, if any.
	editableUsed := editable
	if len(selectedFields) > 0 {
		byLeaf := make(map[string][]string, len(editable))
		for _, t := range editable {
			if strings.TrimSpace(t) == "" {
				continue
			}
			byLeaf[leafOf(t)] = append(byLeaf[leafOf(t)], t)
		}
		editableUsed = nil
		for _, f := range selectedFields {
			ts, ok := byLeaf[f]
			if !ok {
				return nil, nil, fmt.Errorf("%w: field %q is not editable for this bot", ErrValidation, f)
			}
			editableUsed = append(editableUsed, ts...)
		}
	}
	expand := func(templates []string) ([]string, error) {
		out := make([]string, 0, len(templates))
		for _, t := range templates {
			if strings.TrimSpace(t) == "" {
				continue
			}
			p := strings.ReplaceAll(t, categoryPlaceholder, category)
			// A leftover brace = an unknown placeholder; fail closed rather
			// than pin a path carrying a literal "{…}" segment.
			if strings.ContainsAny(p, "{}") {
				return nil, fmt.Errorf("%w: unresolved placeholder in %q", ErrValidation, t)
			}
			out = append(out, p)
		}
		return out, nil
	}
	allowed, err = expand(editableUsed)
	if err != nil {
		return nil, nil, err
	}
	if len(allowed) == 0 {
		return nil, nil, fmt.Errorf("%w: config-share surface declares no editable paths", ErrValidation)
	}
	vis, err := expand(visible)
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool, len(allowed)+len(vis))
	visibleOut = make([]string, 0, len(allowed)+len(vis))
	for _, p := range allowed {
		if !seen[p] {
			seen[p] = true
			visibleOut = append(visibleOut, p)
		}
	}
	for _, p := range vis {
		if !seen[p] {
			seen[p] = true
			visibleOut = append(visibleOut, p)
		}
	}
	return allowed, visibleOut, nil
}
