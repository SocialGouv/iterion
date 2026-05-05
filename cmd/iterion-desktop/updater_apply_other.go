//go:build desktop && !linux && !darwin && !windows

package main

import (
	"fmt"
	goruntime "runtime"
)

// applyArtifact is the catch-all stub used on platforms where we don't
// ship a desktop binary (FreeBSD, NetBSD, OpenBSD, …). The auto-updater
// will surface the error to the UI; we don't currently produce a binary
// for these platforms anyway.
func applyArtifact(_ []byte, _ *Release) error {
	return fmt.Errorf("updater: unsupported GOOS %q", goruntime.GOOS)
}
