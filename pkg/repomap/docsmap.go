package repomap

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// docsPages maps docs/ — 300-odd pages and 100-odd ADRs whose only index
// today is hand-kept prose. One row per page: path, title, opening
// sentence, and an ADR's status, which is the field that decides whether
// a decision still holds.
//
// This is the data half of the complaint in #1458: a prompt promised
// "titles + slugs" and handed the full inventory instead. Link checking
// is NOT here — that is #1233's, and the two compose: this index is a
// source the checker can read.
type docsPages struct{}

func (docsPages) Stem() string  { return "docs" }
func (docsPages) Title() string { return "Docs and ADR map" }

type docRow struct {
	Path    string
	Title   string
	Summary string
	Status  string
	IsADR   bool
}

func (d docsPages) Extract(root string) (string, error) {
	var rows []docRow
	err := walkDirs(filepath.Join(root, "docs"), func(rel string, entries []os.DirEntry) error {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			relPath := filepath.ToSlash(filepath.Join("docs", filepath.Join(rel, e.Name())))
			relPath = strings.ReplaceAll(relPath, "docs/./", "docs/")
			row, err := readDocRow(filepath.Join(root, relPath), relPath)
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })

	adrs := 0
	for _, r := range rows {
		if r.IsADR {
			adrs++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d pages under `docs/`, of which %d ADRs. "+
		"An ADR's **status** is the column that says whether its decision still "+
		"holds; superseded ones stay listed because the reasoning is still "+
		"the reason.\n\n", len(rows), adrs)
	b.WriteString("| Page | Title | Opens with | Status |\n|---|---|---|---|\n")
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = "—"
		}
		href, err := linkTarget(r.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "| [`%s`](%s) | %s | %s | %s |\n",
			r.Path, href, orDash(r.Title), orDash(r.Summary), status)
	}
	return b.String(), nil
}

// linkTargetRe matches the destination of an inline link or image in a
// summary cell: `](target)`.
var linkTargetRe = regexp.MustCompile(`\]\(([^)]*)\)`)

// externalTargetRe matches a target that leaves the repository: a URL with
// a scheme, or a protocol-relative one.
var externalTargetRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*:|//)`)

// reanchor rewrites the relative link targets a summary quotes so they
// resolve from the map that carries it. The sentence is taken from the
// page's own prose, where `adr/081-….md` or `README.md` names a file next
// to that page; on the map (docs/references/, one level below docs/) the
// same text would name nothing. Each target is resolved against the page's
// directory and then written by linkTarget, its fragment kept; a bare
// `#fragment` names the quoted page's own anchor, so it gains that page's
// path. Scheme URLs pass through; so does a `/`-absolute target, which
// github.com resolves against the site origin rather than this repository —
// `task docs:links` reports that one on the page that wrote it.
func reanchor(summary, page string) (string, error) {
	pageDir := path.Dir(page)
	var failed error
	out := linkTargetRe.ReplaceAllStringFunc(summary, func(m string) string {
		target := m[2 : len(m)-1]
		frag := ""
		if i := strings.Index(target, "#"); i >= 0 {
			frag, target = target[i:], target[:i]
		}
		var resolved string
		switch {
		case target == "":
			if frag == "" || frag == "#" {
				return m
			}
			resolved = page
		case externalTargetRe.MatchString(target), strings.HasPrefix(target, "/"):
			return m
		default:
			resolved = path.Join(pageDir, target)
		}
		rel, err := linkTarget(resolved)
		if err != nil {
			// Join rather than overwrite: which refusal a single variable
			// would keep is decided by the order of the targets in the
			// sentence, and every one of them has to be fixed anyway. The
			// text the page wrote is what the author greps for.
			failed = errors.Join(failed, fmt.Errorf("%s: %w", m, err))
			return m
		}
		// path.Join drops a directory target's trailing slash, and that slash
		// is the whole signal: the site reads a directory from it and nothing
		// else, so `../adr` is routed as a page it never builds. A slash on a
		// target that is no directory is reproduced too — the link is broken
		// on the page that wrote it, and cleaning it here hid that.
		if strings.HasSuffix(target, "/") {
			rel += "/"
		}
		return "](" + rel + frag + ")"
	})
	return out, failed
}

// readDocRow pulls a page's H1, its first prose sentence, and — for an
// ADR — the Status bullet the template puts under the title.
func readDocRow(abs, rel string) (docRow, error) {
	f, err := os.Open(abs)
	if err != nil {
		return docRow{}, fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	row := docRow{Path: rel, IsADR: strings.HasPrefix(rel, "docs/adr/")}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inFence := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || line == "" {
			continue
		}
		switch {
		case row.Title == "" && strings.HasPrefix(line, "# "):
			row.Title = firstSentence(strings.TrimPrefix(line, "# "), 90)
		case row.IsADR && row.Status == "" && strings.HasPrefix(line, "- **Status**:"):
			row.Status = firstSentence(strings.TrimPrefix(line, "- **Status**:"), 60)
		case row.Summary == "" && isProse(line):
			summary, err := reanchor(firstSentence(line, 140), rel)
			if err != nil {
				return row, fmt.Errorf("%s: %w", rel, err)
			}
			row.Summary = summary
		}
		if row.Title != "" && row.Summary != "" && (!row.IsADR || row.Status != "") {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return row, fmt.Errorf("read %s: %w", rel, err)
	}
	return row, nil
}

// isProse rejects the lines that are structure rather than content, so
// the summary column carries a sentence and not a table separator.
func isProse(line string) bool {
	switch {
	case strings.HasPrefix(line, "#"), strings.HasPrefix(line, ">"),
		strings.HasPrefix(line, "|"), strings.HasPrefix(line, "-"),
		strings.HasPrefix(line, "*"), strings.HasPrefix(line, "<!--"),
		strings.HasPrefix(line, "<"), strings.HasPrefix(line, "["):
		return false
	}
	return true
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
