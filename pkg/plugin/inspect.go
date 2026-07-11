package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoManifest is returned by Inspect when the source directory has no
// plugin.yaml. Callers branch on it with errors.Is (e.g. the marketplace
// submit flow falls back to skill-library synthesis).
var ErrNoManifest = errors.New("plugin: no plugin.yaml")

// InspectInfo is what Inspect reads out of a plugin source directory before
// any install: the validated manifest plus the README body (empty when the
// source ships none).
type InspectInfo struct {
	Manifest *Manifest
	README   string
}

// Inspect reads and validates a plugin source directory without installing
// it: parses <srcDir>/plugin.yaml (a missing manifest reports ErrNoManifest)
// and reads the README via ReadReadme. The context is accepted for API
// symmetry with Install/InstallWith (a future git-source inspect will need
// it); local inspection does no blocking work beyond file reads.
func Inspect(ctx context.Context, srcDir string) (*InspectInfo, error) {
	_ = ctx
	data, err := os.ReadFile(filepath.Join(srcDir, ManifestFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w in %q", ErrNoManifest, srcDir)
		}
		return nil, fmt.Errorf("plugin: read %s in %q: %w", ManifestFile, srcDir, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, err
	}
	readme, err := ReadReadme(srcDir)
	if err != nil {
		return nil, err
	}
	return &InspectInfo{Manifest: m, README: readme}, nil
}

// readmeCap bounds the README body returned by ReadReadme so a pathological
// file cannot bloat a studio detail response.
const readmeCap = 16 * 1024

// ReadReadme returns the README body from dir, matching the filename
// case-insensitively (README.md / readme.md / Readme.md, …) and capping the
// content at 16 KiB. A directory with no README yields an empty string, not
// an error; an unreadable directory or file is an error.
func ReadReadme(dir string) (string, error) {
	return readReadmeFS(os.DirFS(dir))
}

// readReadmeFS is ReadReadme over an fs.FS root, so builtin plugins (embedded
// FS) and installed plugins (os.DirFS) share the same lookup.
func readReadmeFS(fsys fs.FS) (string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return "", fmt.Errorf("plugin: read readme dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(e.Name(), "README.md") {
			continue
		}
		data, rerr := fs.ReadFile(fsys, e.Name())
		if rerr != nil {
			return "", fmt.Errorf("plugin: read %s: %w", e.Name(), rerr)
		}
		if len(data) > readmeCap {
			data = data[:readmeCap]
		}
		return string(data), nil
	}
	return "", nil
}
