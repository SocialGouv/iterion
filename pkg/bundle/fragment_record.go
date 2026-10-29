package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaterializedFragmentsFile is the bundle's own note of the fragments an
// install materialized from `dependencies.fragments` (ADR-145 clause 2):
// files that live under lib/, that every reader of the unit must see, and
// that the PRODUCER never shipped.
//
// It exists because `bots.lock` pins what a producer published, and
// [ContentHashDir] is the verdict every pin check reads. Without this note a
// resolved dependency would move that hash, and the mechanisms built on it
// would read an idempotent reconciliation as drift: the bot:// resolver
// would refuse the consumer for good, `bots sync`'s "already at its pin"
// short-circuit could never be true again, and the studio's dependency
// views would report a change nobody made.
//
// It is local state, not a trust boundary: anything that can write a
// materialized fragment can write this note beside it. What makes the pin
// trustworthy is the hash verified against the lock at fetch time, before
// any install. The note's job is to say which files are not the producer's,
// and [ContentHashDir] honours it only for fragments it still describes
// exactly — a path under lib/ whose bytes match the recorded digest.
const MaterializedFragmentsFile = ".iterion-materialized-fragments"

// MaterializedFragment is one line of the note: where the fragment landed in
// the bundle, and the digest of the bytes that landed.
type MaterializedFragment struct {
	// Path is bundle-relative and slash-separated, under lib/.
	Path string
	// SHA256 is the hex digest of the materialized bytes.
	SHA256 string
}

// WriteMaterializedFragments replaces the note at dir. An empty list removes
// it, so a bundle whose dependencies were all removed goes back to hashing
// as the producer shipped it rather than keeping an empty file nobody reads.
func WriteMaterializedFragments(dir string, frags []MaterializedFragment) error {
	path := filepath.Join(dir, MaterializedFragmentsFile)
	if len(frags) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("bundle: remove %s: %w", MaterializedFragmentsFile, err)
		}
		return nil
	}
	sorted := make([]MaterializedFragment, len(frags))
	copy(sorted, frags)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	for _, f := range sorted {
		if strings.ContainsAny(f.Path, " \t\n") || f.Path == "" {
			return fmt.Errorf("bundle: %s: fragment path %q is not writable as a record line", MaterializedFragmentsFile, f.Path)
		}
		if len(f.SHA256) != sha256HexLen {
			return fmt.Errorf("bundle: %s: fragment %s has no sha256 digest to record", MaterializedFragmentsFile, f.Path)
		}
		fmt.Fprintf(&b, "%s %s\n", f.SHA256, f.Path)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("bundle: write %s: %w", MaterializedFragmentsFile, err)
	}
	return nil
}

const sha256HexLen = sha256.Size * 2

// ReadMaterializedFragments reads the note at dir. A bundle without one
// carries no materialized fragment, which is every bundle that declares no
// `dependencies.fragments`: the absent file is nil, nil. A note that exists
// and does not parse is an error — a hash computed over a record nobody
// could read would be a verdict with no basis.
func ReadMaterializedFragments(dir string) ([]MaterializedFragment, error) {
	raw, err := os.ReadFile(filepath.Join(dir, MaterializedFragmentsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bundle: read %s: %w", MaterializedFragmentsFile, err)
	}
	var frags []MaterializedFragment
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		digest, rel, ok := strings.Cut(line, " ")
		if !ok || len(digest) != sha256HexLen || rel == "" {
			return nil, fmt.Errorf("bundle: %s line %d: want %q, got %q", MaterializedFragmentsFile, n+1, "<sha256> <path>", line)
		}
		if _, decErr := hex.DecodeString(digest); decErr != nil {
			return nil, fmt.Errorf("bundle: %s line %d: %q is not a sha256 digest", MaterializedFragmentsFile, n+1, digest)
		}
		frags = append(frags, MaterializedFragment{Path: rel, SHA256: digest})
	}
	return frags, nil
}

// materializedFragmentExclusions is the set of bundle-relative paths
// [ContentHashDir] leaves out: the note itself, plus every fragment the note
// still describes exactly. A listed path is honoured only when it sits under
// lib/, names a fragment, and its bytes hash to the recorded digest — so the
// note can hide the files it was written for and nothing else.
func materializedFragmentExclusions(dir string) (map[string]bool, error) {
	frags, err := ReadMaterializedFragments(dir)
	if err != nil {
		return nil, err
	}
	if len(frags) == 0 {
		return nil, nil
	}
	exclusions := make(map[string]bool, len(frags)+1)
	exclusions[MaterializedFragmentsFile] = true
	for _, f := range frags {
		if !strings.HasPrefix(f.Path, DirFragments+"/") || !strings.HasSuffix(strings.ToLower(f.Path), fragmentExtension) {
			return nil, fmt.Errorf("bundle: %s: %q is not a %s fragment under %s/", MaterializedFragmentsFile, f.Path, fragmentExtension, DirFragments)
		}
		if strings.Contains(f.Path, "../") {
			return nil, fmt.Errorf("bundle: %s: %q escapes the bundle", MaterializedFragmentsFile, f.Path)
		}
		digest, digestErr := FileSHA256(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if digestErr != nil {
			return nil, fmt.Errorf("bundle: %s: %s: %w", MaterializedFragmentsFile, f.Path, digestErr)
		}
		if digest != f.SHA256 {
			// The note no longer describes the file: the bytes were edited
			// after the install. Hashing it back IN is the honest answer —
			// the bundle on disk is not what any producer shipped, and the
			// pin check must see that.
			continue
		}
		exclusions[f.Path] = true
	}
	return exclusions, nil
}

// FileSHA256 is the hex digest of a file's bytes — the unit the
// materialized-fragment note records, and what a caller that writes such a
// fragment reports back.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SHA256Bytes is [FileSHA256] for bytes already in hand.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
