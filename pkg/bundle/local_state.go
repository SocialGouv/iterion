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

// LocalStateFile is the bundle's own record of what an install holds that
// its PRODUCER did not ship as it stands:
//
//   - a fragment materialized from `dependencies.fragments` (ADR-145
//     clause 2) under lib/ — a path the producer never shipped, which
//     [ContentHashDir] leaves out exactly as the producer's archive did;
//   - a skill document materialized from `dependencies.skills` (ADR-144
//     clause 2) under skills/ — the same ADDED shape, for the artefact kind
//     a `skills:` list names;
//   - the generated bot catalog of a materialized router bundle (ARB-109) —
//     a path the producer DOES ship, which the regeneration overwrites with
//     the consumer workspace's routing table. The producer's bytes are kept
//     aside under [KeptCopyDir] and [ContentHashDir] reads THEM under that
//     path, so the identity of the install stays the content it was pinned
//     at while the run reads the live table on disk.
//
// One record, two FORMS — a path the producer never shipped, and a path
// whose producer bytes are kept aside — and one shape per artefact kind
// inside them: a second registry would be a second truth about the same
// bundle. It exists because `bots.lock` pins what a producer
// published and [ContentHashDir] is the verdict every pin check reads.
// Without it an idempotent reconciliation moves that hash, and everything
// built on it reads a change nobody made: the post-install verification
// refuses the install outright, `bots sync`'s "already at its pin"
// short-circuit can never be true again, the bot:// resolver refuses the
// consumer for good, and the studio's dependency views report drift.
//
// It is local state, not a trust boundary: anything that can write a
// materialized fragment or a regenerated catalog can write this record
// beside it. What makes a pin trustworthy is the hash verified against the
// lock at fetch time, before any install. What the record can do is bounded
// twice: a path is honoured only while it matches one of the two shapes
// above AND the file still hashes to the digest recorded for it, and a kept
// copy is honoured only while ITS bytes hash to the digest recorded for
// them — so kept bytes that are not the producer's can make the hash move
// (loud drift, which a sync repairs by re-materializing), never make a
// modified install read as pristine.
//
// The file name is the one the record shipped with: renaming it would make
// every install that already carries one drift from its pin for no
// functional gain.
const LocalStateFile = ".iterion-materialized-fragments"

// LocalStateKind is the kind of local state an entry describes. The record
// stores no kind column: the SHAPE of a path is its kind
// ([localStateKindOf]), which is the same test that decides a recorded path
// is one this tree writes at all.
type LocalStateKind string

const (
	// LocalStateFragment is a `.bot` fragment an install materialized under
	// lib/ from `dependencies.fragments`.
	LocalStateFragment LocalStateKind = "fragment"
	// LocalStateSkill is a skill document an install materialized under
	// skills/ from `dependencies.skills`. Directly under it: the mirror
	// that hands skills to an agent reads that one directory, so a nested
	// path is a file no `skills:` reference resolves.
	LocalStateSkill LocalStateKind = "skill"
	// LocalStateCatalog is the generated bot catalog a regeneration wrote
	// into a materialized router bundle's skills/.
	LocalStateCatalog LocalStateKind = "catalog"
)

// GeneratedCatalogFile is the generated bot catalog a router bundle carries
// under [DirSkills] — rendered from the live manifests by pkg/botregistry,
// and the one file a regeneration replaces inside a materialized bundle. The
// spelling lives here because the bundle layout is this package's and the
// record's shape test reads it; pkg/botregistry names it from here.
const GeneratedCatalogFile = "iterion-bot-catalog.md"

// KeptCopyDir holds the producer bytes a local-state entry replaced. It sits
// under the control directory every walker of a bundle's content already
// skips ([IsPackSkipped]), so a kept copy reaches no archive, no content
// hash and no snapshot — and an install leaves it alone (pkg/botinstall
// keeps .iterion, the authoring lock's own home).
const KeptCopyDir = ".iterion/kept"

// LocalStateEntry is one line of the record: where the local file is in the
// bundle, what its bytes hash to, and — when it replaced a file the producer
// shipped — what the producer's kept bytes hash to.
type LocalStateEntry struct {
	// Path is bundle-relative and slash-separated.
	Path string
	// SHA256 is the hex digest of the bytes now at Path.
	SHA256 string
	// KeptSHA256 is the hex digest of the producer bytes this entry
	// replaced, held at [KeptPathFor](Path) — the digest the producer's own
	// tree contributed to its bundle hash. Empty for a path the producer
	// never shipped, which is every [LocalStateFragment].
	KeptSHA256 string
}

// KeptPathFor is where the producer's bytes of a replaced bundle path are
// kept. The record carries the digest, never the location: one function
// answers for the layout, so a recorded line cannot point the hash at a file
// outside the bundle.
func KeptPathFor(rel string) string { return KeptCopyDir + "/" + rel }

