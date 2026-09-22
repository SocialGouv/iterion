package repomap_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/repomap"
)

// emittedLinkRe matches the destination of an inline link in a rendered map.
var emittedLinkRe = regexp.MustCompile(`\]\(([^)]*)\)`)

// externalRe matches a target that leaves the repository: a URL with a
// scheme, or a protocol-relative one.
var externalRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*:|//)`)

// rowSourceRe reads a table row's first cell — the file the rest of the row
// quotes. A map is generated, so an error that names only the artifact sends
// the author to the one file they must not edit.
var rowSourceRe = regexp.MustCompile("^\\| \\[?`([^`]+)`")

// A generated map is read from two places, and a link has to work in both:
// as a file on github.com, where docs/references/map-*.md sits next to the
// tree, and as a page of the documentation site, whose root is docs/ — so a
// target that climbs to the repository root renders on github.com and names
// nothing on the site. That is the whole of #1508: 318 dead links on one
// page kept the site from publishing for three days.
//
// This resolves every target on the real tree rather than matching its
// spelling: reverting a map's links to the repository-root-relative
// `../../docs/<page>.md` reddens the site half here.
//
// Its model of the site covers what the maps emit — `.md` pages under docs/,
// and files outside it that the site rewrites to a github.com blob URL. It is
// not a second VitePress: a target under docs/public/, which the site serves
// from its ROOT, and one whose extension decides that rewrite are the build's
// verdict to give. This is the fast net; `pnpm -C docs build` is the gate.
func TestEveryLinkAGeneratedMapEmitsResolvesFromTheCommonsDirectory(t *testing.T) {
	maps, err := repomap.Generate(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) == 0 {
		t.Fatal("no map generated — this test would prove nothing")
	}

	// The site's page directory for a map: docs/references/ minus the
	// srcDir docs/. A site-internal target is resolved from there, exactly
	// as docs/scripts/check-links.mjs resolves one against its page.
	siteDir := strings.TrimPrefix(repomap.OutputDir+"/", "docs/")

	checked := 0
	for artifact, body := range maps {
		for _, line := range strings.Split(body, "\n") {
			source := artifact
			if r := rowSourceRe.FindStringSubmatch(line); r != nil {
				source = r[1]
			}
			for _, m := range emittedLinkRe.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if i := strings.Index(target, "#"); i >= 0 {
					target = target[:i]
				}
				if target == "" || externalRe.MatchString(target) || strings.HasPrefix(target, "/") {
					continue
				}
				checked++

				// github.com resolves the target against the map's own
				// directory. It must name something that exists.
				repoRel := path.Join(repomap.OutputDir, target)
				if strings.HasPrefix(repoRel, "..") {
					t.Errorf("%s (row of %s): %q escapes the repository (resolves to %q)",
						source, artifact, m[1], repoRel)
					continue
				}
				info, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(repoRel)))
				if err != nil {
					t.Errorf("%s (row of %s): %q names nothing — resolved to %q: %v",
						source, artifact, m[1], repoRel, err)
					continue
				}

				// A target that stays under docs/ is a site page, resolved
				// from the map's directory INSIDE the site. Climbing above
				// the site root is the dead link. A target outside docs/ is
				// rewritten to a github.com blob URL by docs/.vitepress/
				// config.ts, and the Stat above is what proves that one.
				if !strings.HasPrefix(repoRel, "docs/") {
					continue
				}
				if sitePath := path.Join(siteDir, target); strings.HasPrefix(sitePath, "..") {
					t.Errorf("%s (row of %s): %q climbs out of the site root docs/ (resolves to %q on the site)",
						source, artifact, m[1], sitePath)
				}
				switch {
				case info.IsDir() && !strings.HasSuffix(target, "/"):
					// The site resolves a bare target against its PAGE set,
					// not against the tree: `../observability` is the page
					// built from docs/observability.md and resolves, while a
					// nested index.md answers only the spelling that carries
					// the slash — `../comparisons` is dead on the site even
					// though docs/comparisons/index.md exists.
					// A page is a regular file: a *directory* named
					// `<dir>.md` is not globbed into the page set.
					if st, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(repoRel)+".md")); err == nil && st.Mode().IsRegular() {
						break
					}
					t.Errorf("%s (row of %s): %q names the directory %q, under which the site builds no page",
						source, artifact, m[1], repoRel)
				case !info.IsDir() && strings.HasSuffix(target, "/"):
					t.Errorf("%s (row of %s): %q gives the file %q a trailing slash, which the site routes as a directory",
						source, artifact, m[1], repoRel)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no in-repository link examined — the maps stopped emitting links, or this test stopped finding them")
	}
}

