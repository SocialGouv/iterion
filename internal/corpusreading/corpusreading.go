// Package corpusreading reads the committed corpus the way internal/mdcode's
// four first-party readers do — every tracked .md, .yaml and .yml outside the
// skipped trees that git does not mark linguist-generated, hashed under
// mdcode.Mask and mdcode.FenceScanner.Code — and
// keeps the golden internal/mdcode's witness pins it against. One
// implementation serves the witness, `task corpus:reading`, and the release
// cut, which re-pins its own CHANGELOG.md and chart edits from release-it's
// before:git:beforeRelease hook so the commit pins what it ships.
package corpusreading

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/internal/mdcode"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// Golden is the pinned reading, relative to the repository root.
const Golden = "internal/mdcode/testdata/committed-reading.txt"

// goldenPrefix keeps the golden's paths in the form its witness has always
// written: relative to internal/mdcode, the directory the test runs in.
const goldenPrefix = "../../"

// skippedDirs are never walked, wherever they sit: vendored and installed
// trees, sibling worktrees and clones, run state, test fixtures, and the
// dispatcher catalogue's sync output (pkg/cli/templates/dispatch_bots/,
// spliced from bots/ at build time).
var skippedDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, ".repos": true,
	".works": true, ".claude": true, ".iterion": true, "testdata": true,
	"dispatch_bots": true,
}

// Entry is one file's reading.
type Entry struct {
	Path string   // slash-separated, relative to the repository root
	Mask [32]byte // sha256 of mdcode.Mask over the body
	Code [32]byte // sha256 of the per-line FenceScanner.Code verdicts, '1' or '0'
}

// Reading is the entry's golden form: the two hashes, hex, space-separated.
func (e Entry) Reading() string {
	return fmt.Sprintf("%x %x", e.Mask, e.Code)
}

// inCorpus reports whether a root-relative slash path has a pinned extension
// and sits under no skipped tree.
func inCorpus(rel string) bool {
	switch filepath.Ext(rel) {
	case ".md", ".yaml", ".yml":
	default:
		return false
	}
	dirs := strings.Split(rel, "/")
	for _, dir := range dirs[:len(dirs)-1] {
		if skippedDirs[dir] {
			return false
		}
	}
	return true
}

// Read returns the reading of every tracked .md, .yaml and .yml under root
// outside the skipped trees, in filepath.WalkDir order — the order the golden
// is written in. A file the index does not carry is not part of the committed
// corpus: the release cut runs scripts/build.sh before its hook, and a plain
// filesystem walk would pin whatever the build or a local draft left behind.
func Read(root string) ([]Entry, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the corpus root: %w", err)
	}
	tracked, err := gitPaths(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rel := range tracked {
		if inCorpus(rel) {
			candidates = append(candidates, rel)
		}
	}
	generated, err := generatedPaths(root, candidates)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !inCorpus(rel) || !tracked[rel] || generated[rel] {
			return nil
		}
		if strings.ContainsAny(rel, "\n\r") {
			return fmt.Errorf("%q cannot be pinned: the golden is line-oriented", rel)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, readingOf(rel, body))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk the corpus under %s: %w", root, err)
	}
	return out, nil
}

// gitPaths runs a git listing under root that prints NUL-separated paths and
// returns them as a set.
func gitPaths(root string, args ...string) (map[string]bool, error) {
	cmd := exec.Command("git", gitlib.NoRunHooks(append([]string{"-C", root}, args...)...)...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s in %s: %v: %s", strings.Join(args, " "), root, err, strings.TrimSpace(stderr.String()))
	}
	set := map[string]bool{}
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set, nil
}

// generatedPaths returns the candidates git marks linguist-generated in
// .gitattributes (set, or =true). A generated file's reading moves with every
// regeneration — the repository maps move on nearly every landing — so a pin
// on it pins churn, not a reader's verdict, and makes every change that
// regenerates the file collide with the next one on the same golden line in
// the merge queue. The declaration lives in .gitattributes, where GitHub
// reads it as well.
func generatedPaths(root string, candidates []string) (map[string]bool, error) {
	generated := map[string]bool{}
	if len(candidates) == 0 {
		return generated, nil
	}
	cmd := exec.Command("git", gitlib.NoRunHooks("-C", root, "check-attr", "-z", "--stdin", "linguist-generated")...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	cmd.Stdin = strings.NewReader(strings.Join(candidates, "\x00") + "\x00")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git check-attr linguist-generated in %s: %v: %s", root, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return generated, nil
	}
	// -z prints path, attribute and value, each NUL-terminated.
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("git check-attr in %s answered %d field(s), not path/attribute/value triplets", root, len(fields))
	}
	for i := 0; i+2 < len(fields); i += 3 {
		switch fields[i+2] {
		case "set", "true":
			generated[fields[i]] = true
		}
	}
	return generated, nil
}

