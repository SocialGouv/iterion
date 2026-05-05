package cli

import "github.com/SocialGouv/iterion/pkg/internal/appinfo"

// Version returns the human-readable version string (e.g. "v1.2.3 (abc1234)").
// It is the public entry point for cmd/iterion (and cmd/iterion-desktop) to
// read version info without importing internal/appinfo directly — Go's
// internal-package rule forbids imports of pkg/internal/ from outside pkg/.
func Version() string {
	return appinfo.FullVersion()
}

// RawVersion returns the bare version string (e.g. "v1.2.3" or "dev").
// Used by the desktop About panel and the auto-updater to compare semver
// against the latest manifest.
func RawVersion() string { return appinfo.Version }

// RawCommit returns the bare commit string (short SHA, possibly empty).
func RawCommit() string { return appinfo.Commit }
