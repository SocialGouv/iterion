package cli

import "github.com/SocialGouv/iterion/pkg/internal/appinfo"

// Version returns the human-readable version string (e.g. "v1.2.3 (abc1234)").
// It is the public entry point for cmd/iterion to read version info without
// importing internal/appinfo directly.
func Version() string {
	return appinfo.FullVersion()
}