func readingOf(path string, body []byte) Entry {
	var f mdcode.FenceScanner
	var code []byte
	for _, line := range mdcode.Lines(string(body)) {
		if f.Code(line) {
			code = append(code, '1')
		} else {
			code = append(code, '0')
		}
	}
	return Entry{
		Path: path,
		Mask: sha256.Sum256([]byte(mdcode.Mask(string(body)))),
		Code: sha256.Sum256(code),
	}
}

// Format renders entries as the golden: one "mask code path" line per file.
func Format(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s%s\n", e.Reading(), goldenPrefix, e.Path)
	}
	return b.Bytes()
}

// Parse reads a golden into path → reading, keyed like Entry.Path. A line is
// two 64-digit hex hashes and a path, which may itself hold spaces; a path
// pinned twice is refused, since the second pin would silently win.
func Parse(data []byte) (map[string]string, error) {
	out := map[string]string{}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return out, nil
	}
	for _, line := range strings.Split(text, "\n") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || !isHash(parts[0]) || !isHash(parts[1]) {
			return nil, fmt.Errorf("golden line %q is not two hashes and a path", line)
		}
		path, ok := strings.CutPrefix(parts[2], goldenPrefix)
		if !ok || path == "" {
			return nil, fmt.Errorf("golden path %q does not start with %q", parts[2], goldenPrefix)
		}
		if _, dup := out[path]; dup {
			return nil, fmt.Errorf("golden pins %s twice", path)
		}
		out[path] = parts[0] + " " + parts[1]
	}
	return out, nil
}

