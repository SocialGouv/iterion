package model

import "strings"

// ProviderlessModelID strips the leading "<provider>/" off a claw model
// spec. claw's GenerationOptions.Model wants the bare model id while
// Registry.Resolve wants the full "<provider>/<model>" spec — direct
// structured callers (supervisors, the session-board curator) resolve the
// client from the full spec but pass the bare id on the request. Returns
// the input unchanged when it carries no provider prefix.
func ProviderlessModelID(spec string) string {
	if i := strings.Index(spec, "/"); i >= 0 && i+1 < len(spec) {
		return spec[i+1:]
	}
	return spec
}
