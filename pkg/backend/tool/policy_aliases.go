package tool

import "fmt"

func patternsMatchContext(patterns []string, ctx PolicyContext) (bool, error) {
	matched := false
	for _, pattern := range patterns {
		if ctx.ResolvePattern != nil {
			resolved, err := ctx.ResolvePattern(pattern)
			if err != nil {
				return false, fmt.Errorf("%w: policy reference %q: %v", ErrToolDenied, pattern, err)
			}
			pattern = resolved
		}
		matched = matchPattern(pattern, ctx.ToolName) || matched
	}
	return matched, nil
}