func isHash(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Options configures Run. Without Write or Cut the run only checks.
type Options struct {
	Root string // repository root

	// Write rewrites the golden over the whole corpus: the gesture of a
	// change that edits a pinned file, reviewed with its diff. A golden that
	// no longer parses — a merge conflict in it — is rebuilt from the corpus.
	Write bool

	// Cut is the release cut's rewrite: it re-pins only the files the cut
	// itself edited (the uncommitted changes against HEAD) and refuses any
	// other drift, which aborts the cut — a reading change main carries
	// without its golden is a change nobody reviewed, and an unattended
	// commit must not be the one to pin it.
	Cut bool

	Stdout io.Writer
}

// Run compares the corpus's reading with the golden. A check refuses any
// difference — a moved, joined or departed file, or lines out of the walk's
// order — naming what differs. Write and Cut rewrite the golden in place (a
// sibling temporary file renamed over it); a golden that already matches
// byte for byte is left untouched. Corpus candidates the index does not
// carry yet are listed, since they are pinned only once added.
func Run(opts Options) error {
	root, err := filepath.EvalSymlinks(opts.Root)
	if err != nil {
		return fmt.Errorf("resolve the repository root: %w", err)
	}
	entries, err := Read(root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("the walk under %s reached no corpus file; refusing to read it as an empty corpus", root)
	}
	goldenPath := filepath.Join(root, filepath.FromSlash(Golden))
	old, err := os.ReadFile(goldenPath)
	if err != nil {
		// A missing golden means Root is not this repository's root; writing
		// one there would pin a corpus nothing checks.
		return fmt.Errorf("read the golden: %w", err)
	}
	if err := reportUntracked(root, opts.Stdout); err != nil {
		return err
	}
	fresh := Format(entries)
	if bytes.Equal(old, fresh) {
		fmt.Fprintf(opts.Stdout, "corpus reading unchanged (%d files)\n", len(entries))
		return nil
	}
	pinned, parseErr := Parse(old)
	switch {
	case parseErr != nil && opts.Write && !opts.Cut:
		fmt.Fprintf(opts.Stdout, "the golden does not parse (%v); rebuilding it from the corpus — review the whole diff\n", parseErr)
		pinned = map[string]string{}
	case parseErr != nil:
		return fmt.Errorf("parse the golden %s: %w", Golden, parseErr)
	}
	moved := movedPaths(pinned, entries)
	switch {
	case opts.Cut:
		changed, err := gitPaths(root, "diff", "--name-only", "--no-renames", "--relative", "-z", "HEAD")
		if err != nil {
			return err
		}
		var foreign []string
		for _, p := range moved {
			if !changed[p] {
				foreign = append(foreign, p)
				continue
			}
			// The cut edited p, but the drift may predate it: HEAD's own copy
			// must read as the golden pins it (or be absent and unpinned),
			// otherwise main already carried an unpinned change to p.
			ok, err := headMatchesPin(root, p, pinned[p])
			if err != nil {
				return err
			}
			if !ok {
				foreign = append(foreign, p)
			}
		}
		if len(foreign) > 0 {
			return fmt.Errorf("the corpus drifted outside this cut: %s read differently than the golden pins and the cut did not edit them — main carries a reading change nobody reviewed; land its golden through a reviewed change (`task corpus:reading -- --write`) before cutting", strings.Join(foreign, ", "))
		}
	case !opts.Write:
		if len(moved) == 0 {
			return fmt.Errorf("the golden %s pins every file right but its bytes are not the walk's (line order, or the final newline) — run `task corpus:reading -- --write`", Golden)
		}
		return fmt.Errorf("the golden %s is stale for %d file(s): %s — run `task corpus:reading -- --write` and review the moved lines", Golden, len(moved), strings.Join(moved, ", "))
	}
	if err := writeAtomic(goldenPath, fresh); err != nil {
		return err
	}
	fmt.Fprintf(opts.Stdout, "corpus reading rewritten: %d file(s) moved\n", len(moved))
	for _, p := range moved {
		fmt.Fprintf(opts.Stdout, "  %s\n", p)
	}
	return nil
}

// headMatchesPin reports whether HEAD's copy of p reads as the golden pins
// it — or, for a path HEAD does not carry, whether the golden pins nothing.
func headMatchesPin(root, p, pin string) (bool, error) {
	ls := exec.Command("git", gitlib.NoRunHooks("-C", root, "ls-tree", "-z", "--name-only", "HEAD", "--", p)...)
	ls.Env = gitlib.SanitizeEnv(os.Environ())
	var lsOut, lsErr bytes.Buffer
	ls.Stdout, ls.Stderr = &lsOut, &lsErr
	if err := ls.Run(); err != nil {
		return false, fmt.Errorf("git ls-tree HEAD %s in %s: %v: %s", p, root, err, strings.TrimSpace(lsErr.String()))
	}
	if lsOut.Len() == 0 {
		return pin == "", nil
	}
	show := exec.Command("git", gitlib.NoRunHooks("-C", root, "cat-file", "blob", "HEAD:"+p)...)
	show.Env = gitlib.SanitizeEnv(os.Environ())
	var blob, showErr bytes.Buffer
	show.Stdout, show.Stderr = &blob, &showErr
	if err := show.Run(); err != nil {
		return false, fmt.Errorf("git cat-file blob HEAD:%s in %s: %v: %s", p, root, err, strings.TrimSpace(showErr.String()))
	}
	return readingOf(p, blob.Bytes()).Reading() == pin, nil
}

// reportUntracked names the corpus candidates the index does not carry: a
// new page is pinned only once added, and a green check over a tree that
// holds one would otherwise go red on the clean checkout CI runs.
func reportUntracked(root string, w io.Writer) error {
	others, err := gitPaths(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	var candidates []string
	for p := range others {
		if inCorpus(p) {
			candidates = append(candidates, p)
		}
	}
	slices.Sort(candidates)
	for _, p := range candidates {
		fmt.Fprintf(w, "not pinned (untracked; `git add` it to pin it): %s\n", p)
	}
	return nil
}

// movedPaths lists, in walk order then path order, every path whose reading
// differs from the pin, joined the corpus, or left it.
func movedPaths(pinned map[string]string, entries []Entry) []string {
	var moved []string
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Path] = true
		if pinned[e.Path] != e.Reading() {
			moved = append(moved, e.Path)
		}
	}
	var gone []string
	for p := range pinned {
		if !seen[p] {
			gone = append(gone, p)
		}
	}
	slices.Sort(gone)
	return append(moved, gone...)
}

// writeAtomic replaces path with data through a temporary file created in the
// same directory, so a reader never sees a half-written golden.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".committed-reading-*")
	if err != nil {
		return fmt.Errorf("create the golden's temporary file: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write the golden's temporary file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("chmod the golden's temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close the golden's temporary file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace the golden: %w", err)
	}
	return nil
}