// localStateKindOf classifies a recorded path. A path that matches no shape
// belongs to no kind: the record describes what this tree writes, and
// nothing else, so an unknown path is refused rather than honoured (it would
// otherwise hide an arbitrary file from every pin check).
func localStateKindOf(path string) (LocalStateKind, bool) {
	if path == "" || strings.HasPrefix(path, "/") || path != filepath.ToSlash(filepath.Clean(path)) {
		return "", false
	}
	switch {
	case strings.HasPrefix(path, DirFragments+"/") && strings.HasSuffix(strings.ToLower(path), fragmentExtension):
		return LocalStateFragment, true
	// The catalog FIRST: its path is also a skill document under skills/,
	// and it is the one the regeneration owns with kept producer bytes.
	// Classifying it as a skill would hand one line to two writers.
	case path == DirSkills+"/"+GeneratedCatalogFile:
		return LocalStateCatalog, true
	case isMaterializedSkillPath(path):
		return LocalStateSkill, true
	}
	return "", false
}

// isMaterializedSkillPath is the shape of a resolved `dependencies.skills`
// entry: a skill document DIRECTLY under skills/, which is the only place
// the install writes one and the only place the run-start mirror reads.
func isMaterializedSkillPath(path string) bool {
	rel, ok := strings.CutPrefix(path, DirSkills+"/")
	if !ok || strings.Contains(rel, "/") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(rel), skillExtension)
}

// kindOf is [localStateKindOf] as a refusal, named with the record and the
// path so a caller reports what it could not classify.
func (e LocalStateEntry) kindOf() (LocalStateKind, error) {
	kind, ok := localStateKindOf(e.Path)
	if !ok {
		return "", fmt.Errorf("bundle: %s: %q is not a path an install writes (a %s fragment under %s/, or a %s skill directly under %s/, which includes %s/%s)", LocalStateFile, e.Path, fragmentExtension, DirFragments, skillExtension, DirSkills, DirSkills, GeneratedCatalogFile)
	}
	return kind, nil
}

const sha256HexLen = sha256.Size * 2

// WriteLocalStateOfKind replaces the entries of ONE kind in the record at
// dir and leaves every other kind exactly as it stands: the fragment
// resolution and the catalog regeneration write the same record, each owning
// its own lines, so neither erases the other's. A record left with no entry
// at all is removed, so a bundle whose local state is gone hashes as its
// producer shipped it rather than keeping a file nobody reads.
func WriteLocalStateOfKind(dir string, kind LocalStateKind, entries []LocalStateEntry) error {
	kept, err := ReadLocalState(dir)
	if err != nil {
		return err
	}
	merged := make([]LocalStateEntry, 0, len(kept)+len(entries))
	for _, e := range kept {
		existing, kindErr := e.kindOf()
		if kindErr != nil {
			return kindErr
		}
		if existing != kind {
			merged = append(merged, e)
		}
	}
	for _, e := range entries {
		got, kindErr := e.kindOf()
		if kindErr != nil {
			return kindErr
		}
		if got != kind {
			return fmt.Errorf("bundle: %s: %q is %s local state, not %s", LocalStateFile, e.Path, got, kind)
		}
		merged = append(merged, e)
	}
	return writeLocalState(dir, merged)
}

// writeLocalState replaces the whole record at dir.
func writeLocalState(dir string, entries []LocalStateEntry) error {
	path := filepath.Join(dir, LocalStateFile)
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("bundle: remove %s: %w", LocalStateFile, err)
		}
		return nil
	}
	sorted := make([]LocalStateEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	for _, e := range sorted {
		if err := writeLocalStateLine(&b, e); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("bundle: write %s: %w", LocalStateFile, err)
	}
	return nil
}

// writeLocalStateLine renders one entry. A path is rejected rather than
// quoted: every path the record carries is one this tree chose, and a line
// nobody can read back is a verdict with no basis.
func writeLocalStateLine(b *strings.Builder, e LocalStateEntry) error {
	if err := refuseUnrecordablePath(e.Path); err != nil {
		return err
	}
	if len(e.SHA256) != sha256HexLen {
		return fmt.Errorf("bundle: %s: %s has no sha256 digest to record", LocalStateFile, e.Path)
	}
	if e.KeptSHA256 == "" {
		fmt.Fprintf(b, "%s %s\n", e.SHA256, e.Path)
		return nil
	}
	if len(e.KeptSHA256) != sha256HexLen {
		return fmt.Errorf("bundle: %s: the copy kept for %s has no sha256 digest to record", LocalStateFile, e.Path)
	}
	fmt.Fprintf(b, "%s %s %s\n", e.SHA256, e.Path, e.KeptSHA256)
	return nil
}

func refuseUnrecordablePath(path string) error {
	if path == "" || strings.ContainsAny(path, " \t\n") {
		return fmt.Errorf("bundle: %s: path %q is not writable as a record line", LocalStateFile, path)
	}
	return nil
}

