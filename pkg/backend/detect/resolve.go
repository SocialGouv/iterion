package detect

// Resolve picks the first backend in prefOrder that has Available=true
// in the given backends list. Returns "" when nothing matches.
func Resolve(prefOrder []string, backends []BackendStatus) string {
	avail := map[string]bool{}
	for _, b := range backends {
		if b.Available {
			avail[b.Name] = true
		}
	}
	for _, name := range prefOrder {
		if avail[name] {
			return name
		}
	}
	return ""
}

// SuggestedModel returns the suggested model spec for a given backend,
// based on the providers currently available. Returns "" when the backend
// is CLI-managed (claude_code / codex) or no provider matches.
func SuggestedModel(backend string, providers []ProviderStatus) string {
	if backend != BackendClaw {
		return ""
	}
	for _, p := range providers {
		if p.Available && p.SuggestedModel != "" {
			return p.SuggestedModel
		}
	}
	return ""
}
