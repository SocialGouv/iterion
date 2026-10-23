package repomap

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/mdcode"
	"github.com/SocialGouv/iterion/internal/treeskip"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
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
	// The index chooses the corpus; the working tree supplies its current
	// contents. Local drafts stay out, staged additions and unstaged edits
	// to tracked pages participate before the contributor commits them.
	// Outside a repository (a bare fixture, a non-git tree) the filesystem
	// IS the corpus: the ls-files failure falls back to the walk.
	var files []byte
	cmd := exec.Command("git", gitlib.NoRunHooks("ls-files", "--cached", "-z", "--", "docs")...)
	cmd.Dir = root
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	if out, err := cmd.Output(); err == nil {
		files = out
	} else {
		var walked []string
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			if strings.HasSuffix(rel, ".md") && !treeskip.Path(filepath.ToSlash(rel)) {
				walked = append(walked, filepath.ToSlash(rel))
			}
			return nil
		})
		if walkErr != nil {
			return "", fmt.Errorf("list indexed docs: %w (and the filesystem fallback: %w)", err, walkErr)
		}
		sort.Strings(walked)
		for _, rel := range walked {
			files = append(files, []byte(rel)...)
			files = append(files, 0)
		}
	}
	var rows []docRow
	for _, relPath := range strings.Split(string(files), "\x00") {
		if !strings.HasSuffix(relPath, ".md") || treeskip.Path(relPath) {
			continue
		}
		row, err := readDocRow(filepath.Join(root, filepath.FromSlash(relPath)), relPath)
		if err != nil {
			// An indexed page deleted from the WORKING TREE (an unstaged
			// deletion) must not abort the generation: the map regenerates
			// without it, and the freshness check flags the index/tree
			// mismatch the deletion still is. Any other read error is real.
			// errors.Is, not os.IsNotExist: the latter does not unwrap the
			// %w chain readDocRow wraps its PathError in.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })

	var b strings.Builder
	b.WriteString("One row per page under `docs/`, ADRs included. An ADR's **status** " +
		"is the column that says whether its decision still holds; superseded " +
		"ones stay listed because the reasoning is still the reason.\n\n")
	b.WriteString("| Page | Title | Opens with | Status |\n|---|---|---|---|\n")
	for _, r := range rows {
		href, err := linkTarget(r.Path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "| [`%s`](%s) | %s | %s | %s |\n",
			r.Path, href, orDash(r.Title), orDash(r.Summary), orDash(r.Status))
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
//
// An occurrence inside a code span is a form the page QUOTES, not a link it
// makes. Rewriting one would edit quoted text, and refusing one fails the
// whole generator on a page whose own links are sound — which is what a page
// documenting this rule does. internal/mdcode says where the code is, at the
// same offsets, so the grammar of a link stays this file's.
func reanchor(summary, page string) (string, error) {
	pageDir := path.Dir(page)
	var failed error
	rewrite := func(m string) string {
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
	}

	return mdcode.ReplaceOutsideCode(summary, linkTargetRe, rewrite), failed
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
	// The fence rule is internal/mdcode's — the whole machine, not the half
	// that says which lines are delimiters. Recognising `~~~` and blockquoted
	// fences while closing on any delimiter ends a ``` block on the `~~~`
	// line inside it, which is content, and the sentence this function then
	// hands to reanchor is quoted code.
	var fence mdcode.FenceScanner
	var front mdcode.FrontMatterScanner
	for scanner.Scan() {
		// The scanner reads the RAW line: a fence closes at its opener's own
		// blockquote depth and indentation, and trimming erases both before
		// it can measure them.
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if front.Skip(raw) || fence.Code(raw) || line == "" {
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

// orDash renders one table cell of a map: the text, or an em dash when
// there is none.
//
// The text is a bundle's or a page's own — a persona, a description, a
// version — so it goes through internal/mdcode.Cell: a newline in it would
// end the row early and put the rest of the value at column 0, where a
// delimiter run opens a fenced block over the whole map below it, and a pipe
// would end the cell and shift every column after it. It is the one cell
// renderer of these maps: a value spliced into a row without it is a value
// nothing escapes.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return mdcode.Cell(s)
}
