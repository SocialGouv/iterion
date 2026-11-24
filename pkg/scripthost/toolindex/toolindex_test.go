package toolindex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func mustIndex(t *testing.T, tools []Tool) *Index {
	t.Helper()
	cat, omitted, err := Bound(tools)
	if err != nil {
		t.Fatalf("Bound: %v", err)
	}
	return New(cat, omitted)
}

func sample() []Tool {
	return []Tool{
		{Name: "read_file", Description: "Read a file from the workspace. Returns its text.", InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"}},"required":["path"]}`)},
		{Name: "bash", Description: "Run a shell command.", InputSchema: schema(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		{Name: "list_dir", Description: "List a directory.", InputSchema: schema(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	}
}

// The index lines sit in the cached prompt prefix, so two registrations of
// the same tools in a different order must produce the same bytes — lines AND
// declarations.
func TestTheIndexIsByteStableWhateverTheRegistrationOrder(t *testing.T) {
	a := sample()
	b := []Tool{a[2], a[0], a[1]}
	ia, ib := mustIndex(t, a), mustIndex(t, b)
	if ia.Lines() != ib.Lines() {
		t.Fatalf("the lines depend on registration order:\n%s\n---\n%s", ia.Lines(), ib.Lines())
	}
	for _, tl := range a {
		da, _ := ia.Describe(tl.Name)
		db, _ := ib.Describe(tl.Name)
		if da != db {
			t.Fatalf("%s's declaration depends on registration order", tl.Name)
		}
	}
	want := "- bash {command}: Run a shell command.\n- list_dir {path?}: List a directory.\n- read_file {path, offset?}: Read a file from the workspace."
	if got := ia.Lines(); got != want {
		t.Fatalf("lines =\n%s\nwant\n%s", got, want)
	}
}

// The node's dispatcher keys its tools by name and the last registration
// wins (buildToolMap), so the catalogue must describe the same one.
func TestAToolRegisteredTwiceIsDescribedAsItsLastRegistration(t *testing.T) {
	x := mustIndex(t, []Tool{
		{Name: "probe", Description: "First registration."},
		{Name: "probe", Description: "Last registration."},
	})
	if x.Len() != 1 {
		t.Fatalf("Len = %d, want 1", x.Len())
	}
	if !strings.Contains(x.Lines(), "Last registration.") {
		t.Fatalf("the index describes a registration the dispatcher does not serve:\n%s", x.Lines())
	}
}

func TestAnEmptyIndexSaysSo(t *testing.T) {
	if got := mustIndex(t, nil).Lines(); !strings.Contains(got, "no tool a program may call") {
		t.Fatalf("empty index lines = %q", got)
	}
}

// Past MaxTools, the tools beyond stay callable but unindexed, and the index
// says how many — never a silent cut.
func TestPastTheToolBoundTheIndexCountsTheRest(t *testing.T) {
	var tools []Tool
	for i := 0; i < MaxTools+44; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("t%04d", i)})
	}
	x := mustIndex(t, tools)
	if x.Len() != MaxTools {
		t.Fatalf("Len = %d, want %d", x.Len(), MaxTools)
	}
	if !strings.HasSuffix(x.Lines(), fmt.Sprintf("- … and 44 more a program can call, outside the index's bounds (%d tools, %d-byte names): call.search does not find them, call(name, input) still reaches them", MaxTools, MaxNameBytes)) {
		t.Fatalf("the index does not count the tools past its bound:\n…%s", x.Lines()[len(x.Lines())-200:])
	}
}

// A schema over the per-tool bound is omitted, and its declaration says how
// large it was instead of rendering an input the tool does not take.
func TestASchemaOverItsBoundIsOmittedAndTheDeclarationSaysSo(t *testing.T) {
	big := `{"type":"object","properties":{"x":{"type":"string","description":"` + strings.Repeat("a", MaxSchemaBytes) + `"}}}`
	x := mustIndex(t, []Tool{{Name: "huge", InputSchema: schema(big)}})
	d, _ := x.Describe("huge")
	if !strings.Contains(d, fmt.Sprintf("input: unknown /* the input schema is %d bytes, over the catalogue's bound", len(big))) {
		t.Fatalf("declaration of an omitted schema = %s", d)
	}
}

// Past the total schema budget, the LARGEST schemas go first — and only as
// many as needed, so the rest stay described.
func TestTheSchemaBudgetDropsTheLargestFirst(t *testing.T) {
	mk := func(n int) json.RawMessage {
		return schema(`{"type":"object","description":"` + strings.Repeat("s", n) + `"}`)
	}
	var tools []Tool
	for i := 0; i < 40; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("small%02d", i), InputSchema: mk(20 << 10)}) // 40 × 20 KiB = 800 KiB
	}
	tools = append(tools,
		Tool{Name: "large_a", InputSchema: mk(30 << 10)},
		Tool{Name: "large_b", InputSchema: mk(30 << 10)},
		Tool{Name: "large_c", InputSchema: mk(31 << 10)},
		Tool{Name: "large_d", InputSchema: mk(31 << 10)},
		Tool{Name: "large_e", InputSchema: mk(31 << 10)},
		Tool{Name: "large_f", InputSchema: mk(31 << 10)},
		Tool{Name: "large_g", InputSchema: mk(31 << 10)},
		Tool{Name: "large_h", InputSchema: mk(31 << 10)},
	)
	cat, _, err := Bound(tools)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, tl := range cat {
		total += len(tl.InputSchema)
		if strings.HasPrefix(tl.Name, "small") && tl.SchemaOmitted != 0 {
			t.Errorf("%s lost its schema while larger ones remained", tl.Name)
		}
	}
	if total > MaxSchemasBytes {
		t.Fatalf("schemas total %d bytes, over %d", total, MaxSchemasBytes)
	}
	omitted := 0
	for _, tl := range cat {
		if tl.SchemaOmitted > 0 {
			omitted++
		}
	}
	if omitted == 0 || omitted == 8 {
		t.Fatalf("omitted %d large schemas; want some but not all of them", omitted)
	}
}

