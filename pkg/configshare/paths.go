package configshare

import (
	"fmt"
	"sort"
	"strings"
)

// forbiddenKeys are rejected at any depth of a patch — prototype-pollution
// vectors that must never enter a merge.
var forbiddenKeys = map[string]bool{"__proto__": true, "constructor": true, "prototype": true}

// hardForbiddenSegments are field names a share PATCH may NEVER touch even if a
// mis-configured AllowedPaths listed them. `sinks` routes the digest (which
// webhook / channel) — off-limits to an editor regardless of the grant.
var hardForbiddenSegments = map[string]bool{"sinks": true}

type patchLeaf struct {
	path  []string
	value any
}

// collectPatchLeaves walks a patch and returns its allowed leaf writes, or an
// error (fail-closed — the WHOLE request is rejected, never a silent strip) if
// any key at any depth is outside the allow-list, is a prototype-pollution
// key, is malformed, or hits a hard-forbidden segment.
func collectPatchLeaves(patch map[string]any, allowed map[string]bool) ([]patchLeaf, error) {
	var out []patchLeaf
	var walk func(node map[string]any, prefix []string) error
	walk = func(node map[string]any, prefix []string) error {
		for k, v := range node {
			if forbiddenKeys[k] || hardForbiddenSegments[k] || k == "" || strings.Contains(k, ".") {
				return fmt.Errorf("illegal key %q", k)
			}
			path := append(append([]string{}, prefix...), k)
			dotted := strings.Join(path, ".")
			if allowed[dotted] {
				// A leaf grant writes a scalar or array value — never an object
				// subtree. Rejecting object values is what makes
				// hardForbiddenSegments a true backstop: an over-broad grant
				// (e.g. allowed=["categories.a11y"]) can't be handed an object
				// carrying `sinks` that the key-walk above never traverses.
				if _, isObj := v.(map[string]any); isObj {
					return fmt.Errorf("field %q must be a value, not an object", dotted)
				}
				// And scan the value (an array may hold objects) for any
				// forbidden key at any depth.
				if err := rejectForbiddenIn(v); err != nil {
					return err
				}
				out = append(out, patchLeaf{path: path, value: v})
				continue
			}
			child, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("field %q is not editable", dotted)
			}
			if err := walk(child, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(patch, nil); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("patch has no editable field")
	}
	return out, nil
}

// rejectForbiddenIn recursively scans a value for a forbidden or hard-forbidden
// key, so a forbidden field (sinks / prototype-pollution) can't ride inside the
// value of an over-broad allowed leaf grant — the backstop hardForbiddenSegments
// promises, closed even when the key is never traversed as a patch key.
func rejectForbiddenIn(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			if forbiddenKeys[k] || hardForbiddenSegments[k] {
				return fmt.Errorf("illegal key %q in value", k)
			}
			if err := rejectForbiddenIn(vv); err != nil {
				return err
			}
		}
	case []any:
		for _, vv := range t {
			if err := rejectForbiddenIn(vv); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneForbidden removes any prototype-pollution or hard-forbidden key
// (`sinks`) at ANY depth of a value. It is the READ-side backstop symmetric to
// rejectForbiddenIn on the write side: a broad or mis-configured VisiblePaths
// entry (an ancestor path whose subtree carries `sinks`) must never project the
// digest routing to a share editor. ValidatePaths rejects a visible path that
// *is* a forbidden segment, but not one that is an *ancestor* of it — so the
// projection prunes unconditionally, keeping `sinks` off-limits regardless of
// the grant shape. Mutates in place (called on a fresh deep copy).
func pruneForbidden(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if forbiddenKeys[k] || hardForbiddenSegments[k] {
				delete(t, k)
				continue
			}
			t[k] = pruneForbidden(t[k])
		}
		return t
	case []any:
		for i := range t {
			t[i] = pruneForbidden(t[i])
		}
		return t
	default:
		return v
	}
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = deepCopyValue(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = deepCopyValue(vv)
		}
		return s
	default:
		return v
	}
}

func getPath(m map[string]any, segs []string) (any, bool) {
	cur := any(m)
	for _, s := range segs {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := cm[s]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func setPath(m map[string]any, segs []string, val any) {
	cur := m
	for i, s := range segs {
		if i == len(segs)-1 {
			cur[s] = val
			return
		}
		next, ok := cur[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[s] = next
		}
		cur = next
	}
}

// ProjectConfig builds a FRESH object containing ONLY the visible dotted paths
// present in full — never a filtered pass over the original, so no unrelated
// category, field, or top-level key can leak on the wire. Missing paths are
// skipped, not errored.
func ProjectConfig(full map[string]any, visiblePaths []string) map[string]any {
	out := map[string]any{}
	for _, p := range visiblePaths {
		if p == "" {
			continue
		}
		segs := strings.Split(p, ".")
		// A visible path whose own last segment is forbidden is rejected at mint
		// (ValidatePaths); guard it here too so ProjectConfig is safe standalone.
		if last := segs[len(segs)-1]; forbiddenKeys[last] || hardForbiddenSegments[last] {
			continue
		}
		if v, ok := getPath(full, segs); ok {
			setPath(out, segs, pruneForbidden(deepCopyValue(v)))
		}
	}
	return out
}

// ApplyPatch validates a patch against allowedPaths (fail-closed) and
// deep-merges its leaves onto a COPY of full, leaving every unrelated field
// intact. Returns the merged config and the sorted changed dotted paths.
func ApplyPatch(full, patch map[string]any, allowedPaths []string) (map[string]any, []string, error) {
	allowed := make(map[string]bool, len(allowedPaths))
	for _, p := range allowedPaths {
		allowed[p] = true
	}
	leaves, err := collectPatchLeaves(patch, allowed)
	if err != nil {
		return nil, nil, err
	}
	merged, _ := deepCopyValue(full).(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	changed := make([]string, 0, len(leaves))
	for _, lf := range leaves {
		setPath(merged, lf.path, deepCopyValue(lf.value))
		changed = append(changed, strings.Join(lf.path, "."))
	}
	sort.Strings(changed)
	return merged, changed, nil
}
