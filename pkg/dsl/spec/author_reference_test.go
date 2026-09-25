package spec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The docs site builds this page, and its markdown reads a `{ … }` in a
// table cell as an attribute block (markdown-it-attrs): the cell gets an
// attribute named `…`, and the page no longer compiles. No table row of the
// author reference holds a brace outside a code span.
func TestTheAuthorReferenceHoldsNoBareBraceInATable(t *testing.T) {
	codeSpan := regexp.MustCompile("`[^`]*`")
	for i, line := range strings.Split(AuthorReference(), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if bare := codeSpan.ReplaceAllString(line, ""); strings.ContainsAny(bare, "{}") {
			t.Errorf("line %d holds a brace outside a code span: %s", i+1, line)
		}
	}
}

// The top-level keys the reference lists are the keys the author JSON
// Schema's root accepts — both directions, in every profile: a key added to
// one and not the other is a page that describes another document than the
// schema validates.
func TestAuthorKeysAreTheSchemasRoot(t *testing.T) {
	var listed []string
	for _, k := range AuthorKeys() {
		listed = append(listed, k.Name)
	}
	sort.Strings(listed)
	for _, profile := range []int{1, 2} {
		raw, err := RenderSchema(profile)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		var root []string
		for k := range schema.Properties {
			root = append(root, k)
		}
		sort.Strings(root)
		if strings.Join(root, " ") != strings.Join(listed, " ") {
			t.Fatalf("profile %d: the schema's root accepts %v, the reference lists %v", profile, root, listed)
		}
	}
}

// Every value form the registry uses has its row in the reference, and every
// row is a form the registry uses — a form added to the language without its
// YAML spelling, or a row left for a form gone, fails.
func TestAuthorFormsAreTheRegistrysForms(t *testing.T) {
	used := map[Form]bool{}
	fields := func(fs []Field) {
		for _, f := range fs {
			used[f.Form] = true
		}
	}
	var entries func(e *Entries)
	entries = func(e *Entries) {
		if e == nil {
			return
		}
		fields(e.Fields)
		entries(e.Entries)
	}
	for _, k := range Kinds {
		for _, p := range k.Properties {
			used[p.Form] = true
		}
		if k.Header != nil {
			fields(k.Header.Fields)
		}
		entries(k.Entries)
	}
	listed := map[Form]bool{}
	for _, f := range authorFormOrder {
		if listed[f] {
			t.Errorf("the form %q is listed twice", f)
		}
		listed[f] = true
		if _, ok := authorFormNotes[f]; !ok {
			t.Errorf("the form %q is ordered but has no spelling", f)
		}
	}
	for f := range authorFormNotes {
		if !listed[f] {
			t.Errorf("the form %q has a spelling but is not ordered — it would not be rendered", f)
		}
	}
	for f := range used {
		if !listed[f] {
			t.Errorf("the registry uses the form %q, which the reference does not say how to write", f)
		}
	}
	for f := range listed {
		if !used[f] {
			t.Errorf("the reference says how to write %q, which no property or entry uses", f)
		}
	}
}

// Every node kind of the registry is listed with a link to its property
// table, and nothing else is.
func TestAuthorReferenceListsEveryNodeKind(t *testing.T) {
	body := AuthorReference()
	for _, k := range Kinds {
		line := "- `" + k.Name + ": <name>` — [properties](dsl-properties.md#" + anchor(k.Name) + ")"
		if has := strings.Contains(body, line); has != (k.Role == Node) {
			t.Errorf("%s (a %s): listed as a node %v", k.Name, k.Role, has)
		}
	}
}

// The author surfaces teach how a value is WRITTEN, and in YAML a bare
// header is refused (E051, "got null") where the .bot reads one as an empty
// declaration — so no rendered author surface (the reference page, either
// profile's JSON Schema) may repeat the .bot's "a bare header declares"
// sentence, and none may send a reader to --recipe for presets: a recipe
// file carries its own preset_vars and does not select them. The overrides
// that rewrite the registry Doc on this surface live in author_reference.go;
// this test renders the surfaces so a new registry Doc that grows one of the
// phrases re-reddens here instead of shipping the lie.
func TestTheAuthorSurfacesNeverTeachABareHeaderOrARecipePreset(t *testing.T) {
	surfaces := map[string]string{"reference": AuthorReference()}
	for _, profile := range []int{1, 2} {
		raw, err := RenderSchema(profile)
		if err != nil {
			t.Fatal(err)
		}
		surfaces[fmt.Sprintf("schema v%d", profile)] = string(raw)
	}
	for name, body := range surfaces {
		for _, phrase := range []string{"a bare header", "--recipe"} {
			if strings.Contains(body, phrase) {
				t.Errorf("the author %s teaches %q — the converter refuses a bare header (E051) and a recipe file does not select presets", name, phrase)
			}
		}
	}
	if !strings.Contains(surfaces["reference"], "an empty schema is written `verdict: {}`") {
		t.Error("the reference no longer spells the working empty form for schemas")
	}
}
