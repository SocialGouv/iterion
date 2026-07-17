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
		if v, ok := getPath(full, segs); ok {
			setPath(out, segs, deepCopyValue(v))
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
