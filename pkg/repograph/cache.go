package repograph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"

	"github.com/SocialGouv/iterion/internal/treeskip"
)

// CacheDir is where a built graph lives, relative to the repository
// root. It is under .iterion/, which this repo already gitignores: the
// small markdown maps are committed, a multi-megabyte graph is not.
const CacheDir = ".iterion/map"

const cacheFile = "graph.json"

// Load returns the cached graph when it still describes the tree, and
// rebuilds it otherwise. The second result says which happened, because
// "did that take two seconds because it rebuilt?" is the first question
// anyone asks.
//
// Staleness is decided by a fingerprint over every file the graph reads
// — path, size and modification time. Any difference rebuilds the WHOLE
// graph. Patching a graph in place would be faster and would risk the
// one failure this graph exists to avoid: describing a repository that
// no longer exists.
func Load(root string) (g *Graph, rebuilt bool, err error) {
	want, err := Fingerprint(root)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(root, CacheDir, cacheFile)
	if f, openErr := os.Open(path); openErr == nil {
		cached, readErr := Read(f)
		_ = f.Close()
		if readErr == nil && cached.Fingerprint == want {
			return cached, false, nil
		}
	}
	g, err = Build(root)
	if err != nil {
		return nil, false, err
	}
	g.Fingerprint = want
	if err := save(root, g); err != nil {
		return nil, true, err
	}
	return g, true, nil
}

func save(root string, g *Graph) error {
	dir := filepath.Join(root, CacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("repograph: create %s: %w", CacheDir, err)
	}
	tmp := filepath.Join(dir, cacheFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("repograph: create cache: %w", err)
	}
	if err := g.Write(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("repograph: close cache: %w", err)
	}
	// Rename last: a reader either sees the previous graph or the new
	// one, never a half-written file.
	if err := os.Rename(tmp, filepath.Join(dir, cacheFile)); err != nil {
		return fmt.Errorf("repograph: publish cache: %w", err)
	}
	return nil
}

// fingerprinted names every file type the BUILDER opens. A file the
// builder reads and the fingerprint ignores is a cache that reports
// "current" for a tree that changed — and the two that are easy to miss
// are not source files anyone would think to list: `CompileWorkflowPath`
// reads a bundle's `manifest.yaml` and probes its `.mcp.json`.
func fingerprinted(name string) bool {
	// The `.bot` door folds its case (#1762): a builder that reads
	// RUN.BOT must fingerprint it, or the cache reports "current" for a
	// tree that changed.
	if strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".md") ||
		workflowfile.IsWorkflowFile(name) {
		return true
	}
	return name == "go.mod" || name == "manifest.yaml" || name == ".mcp.json"
}

// Fingerprint hashes the CONTENT of everything the builder reads,
// outside the skipped trees. Sorted, so the digest depends on the tree
// and not on the order the filesystem happened to hand it over.
//
// Content, not (size, mtime). That cheaper key is blind to a same-size
// edit landing inside one tick of the kernel's coarse clock, which is
// not an exotic case — a codegen pass, a formatter or any script
// rewriting a file in place produces one, and this package's own test
// suite hit it by accident on its first run. Reading the ~4 600 files
// this covers costs ~70 ms against a ~700 ms rebuild: exactness is a
// tenth of what it protects, and a cache that is wrong is worth less
// than no cache at all.
func Fingerprint(root string) (string, error) {
	var lines []string
	err := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, abs)
			if rel != "." && treeskip.Dir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !fingerprinted(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(abs)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(body)
		lines = append(lines, filepath.ToSlash(rel)+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("repograph: fingerprint: %w", err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}
