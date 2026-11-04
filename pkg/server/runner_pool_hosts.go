package server

import "github.com/SocialGouv/iterion/pkg/platformcfg"

// Warnings are a derived response field; the stored registry and its shape
// remain unchanged, including a nil record's null response.
func runnerPoolRegistryView(r *platformcfg.RunnerPools) any {
	if r == nil {
		return nil
	}
	return struct {
		*platformcfg.RunnerPools
		Warnings []platformcfg.PoolHostWarning `json:"warnings,omitempty"`
	}{r, r.HostWarnings()}
}
