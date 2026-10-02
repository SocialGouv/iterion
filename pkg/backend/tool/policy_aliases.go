package tool

import "fmt"

func patternsMatchContext(patterns []string, ctx PolicyContext) (bool, error) {
	matched := false
	for _, pattern := range patterns {
		name := ctx.ToolName
		if ctx.ResolvePattern != nil {
			resolved, err := ctx.ResolvePattern(pattern)
			if err != nil {
				return false, fmt.Errorf("%w: policy reference %q: %v", ErrToolDenied, pattern, err)
			}
			if resolved != pattern && ctx.QualifiedToolName != "" {
				name = ctx.QualifiedToolName
			}
			pattern = resolved
		}
		matched = matchPattern(pattern, name) || matched
	}
	return matched, nil
}
