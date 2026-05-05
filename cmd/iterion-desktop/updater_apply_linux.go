//go:build desktop && linux

package main

import (
	"bytes"
	"encoding/hex"
	"os"

	selfupdate "github.com/creativeprojects/go-selfupdate/update"
)

// applyArtifact replaces the running AppImage in place using go-selfupdate's
// cross-platform safe binary swap machinery. DownloadAndApply has already
// verified the Ed25519 signature over the artefact bytes; passing the SHA256
// checksum here keeps go-selfupdate's on-disk replacement path guarded against
// accidental corruption as well.
func applyArtifact(body []byte, rel *Release) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	checksum, err := hex.DecodeString(rel.SHA256)
	if err != nil {
		return err
	}
	return selfupdate.Apply(bytes.NewReader(body), selfupdate.Options{
		TargetPath: exe,
		TargetMode: 0o755,
		Checksum:   checksum,
	})
}
