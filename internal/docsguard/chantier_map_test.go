package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	chantierMapCount = regexp.MustCompile(`The (\d+) open epics`)
	chantierMapIssue = regexp.MustCompile(`https://github\.com/SocialGouv/iterion/issues/(\d+)`)
)

// TestTheChantierMapCountsItsOwnRows holds the chantier map of
// docs/state-of-the-art.md to its own sentence: the number of open epics it
// states equals the rows of its table, each row names its epic in its first
// cell, and no issue appears in two rows — an epic folded into another leaves
// no second row behind.
func TestTheChantierMapCountsItsOwnRows(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "state-of-the-art.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v — the guard's subject moved; move the guard with it", path, err)
	}
	page := string(raw)
	start := strings.Index(page, "\n## The chantier map\n")
	if start < 0 {
		t.Fatalf("%s: no `## The chantier map` section", path)
	}
	section := page[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	m := chantierMapCount.FindStringSubmatch(section)
	if m == nil {
		t.Fatalf("the chantier map no longer states `The N open epics`")
	}
	stated, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}

	rows := 0
	rowOf := map[string]int{} // issue number → line of the section holding it
	for i, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| epic |") {
			continue
		}
		rows++
		if chantierMapIssue.FindStringSubmatch(strings.Split(line, "|")[1]) == nil {
			t.Errorf("line %d of the section names no epic issue in its first cell: %s", i, line)
		}
		for _, issue := range chantierMapIssue.FindAllStringSubmatch(line, -1) {
			if prev, dup := rowOf[issue[1]]; dup && prev != i {
				t.Errorf("issue #%s appears in two rows (lines %d and %d of the section)", issue[1], prev, i)
			}
			rowOf[issue[1]] = i
		}
	}
	if rows != stated {
		t.Fatalf("the chantier map says %d open epics and has %d rows", stated, rows)
	}
}
