package repomap

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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
		fmt.Fprintf(&b, "| [`%s`](../../%s) | %s | %s | %s |\n",
			r.Path, r.Path, orDash(r.Title), orDash(r.Summary), status)
	}
	return b.String(), nil
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
			row.Summary = firstSentence(line, 140)
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
