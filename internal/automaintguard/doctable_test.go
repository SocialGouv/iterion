package automaintguard

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The preflight and the page that documents it are two systems, and nothing
// type-checks the agreement between them — this project's own thesis, applied
// to the file that states it. The bill came: agreement 8 landed in the script
// and in this bench while the page kept spelling "Seven agreements" over seven
// rows, 280 lines below its own paragraph describing agreement 8.
//
// So the count a reader counts is derived from the two artefacts, once, at
// load: the script's own calls on one side, the table and the spelled number on
// the other.

var (
	// Every label the script can emit. The number is the identity a reader
	// counts; the rest of the label is quoted back so a failure names the
	// agreement rather than a digit.
	scriptAgreementRE = regexp.MustCompile(`\b(?:agree|disagree|blind) "(\d+) ([^"]*)"`)
	// A row of the agreement table — number, agreement, what broken means.
	docAgreementRowRE = regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|([^|]*)\|([^|]*)\|`)
	// The count the page spells in prose, which is what a reader trusts.
	docAgreementCountRE = regexp.MustCompile(`([A-Za-z]+) agreements:`)
)

// Spelled out rather than parsed as a digit, because the page spells it out.
var spelledNumbers = map[string]int{
	"One": 1, "Two": 2, "Three": 3, "Four": 4, "Five": 5, "Six": 6,
	"Seven": 7, "Eight": 8, "Nine": 9, "Ten": 10, "Eleven": 11, "Twelve": 12,
}

// readOnce freezes the artefact under judgement: the text parsed below is the
// text read here, never re-read at verdict time.
func readOnce(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// preflightSection narrows the parse to the one section that counts
// agreements, so the permission table of step 2 and the recipe's own numbered
// steps cannot be mistaken for agreement rows.
func preflightSection(t *testing.T, doc string) string {
	t.Helper()
	const heading = "\n## The preflight\n"
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("%s carries no %q section — this witness would have nothing to compare", docRelPath, strings.TrimSpace(heading))
	}
	rest := doc[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func atoiOrFatal(t *testing.T, s, what string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%s: %q is not a number: %v", what, s, err)
	}
	return n
}

func TestTheDocTableNamesEveryAgreementTheScriptCanEmit(t *testing.T) {
	script := readOnce(t, scriptRelDir)
	section := preflightSection(t, readOnce(t, docRelPath))

	emitted := map[int]string{}
	for _, m := range scriptAgreementRE.FindAllStringSubmatch(script, -1) {
		emitted[atoiOrFatal(t, m[1], "an agreement label in "+scriptRelDir)] = m[2]
	}
	if len(emitted) == 0 {
		t.Fatalf("no agree/disagree/blind call with a numbered label found in %s — this witness would pass on an empty script", scriptRelDir)
	}
	// Contiguous from 1, or every count below is a statement about something
	// else: a reader counting rows cannot see a hole in the numbering.
	for i := 1; i <= len(emitted); i++ {
		if _, ok := emitted[i]; !ok {
			t.Errorf("%s emits %d distinct agreement numbers and none of them is %d — the numbering has a hole",
				scriptRelDir, len(emitted), i)
		}
	}

	rows := map[int]bool{}
	for _, m := range docAgreementRowRE.FindAllStringSubmatch(section, -1) {
		n := atoiOrFatal(t, m[1], "an agreement row in "+docRelPath)
		if rows[n] {
			t.Errorf("%s lists agreement %d twice", docRelPath, n)
		}
		rows[n] = true
		if strings.TrimSpace(m[2]) == "" || strings.TrimSpace(m[3]) == "" {
			t.Errorf("row %d of %s leaves a column empty (%q |%q) — an agreement counted among the lines that can redden has to say what broken means",
				n, docRelPath, m[2], m[3])
		}
	}
	if len(rows) == 0 {
		t.Fatalf("the preflight section of %s carries no agreement row — this witness would pass on an empty table", docRelPath)
	}

	for n, label := range emitted {
		if !rows[n] {
			t.Errorf("the script can emit agreement %d (%q) and %s lists no row for it — the page undercounts what the preflight decides",
				n, label, docRelPath)
		}
	}
	for n := range rows {
		if _, ok := emitted[n]; !ok {
			t.Errorf("%s lists agreement %d, which %s can never emit", docRelPath, n, scriptRelDir)
		}
	}

	// The spelled count last, because a reader trusts the sentence over the
	// rows: "Seven agreements:" above eight rows is the same drift one line up.
	counts := docAgreementCountRE.FindAllStringSubmatch(section, -1)
	if len(counts) != 1 {
		t.Fatalf("expected exactly one spelled agreement count in the preflight section of %s, found %d — this witness cannot tell which one a reader counts",
			docRelPath, len(counts))
	}
	spelled, ok := spelledNumbers[counts[0][1]]
	if !ok {
		t.Fatalf("%s spells %q agreements, a number this witness does not know — add it rather than widening the match",
			docRelPath, counts[0][1])
	}
	if spelled != len(rows) {
		t.Errorf("%s says %q agreements (%d) over %d rows — the sentence and the table disagree",
			docRelPath, counts[0][1], spelled, len(rows))
	}
}