// ReadLocalState reads the record at dir. A bundle without one holds no
// local state, which is every bundle an install left exactly as its producer
// shipped it: the absent file is nil, nil. A record that exists and does not
// parse is an error — a hash computed over a record nobody could read would
// be a verdict with no basis.
//
// A line is `<sha256> <path>` for a path the producer never shipped, or
// `<sha256> <path> <kept sha256>` for one whose producer bytes are kept
// aside at [KeptPathFor].
func ReadLocalState(dir string) ([]LocalStateEntry, error) {
	raw, err := os.ReadFile(filepath.Join(dir, LocalStateFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bundle: read %s: %w", LocalStateFile, err)
	}
	var entries []LocalStateEntry
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry, parseErr := parseLocalStateLine(line)
		if parseErr != nil {
			return nil, fmt.Errorf("bundle: %s line %d: %w", LocalStateFile, n+1, parseErr)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// ReadLocalStateOfKind is [ReadLocalState] narrowed to one kind. The writer
// of a kind reconciles its OWN entries and must never read another kind's as
// its own: a sync's fragment pass would otherwise see a regenerated catalog
// as a fragment the manifest no longer declares, and refuse the sync over a
// file it does not own.
func ReadLocalStateOfKind(dir string, kind LocalStateKind) ([]LocalStateEntry, error) {
	entries, err := ReadLocalState(dir)
	if err != nil {
		return nil, err
	}
	var of []LocalStateEntry
	for _, e := range entries {
		got, kindErr := e.kindOf()
		if kindErr != nil {
			return nil, kindErr
		}
		if got == kind {
			of = append(of, e)
		}
	}
	return of, nil
}

func parseLocalStateLine(line string) (LocalStateEntry, error) {
	fields := strings.Fields(line)
	switch len(fields) {
	case 2:
		if err := refuseNotADigest(fields[0]); err != nil {
			return LocalStateEntry{}, err
		}
		return LocalStateEntry{Path: fields[1], SHA256: fields[0]}, nil
	case 3:
		if err := refuseNotADigest(fields[0]); err != nil {
			return LocalStateEntry{}, err
		}
		if err := refuseNotADigest(fields[2]); err != nil {
			return LocalStateEntry{}, err
		}
		return LocalStateEntry{Path: fields[1], SHA256: fields[0], KeptSHA256: fields[2]}, nil
	}
	return LocalStateEntry{}, fmt.Errorf("want %q or %q, got %q", "<sha256> <path>", "<sha256> <path> <kept sha256>", line)
}

func refuseNotADigest(field string) error {
	if len(field) != sha256HexLen {
		return fmt.Errorf("%q is not a sha256 digest", field)
	}
	if _, err := hex.DecodeString(field); err != nil {
		return fmt.Errorf("%q is not a sha256 digest", field)
	}
	return nil
}

// localStateOverrides is how [collectEntries] honours the record: the
// bundle-relative paths it must leave out, and the paths whose KEPT bytes it
// must read in place of the ones on disk (keyed by path, valued with the
// absolute path of the kept copy). Both are empty for a bundle with no
// record, which is the common case and costs one failed open.
//
// An entry is honoured only while it describes the install exactly:
//
//   - the recorded path must still read as the digest recorded for it. It
//     does not — edited after the install, gone, a directory now: the entry
//     is honoured as nothing, so the walk hashes what is actually there (or
//     fails on it in its own terms) and the hash MOVES. Drift is the honest
//     answer and the one a pin check must see: what is on disk is not what
//     any producer shipped, and `bots sync` repairs it by re-materializing.
//   - a kept copy must still be a regular file hashing to ITS recorded
//     digest. It does not: the replacement cannot be undone for the hash, so
//     the live bytes are hashed and the install reads as drifted too.
//
// Nothing here refuses the bundle: a refusal would turn a tidied skills/ or
// a cleared .iterion into an unreadable install at sixteen call sites, where
// the question every one of them asks — "is this still what the lock pinned?"
// — has a true answer: no.
func localStateOverrides(dir string) (skip map[string]bool, kept map[string]string, err error) {
	entries, err := ReadLocalState(dir)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	skip = map[string]bool{LocalStateFile: true}
	for _, e := range entries {
		if _, kindErr := e.kindOf(); kindErr != nil {
			return nil, nil, kindErr
		}
		digest, digestErr := FileSHA256(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if digestErr != nil || digest != e.SHA256 {
			continue
		}
		if e.KeptSHA256 == "" {
			skip[e.Path] = true
			continue
		}
		keptAbs := filepath.Join(dir, filepath.FromSlash(KeptPathFor(e.Path)))
		info, statErr := os.Lstat(keptAbs)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		keptDigest, keptErr := FileSHA256(keptAbs)
		if keptErr != nil || keptDigest != e.KeptSHA256 {
			continue
		}
		if kept == nil {
			kept = make(map[string]string, len(entries))
		}
		kept[e.Path] = keptAbs
	}
	return skip, kept, nil
}

// FileSHA256 is the hex digest of a file's bytes — the unit the local-state
// record stores, and what a caller that writes such a file reports back.
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