// The catalogue crosses into the child before it is confined, so its
// ENCODED size is what is bounded. Control characters escape six-fold: a
// bound on raw bytes would let a hostile catalogue past it.
func TestTheWorstCaseCatalogueEncodesUnderItsBound(t *testing.T) {
	hostile := strings.Repeat("\x01", 10<<10)
	var tools []Tool
	for i := 0; i < MaxTools+10; i++ {
		tools = append(tools, Tool{
			Name:        fmt.Sprintf("tool_%03d_%s", i, strings.Repeat("n", 200)),
			Description: hostile,
			InputSchema: schema(`{"type":"object","description":"` + strings.Repeat("d", 30<<10) + `"}`),
		})
	}
	tools = append(tools, Tool{Name: strings.Repeat("\x02", 100)})
	cat, omitted, err := Bound(tools)
	if err != nil {
		t.Fatalf("Bound refused the worst case: %v", err)
	}
	encoded, _ := json.Marshal(cat)
	if len(encoded) > MaxCatalogBytes {
		t.Fatalf("catalogue encodes to %d bytes, over %d", len(encoded), MaxCatalogBytes)
	}
	if omitted != 11 {
		t.Fatalf("omitted = %d, want 11 (10 past MaxTools + 1 name over MaxNameBytes)", omitted)
	}
	for _, tl := range cat {
		if encodedLen(tl.Description) > MaxDescriptionBytes {
			t.Fatalf("%s's description encodes to %d bytes", tl.Name, encodedLen(tl.Description))
		}
	}
}

