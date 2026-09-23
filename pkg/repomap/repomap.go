// Package repomap generates this repository's discoverability commons:
// small, committed indexes that let an agent — one working on the repo,
// or a bot running inside a sandbox — find the right file without
// grepping the tree first.
//
// Three properties make them worth their bytes:
//
//   - DETERMINISTIC. Everything here is derived by Go's own parser, by
//     reading markdown headings, or by the bundle manifest loader. No
//     model call, no embedding, no network. The same tree always renders
//     the same bytes.
//   - COMMITTED AND GATED. The artifacts live in docs/references/ and a
//     Go test fails when they drift from the tree, exactly as
//     pkg/dsl/spec gates the generated DSL reference. Freshness rides
//     the required `test` check; there is no separate CI job to forget.
//   - ONE ARTIFACT, TWO AUDIENCES. A file in the tree needs no protocol:
//     a session reads it, and so does a bot that checked the repo out
//     inside a container.
//
// Why these three extractors and not one: a seam with a single
// implementation is a promise, not a seam. Go packages, markdown docs and
// bot bundles exercise it from three different directions in the same
// change — and only the first is Go-specific, which is the limit the
// documentation states rather than hides.
//
// See docs/references/context-retrieval-state-of-the-art.md for what the
// alternatives would cost, and why an embedding index is a dated refusal
// rather than an oversight.
package repomap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/treeskip"
)

// Extractor renders one generated map from a repository tree.
type Extractor interface {
	// Stem names the artifact: "packages" → docs/references/map-packages.md.
	Stem() string
	// Title is the artifact's H1.
	Title() string
	// Extract reads the tree rooted at root and renders the body that
	// follows the generated-file header.
	Extract(root string) (string, error)
}

// Extractors returns the maps this repository generates, in a stable
// order.
func Extractors() []Extractor {
	return []Extractor{goPackages{}, docsPages{}, botBundles{}}
}

// OutputDir is where the committed maps live, relative to the repo root.
const OutputDir = "docs/references"

// linkTarget renders a repository-root-relative path as the link target a
// generated map writes. Every link a map emits goes through it, so no two
// columns can spell the same rule differently.
//
// The rule is one rewrite: the path relative to OutputDir. The documentation
// site's root is docs/, so a target that climbs to the repository root —
// `../../docs/quickstart.md` — leaves the site and names nothing there, while
// rendering fine on github.com; written from docs/references/ instead,
// `../quickstart.md` resolves on both. A target outside docs/ keeps climbing
// (`../../pkg/runtime/pause.go`): the site rewrites it to a github.com blob
// URL, which docs/scripts/check-links.mjs then resolves against the real tree.
func linkTarget(repoRelPath string) (string, error) {
	// The input is a path from the repository root. One that names the root
	// itself, or climbs above it, reaches no file a reader of the map can
	// open, and rewriting it only makes it climb further: refuse, naming it,
	// rather than write it down. Cleaning first tests the property rather
	// than an orthography — `./`, `a/..` and `docs/..` are that same root.
	repoRelPath = filepath.ToSlash(filepath.Clean(repoRelPath))
	if repoRelPath == "." || repoRelPath == ".." || strings.HasPrefix(repoRelPath, "../") {
		return "", fmt.Errorf("repomap: %q leaves the repository — the page that quotes it has a broken link", repoRelPath)
	}
	rel, err := filepath.Rel(OutputDir, repoRelPath)
	if err != nil {
		return "", fmt.Errorf("repomap: link from %s to %s: %w", OutputDir, repoRelPath, err)
	}
	// filepath separators are the host's; a markdown target is always slashes.
	return filepath.ToSlash(rel), nil
}

// Path returns an extractor's artifact path relative to the repo root.
func Path(e Extractor) string {
	return filepath.Join(OutputDir, "map-"+e.Stem()+".md")
}

// Generate renders every map. The result is keyed by repo-relative path.
func Generate(root string) (map[string]string, error) {
	out := make(map[string]string, len(Extractors()))
	for _, e := range Extractors() {
		body, err := e.Extract(root)
		if err != nil {
			return nil, fmt.Errorf("repomap: extract %s: %w", e.Stem(), err)
		}
		out[Path(e)] = header(e) + body
	}
	return out, nil
}

// Write renders every map and writes the ones whose bytes changed.
// Returns the paths it rewrote.
func Write(root string) ([]string, error) {
	generated, err := Generate(root)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, rel := range sortedKeys(generated) {
		abs := filepath.Join(root, rel)
		if current, err := os.ReadFile(abs); err == nil && string(current) == generated[rel] {
			continue
		}
		if err := os.WriteFile(abs, []byte(generated[rel]), 0o644); err != nil {
			return written, fmt.Errorf("repomap: write %s: %w", rel, err)
		}
		written = append(written, rel)
	}
	return written, nil
}

// Stale returns the artifacts whose committed bytes differ from what the
// tree renders now — including any that are missing entirely. An empty
// result means the commons match the repository.
func Stale(root string) ([]string, error) {
	generated, err := Generate(root)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, rel := range sortedKeys(generated) {
		current, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(current) != generated[rel] {
			stale = append(stale, rel)
		}
	}
	return stale, nil
}

// header is the generated-file preamble. It names the command that
// rewrites the file, because a generated artifact whose regeneration
// command is not on the page gets hand-edited.
func header(e Extractor) string {
	return fmt.Sprintf(`# %s

<!-- Generated by `+"`iterion map gen`"+` — do not edit by hand.
     Run `+"`task map:gen`"+` and commit; `+"`task map:check`"+` fails on drift. -->

`, e.Title())
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// walkDirs visits every directory under root that is not skipped,
// calling fn with the directory's repo-relative path ("." for root).
func walkDirs(root string, fn func(rel string, entries []os.DirEntry) error) error {
	return filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		if rel != "." && treeskip.Dir(d.Name()) {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(abs)
		if readErr != nil {
			return readErr
		}
		return fn(filepath.ToSlash(rel), entries)
	})
}

// firstSentence reduces a doc comment or a paragraph to its opening
// sentence, collapsed onto one line and bounded, so a table row stays a
// row. Markdown pipes are escaped: an unescaped one would split a cell.
func firstSentence(text string, max int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	if i := strings.Index(text, ". "); i > 0 {
		text = text[:i+1]
	}
	if len(text) > max {
		cut := strings.LastIndex(text[:max], " ")
		if cut < max/2 {
			cut = max
		}
		// Back up to a rune boundary. `max` is a BYTE bound, and a cut
		// inside a multi-byte rune — an em-dash, an accent, both common
		// in this tree — writes invalid UTF-8 into a committed artifact.
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = strings.TrimSpace(text[:cut]) + "…"
	}
	return strings.ReplaceAll(text, "|", `\|`)
}