// docsRowRe reads one row of the docs map's Page column: the page it names,
// and the target it links that name to.
var docsRowRe = regexp.MustCompile("(?m)^\\| \\[`([^`]+)`\\]\\(([^)]+)\\) \\|")

// The Page column's name and its link are two spellings of one path, and
// nothing but this test keeps them equal. It reddens when the link stops
// naming its own row — a different page, a stripped extension, a fragment, a
// leading slash — and deliberately not on a mere change of spelling: any
// `../../docs/x.md` form still resolves to the row it names, and the sibling
// test above is what catches that one.
func TestTheDocsMapLinksEveryPageToTheFileItNames(t *testing.T) {
	maps, err := repomap.Generate(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := maps[filepath.Join(repomap.OutputDir, "map-docs.md")]
	if !ok {
		t.Fatalf("no docs map among %d artifacts", len(maps))
	}

	rows := docsRowRe.FindAllStringSubmatch(body, -1)
	// The corpus this map indexes is every markdown page under docs/; a
	// handful of rows would mean the column stopped linking, not that the
	// tree shrank.
	if len(rows) < 100 {
		t.Fatalf("docs map has %d linked rows — the Page column stopped linking", len(rows))
	}
	for _, r := range rows {
		named, target := r[1], r[2]
		if got := path.Join(repomap.OutputDir, target); got != named {
			t.Errorf("row names %q but links %q, which resolves to %q", named, target, got)
		}
	}
}

// The package and bot maps render every path as inline code, never as a
// link: their prose columns quote a Go doc comment and a bundle description
// verbatim, and a target written to resolve from `pkg/x/` or `bots/y/` names
// something else from docs/references/ — often a file that exists, which is
// how a wrong link passes a resolution check. Emitting one is the signal to
// route that column through reanchor, not to delete this test.
func TestThePackageAndBotMapsEmitNoLinks(t *testing.T) {
	maps, err := repomap.Generate(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, stem := range []string{"packages", "bots"} {
		artifact := filepath.Join(repomap.OutputDir, "map-"+stem+".md")
		body, ok := maps[artifact]
		if !ok {
			t.Fatalf("no %s map among %d artifacts", stem, len(maps))
		}
		for _, m := range emittedLinkRe.FindAllStringSubmatch(body, -1) {
			t.Errorf("%s renders %q as a markdown link — a quoted column started carrying `](…)`; re-anchor it against the source that wrote it, or escape the brackets",
				artifact, m[1])
		}
	}
}

// The real extractor, on a tree built to carry the condition: a page whose
// opening sentence quotes a sibling directory. Read from docs/references/
// the rendered target must still name a directory — the trailing slash is
// what sends it to a github.com tree URL instead of to a page the site never
// builds. The real corpus carries no such quote today, so this fixture is
// the only thing that exercises the rule.
func TestADirectoryQuotedInAPageStaysADirectoryOnTheMap(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"docs/guide.md":            "# A guide\n\nThe [decisions](adr/) behind it and the [next one](sub/deep/) too.\n",
		"docs/adr/001-decision.md": "# ADR-001\n\n- **Status**: Accepted\n\nBecause of the reason.\n",
		"docs/sub/deep/page.md":    "# Deep\n\nIt goes deeper.\n",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var docs repomap.Extractor
	for _, e := range repomap.Extractors() {
		if e.Stem() == "docs" {
			docs = e
		}
	}
	if docs == nil {
		t.Fatal("no docs extractor — this test would prove nothing")
	}
	body, err := docs.Extract(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"](../adr/)", "](../sub/deep/)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the map does not carry %s:\n%s", want, body)
		}
	}
}
