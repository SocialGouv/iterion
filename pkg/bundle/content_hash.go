package bundle

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ContentHashDir computes the same stable logical-content SHA-256 as PackDir
// and Bundle.Hash, without first creating an archive. Pack exclusions and
// irregular-file checks are deliberately shared with the packer.
func ContentHashDir(dir string) (string, error) {
	entries, _, err := collectEntries(dir)
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
