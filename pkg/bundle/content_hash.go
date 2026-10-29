package bundle

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ContentHashDir computes the same stable logical-content SHA-256 as PackDir
// and Bundle.Hash, without first creating an archive. Pack exclusions, the
// draft rule (IsDraftEntry), the materialized-fragment rule
// ([MaterializedFragmentsFile]) and irregular-file checks are deliberately
// shared with the packer.
//
// What it hashes is the content the bundle's PRODUCER published, which is
// what a lockfile pins: a fragment an install materialized from
// `dependencies.fragments` lives under lib/ for every reader of the unit,
// and is excluded here, so resolving a declared dependency is not read as
// drift by the pin checks built on this verdict.
func ContentHashDir(dir string) (string, error) {
	entries, _, _, err := collectEntries(dir, true)
	if err != nil {
		return "", err
	}
	hasher := newContentHasher()
	for _, entry := range entries {
		if entry.isDir {
			continue
		}
		hasher.AddFile(entry.rel)
		f, openErr := os.Open(entry.absPath)
		if openErr != nil {
			return "", fmt.Errorf("bundle: hash open %s: %w", entry.rel, openErr)
		}
		_, copyErr := io.Copy(hasher, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("bundle: hash read %s: %w", entry.rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("bundle: hash close %s: %w", entry.rel, closeErr)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
