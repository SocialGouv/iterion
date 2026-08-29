// Package botlock loads the project-root lockfile for shared bot bundles.
package botlock

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v2"
)

const (
	FileName       = "bots.lock"
	CurrentVersion = 1
)

var (
	dependencyNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sha256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Lock is the committed, project-wide resolution of manifest dependencies.
type Lock struct {
	Version      int                   `yaml:"version" json:"version"`
	Dependencies map[string]Dependency `yaml:"dependencies" json:"dependencies"`
}

// Dependency pins one bundle source to an immutable logical content hash.
type Dependency struct {
	Source       string `yaml:"source" json:"source"`
	Ref          string `yaml:"ref" json:"ref"`
	Path         string `yaml:"path,omitempty" json:"path,omitempty"`
	BundleSHA256 string `yaml:"bundle_sha256" json:"bundle_sha256"`
}

// Load reads <workdir>/bots.lock using strict YAML decoding.
func Load(workdir string) (*Lock, error) {
	path := filepath.Join(workdir, FileName)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bot dependencies: read %s: %w", path, err)
	}
	var lock Lock
	if err := yaml.UnmarshalStrict(body, &lock); err != nil {
		return nil, fmt.Errorf("bot dependencies: parse %s: %w", path, err)
	}
	if err := lock.Validate(); err != nil {
		return nil, fmt.Errorf("bot dependencies: %s: %w", path, err)
	}
	return &lock, nil
}

// Save atomically replaces <workdir>/bots.lock after validating the complete
// v1 document. The temporary file lives beside the lock so Rename remains an
// atomic same-filesystem operation.
func Save(workdir string, lock *Lock) error {
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("bot dependencies: %s: %w", filepath.Join(workdir, FileName), err)
	}
	body, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("bot dependencies: encode %s: %w", filepath.Join(workdir, FileName), err)
	}
	path := filepath.Join(workdir, FileName)
	tmp, err := os.CreateTemp(workdir, ".bots.lock-*")
	if err != nil {
		return fmt.Errorf("bot dependencies: create temporary lock: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bot dependencies: write temporary lock: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bot dependencies: sync temporary lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bot dependencies: close temporary lock: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("bot dependencies: replace %s: %w", path, err)
	}
	return nil
}

// Validate enforces the lockfile's closed v1 contract.
func (l *Lock) Validate() error {
	if l == nil {
		return fmt.Errorf("lock is nil")
	}
	if l.Version != CurrentVersion {
		return fmt.Errorf("version %d is not supported (expected %d)", l.Version, CurrentVersion)
	}
	for name, dep := range l.Dependencies {
		if !dependencyNamePattern.MatchString(name) {
			return fmt.Errorf("dependency name %q is invalid", name)
		}
		if strings.TrimSpace(dep.Source) == "" {
			return fmt.Errorf("dependency %q: source is required", name)
		}
		if strings.TrimSpace(dep.Ref) == "" {
			return fmt.Errorf("dependency %q: ref is required and must be pinned", name)
		}
		if dep.Path == "." || filepath.IsAbs(dep.Path) || strings.HasPrefix(filepath.ToSlash(filepath.Clean(dep.Path)), "../") {
			return fmt.Errorf("dependency %q: path %q must stay inside its source repository", name, dep.Path)
		}
		if !sha256Pattern.MatchString(dep.BundleSHA256) {
			return fmt.Errorf("dependency %q: bundle_sha256 must be 64 lowercase hexadecimal characters", name)
		}
	}
	return nil
}
