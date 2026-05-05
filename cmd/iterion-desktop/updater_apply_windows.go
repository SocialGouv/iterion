//go:build desktop && windows

package main

import (
	"bytes"
	"encoding/hex"
	"os"

	selfupdate "github.com/creativeprojects/go-selfupdate/update"
)

// applyArtifact writes and swaps the running .exe using go-selfupdate's safe
// replacement flow (new file, old-file backup, rollback on failure, Windows
// old-file handling). DownloadAndApply has already verified the Ed25519
// artefact signature; the checksum is supplied to go-selfupdate as a second
// guard during replacement.
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
