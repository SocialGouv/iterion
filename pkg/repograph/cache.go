package repograph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// Fingerprint hashes the shape of everything the builder reads: every
// .go, .md and .bot file outside the skipped trees, with its size and
// modification time. Sorted, so the digest depends on the tree and not
// on the order the filesystem happened to hand it over.
func Fingerprint(root string) (string, error) {
	var lines []string
	err := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, abs)
			if rel != "." && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".md") &&
			!strings.HasSuffix(name, ".bot") && name != "go.mod" {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		lines = append(lines, filepath.ToSlash(rel)+" "+
			strconv.FormatInt(info.Size(), 10)+" "+
			strconv.FormatInt(info.ModTime().UnixNano(), 10))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("repograph: fingerprint: %w", err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}