func TestSummaryIsTheFirstSentenceWithAMarkWhenCut(t *testing.T) {
	cases := map[string]string{
		"Read a file. Then more.":    "Read a file.",
		"Version v1.2 is used here.": "Version v1.2 is used here.",
		"  spaced\n\tout   text ":    "spaced out text",
		"":                           "",
	}
	for in, want := range cases {
		if got := Summary(in); got != want {
			t.Errorf("Summary(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("word ", 60)
	got := Summary(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > summaryRunes {
		t.Fatalf("a long sentence is cut to %d runes with a mark: %q", summaryRunes, got)
	}
}

func TestSearchRanksNameOverProseAndBreaksTiesByName(t *testing.T) {
	x := mustIndex(t, []Tool{
		{Name: "grep_files", Description: "Search inside files for a pattern."},
		{Name: "read_file", Description: "Read a file."},
		{Name: "notes", Description: "Keep notes about a file you read."},
		{Name: "weather", Description: "Tell the weather."},
	})
	hits := x.Search("read file", 10)
	if len(hits) == 0 || hits[0].Name != "read_file" {
		t.Fatalf("hits = %+v, want read_file first", hits)
	}
	for _, h := range hits {
		if h.Name == "weather" {
			t.Fatalf("a tool matching no query term was returned: %+v", hits)
		}
	}
	if got := x.Search("", 10); len(got) != 0 {
		t.Fatalf("an empty query answered %d hits", len(got))
	}
	// Equal documents rank by name, whatever their registration order.
	y := mustIndex(t, []Tool{{Name: "b_tool", Description: "same"}, {Name: "a_tool", Description: "same"}})
	if h := y.Search("same", 5); len(h) != 2 || h[0].Name != "a_tool" {
		t.Fatalf("tie order = %+v, want a_tool first", h)
	}
}

// A program that knows a tool's exact name is looking for that tool, even
// when another tool's name and prose hold the word more often — the case BM25
// alone ranks the other way.
func TestSearchPutsAnExactNameFirst(t *testing.T) {
	x := mustIndex(t, []Tool{
		{Name: "run", Description: "Start the configured pipeline for the current workspace and stream every stage's output back to the caller until the last stage completes or one of them fails."},
		{Name: "run_all", Description: "Run, run and run again: run everything."},
	})
	if h := x.Search("run", 5); len(h) == 0 || h[0].Name != "run" {
		t.Fatalf("hits = %+v, want the exact name first", h)
	}
}

// New indexes in name order whatever order it is handed — on its own, not
// only because Bound sorted first: the child builds its index from what the
// request carried.
func TestNewIndexesInNameOrderWhateverItIsGiven(t *testing.T) {
	x := New([]Tool{{Name: "zeta", Description: "same"}, {Name: "alpha", Description: "same"}}, 0)
	if got := x.Lines(); got != "- alpha: same\n- zeta: same" {
		t.Fatalf("lines = %q", got)
	}
	if h := x.Search("same", 5); len(h) != 2 || h[0].Name != "alpha" {
		t.Fatalf("tie order = %+v, want alpha first", h)
	}
}

func TestSearchClampsItsLimit(t *testing.T) {
	var tools []Tool
	for i := 0; i < 30; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("file_tool_%02d", i), Description: "file"})
	}
	x := mustIndex(t, tools)
	if n := len(x.Search("file", 0)); n != DefaultSearchLimit {
		t.Errorf("limit 0 answered %d, want the default %d", n, DefaultSearchLimit)
	}
	if n := len(x.Search("file", 1000)); n != MaxSearchLimit {
		t.Errorf("limit 1000 answered %d, want the maximum %d", n, MaxSearchLimit)
	}
}

func TestTokensSplitCaseAndSeparators(t *testing.T) {
	got := strings.Join(tokens("readFile read_file read-file HTTPServer v2"), " ")
	if want := "read file read file read file httpserver v2"; got != want {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

// Revi Rbc7e38: encoding/json HTML-escapes `<`, `>` and `&` six-fold, inside a
// json.RawMessage schema too. Bounds measured on raw bytes and a catalogue
// written HTML-escaped disagreed, and a few MCP schemas full of `<` failed the
// whole node. The bounds and the encoding are now one rule (Encode).
func TestSchemasFullOfHTMLCharactersStayUnderTheBound(t *testing.T) {
	var tools []Tool
	for i := 0; i < 64; i++ {
		tools = append(tools, Tool{
			Name:        fmt.Sprintf("mcp_%02d", i),
			Description: strings.Repeat("<&>", 100),
			InputSchema: schema(`{"type":"object","description":"` + strings.Repeat("<", 30<<10) + `"}`),
		})
	}
	cat, _, err := Bound(tools)
	if err != nil {
		t.Fatalf("Bound failed the node on HTML characters: %v", err)
	}
	encoded, err := Encode(cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxCatalogBytes {
		t.Fatalf("catalogue encodes to %d bytes, over %d", len(encoded), MaxCatalogBytes)
	}
	if strings.Contains(string(encoded), `\u003c`) {
		t.Fatalf("the catalogue was written HTML-escaped: the bounds measure an encoding it does not use")
	}
	kept := 0
	for _, tl := range cat {
		if len(tl.InputSchema) > 0 {
			kept++
		}
	}
	if kept == 0 {
		t.Fatalf("every schema was omitted: `<` must cost one byte, not six")
	}
}
