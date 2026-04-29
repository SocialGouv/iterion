package hooks

// MergeConfigs merges hook configs from multiple scopes.
// Later configs (higher priority) extend earlier ones with dedup.
// Local > Project > User ordering.
func MergeConfigs(configs ...HookConfig) HookConfig {
	var result HookConfig
	result.PreToolUse = mergeCommandLists(configs, func(c HookConfig) []string { return c.PreToolUse })
	result.PostToolUse = mergeCommandLists(configs, func(c HookConfig) []string { return c.PostToolUse })
	result.PostToolUseFailure = mergeCommandLists(configs, func(c HookConfig) []string { return c.PostToolUseFailure })
	return result
}

// mergeCommandLists merges command lists from multiple configs with dedup.
func mergeCommandLists(configs []HookConfig, getter func(HookConfig) []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, cfg := range configs {
		for _, cmd := range getter(cfg) {
			if _, ok := seen[cmd]; !ok {
				seen[cmd] = struct{}{}
				result = append(result, cmd)
			}
		}
	}
	return result
}
