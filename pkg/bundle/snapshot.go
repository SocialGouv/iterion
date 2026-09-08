package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Snapshot freezes a bundle collection at launch. Every file is below a
// collection-relative bundle name, so sibling subbots retain their paths.
// Bytes and executable bits survive transport; symlinks never cross it.
type Snapshot struct {
	Root  string                  `json:"root"`
	Files map[string]SnapshotFile `json:"files"`
}

type SnapshotFile struct {
	Content    []byte `json:"content"`
	Executable bool   `json:"executable,omitempty"`
}

const MaxSnapshotBytes = 32 << 20
const MaxSnapshotFiles = 4096

func snapshotName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

func snapshotPath(s string) bool {
	return s != "" && path.Clean(s) == s && !path.IsAbs(s) &&
		!strings.ContainsAny(s, "\\\x00") && !strings.HasPrefix(s, "../") &&
		strings.Contains(s, "/")
}

// AddDir captures one authoritative bundle, omitting the same generated trees
// as botsource.ReadBundleDir. It refuses aliases to files outside the bundle.
func (s *Snapshot) AddDir(name, dir string) error {
	if !snapshotName(name) {
		return fmt.Errorf("bundle snapshot: unsafe bundle name %q", name)
	}
	if s.Files == nil {
		s.Files = map[string]SnapshotFile{}
	}
	return filepath.WalkDir(dir, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle snapshot: symlink %s is not portable", p)
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".devbox", ".iterion", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundle snapshot: non-regular file %s", p)
		}
		if info.Size() > MaxSnapshotBytes {
			return fmt.Errorf("bundle snapshot: oversized file %s", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		key := name + "/" + filepath.ToSlash(rel)
		if !snapshotPath(key) {
			return fmt.Errorf("bundle snapshot: unsafe path %q", key)
		}
		if _, exists := s.Files[key]; exists {
			return fmt.Errorf("bundle snapshot: duplicate file %s", key)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s.Files[key] = SnapshotFile{Content: content, Executable: info.Mode()&0o111 != 0}
		return s.checkLimits()
	})
}

func (s *Snapshot) checkLimits() error {
	if len(s.Files) > MaxSnapshotFiles {
		return fmt.Errorf("bundle snapshot: exceeds %d files", MaxSnapshotFiles)
	}
	total := 0
	for p, f := range s.Files {
		total += len(p) + len(f.Content)
	}
	if total > MaxSnapshotBytes {
		return fmt.Errorf("bundle snapshot: exceeds %d bytes", MaxSnapshotBytes)
	}
	return nil
}

func (s *Snapshot) Validate() error {
	if !snapshotName(s.Root) {
		return fmt.Errorf("bundle snapshot: unsafe root %q", s.Root)
	}
	if len(s.Files[s.Root+"/main.bot"].Content) == 0 {
		return fmt.Errorf("bundle snapshot: parent main.bot is missing")
	}
	for p := range s.Files {
		if !snapshotPath(p) {
			return fmt.Errorf("bundle snapshot: unsafe path %q", p)
		}
		// A file cannot also be an ancestor directory of another file.
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if _, ok := s.Files[parent]; ok {
				return fmt.Errorf("bundle snapshot: file/directory collision at %s", parent)
			}
		}
	}
	return s.checkLimits()
}

func (s *Snapshot) Encode() ([]byte, string, error) {
	if err := s.Validate(); err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:]), nil
}

func DecodeSnapshot(body []byte, digest string) (*Snapshot, error) {
	if len(body) > MaxSnapshotBytes*2 {
		return nil, fmt.Errorf("bundle snapshot: encoded size exceeds limit")
	}
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != digest {
		return nil, fmt.Errorf("bundle snapshot: digest mismatch")
	}
	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("bundle snapshot: decode: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Materialize returns the parent bundle directory and cleanup for the WHOLE
// collection. Every path is validated before touching the private temp tree.
func (s *Snapshot) Materialize() (string, func(), error) {
	if err := s.Validate(); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "iterion-bundle-snapshot-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for rel, f := range s.Files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			cleanup()
			return "", nil, err
		}
		mode := os.FileMode(0o644)
		if f.Executable {
			mode = 0o755
		}
		if err := os.WriteFile(p, f.Content, mode); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return filepath.Join(dir, s.Root), cleanup, nil
}

// ResolveSnapshotSource never falls back to a pod's independently baked catalog.
func ResolveSnapshotSource(source, parentDir, collectionDir string) (string, error) {
	if filepath.IsAbs(source) || strings.ContainsAny(source, "\\\x00") {
		return "", fmt.Errorf("bundle snapshot: unsafe child source %q", source)
	}
	p := filepath.Join(parentDir, source)
	rel, err := filepath.Rel(collectionDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bundle snapshot: child %q escapes the collection", source)
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", fmt.Errorf("bundle snapshot: child %q is absent: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("bundle snapshot: child %q is not a regular file", source)
	}
	// Re-check real paths as a running tool can modify a temporary directory.
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(collectionDir)
	if err != nil {
		return "", err
	}
	rel, err = filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bundle snapshot: child %q escapes through a symlink", source)
	}
	return p, nil
}
