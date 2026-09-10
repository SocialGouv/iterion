package forge

import (
	"slices"
	"strings"
	"testing"
)

func TestParseProjectRef(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantOwn string
		wantNum int
		wantErr bool
	}{
		{in: "SocialGouv/203", wantOwn: "SocialGouv", wantNum: 203},
		{in: "  SocialGouv / 203 ", wantOwn: "SocialGouv", wantNum: 203},
		{in: "SocialGouv", wantErr: true},
		{in: "SocialGouv/", wantErr: true},
		{in: "SocialGouv/abc", wantErr: true},
		{in: "SocialGouv/0", wantErr: true},
		{in: "SocialGouv/-1", wantErr: true},
		{in: "/203", wantErr: true},
	} {
		got, err := ParseProjectRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProjectRef(%q) = %+v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProjectRef(%q): %v", tc.in, err)
			continue
		}
		if got.Owner != tc.wantOwn || got.Number != tc.wantNum {
			t.Errorf("ParseProjectRef(%q) = %+v", tc.in, got)
		}
		if got.String() != tc.wantOwn+"/203" {
			t.Errorf("String() = %q", got.String())
		}
	}
}

func TestProjectOwnerKindDefaultsToOrg(t *testing.T) {
	var zero ProjectOwnerKind
	if zero.Valid() {
		t.Error("the zero owner kind must not validate on its own")
	}
	if zero.OrDefault() != ProjectOwnerOrg {
		t.Errorf("OrDefault() = %q, want %q", zero.OrDefault(), ProjectOwnerOrg)
	}
	if ProjectOwnerKind("group").OrDefault() != "group" {
		t.Error("OrDefault must leave a typo visible, not silently correct it")
	}
	if err := (ProjectRef{Owner: "o", Number: 1, OwnerKind: "group"}).Validate(); err == nil {
		t.Error("an unknown owner kind must fail validation")
	}
}

// TestDefaultStatusMappingIsInjective is the property the two-way sync rests
// on: every board status maps to exactly one native state and back. A
// many-to-one map would make the round trip lossy — an import would drag a
// card out of the state a reflect had just left it in.
func TestDefaultStatusMappingIsInjective(t *testing.T) {
	m := DefaultStatusMapping()
	seenStatus := map[string]bool{}
	seenState := map[string]bool{}
	for _, p := range m {
		if seenStatus[p.Status] {
			t.Errorf("status %q mapped twice", p.Status)
		}
		if seenState[p.State] {
			t.Errorf("state %q mapped twice", p.State)
		}
		seenStatus[p.Status], seenState[p.State] = true, true

		state, ok := StateForStatus(m, p.Status)
		if !ok || state != p.State {
			t.Errorf("StateForStatus(%q) = %q,%v", p.Status, state, ok)
		}
		status, ok := StatusForState(m, p.State)
		if !ok || status != p.Status {
			t.Errorf("StatusForState(%q) = %q,%v", p.State, status, ok)
		}
	}
}

func TestStatusMappingLookupsAreFolded(t *testing.T) {
	m := DefaultStatusMapping()
	if got, ok := StateForStatus(m, " IN progress "); !ok || got != "in_progress" {
		t.Errorf("status lookup must be trimmed + case-insensitive, got %q,%v", got, ok)
	}
	if _, ok := StateForStatus(m, "Icebox"); ok {
		t.Error("an unmapped status must report false, never a zero-value state")
	}
	if _, ok := StatusForState(m, "review"); ok {
		t.Error("an unmapped native state must be inert, not collapsed onto a neighbour")
	}
}

// TestStatusMappingFromMap covers the operator escape hatch: the five shipped
// column names are a DEFAULT, so a board reading "Todo"/"Doing" binds by
// naming them. The one thing it refuses is a non-injective map — two columns
// on one state make the reverse direction ambiguous, so the reflect would pick
// one and the next import would read the other back and undo it.
func TestStatusMappingFromMap(t *testing.T) {
	got, err := StatusMappingFromMap(map[string]string{
		"Todo": "ready", "Doing": "in_progress", "Shipped": "done",
	})
	if err != nil {
		t.Fatalf("StatusMappingFromMap: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d pairs, want 3: %+v", len(got), got)
	}
	// Sorted by state, so a stored map is stable across runs.
	if got[0].State != "done" || got[1].State != "in_progress" || got[2].State != "ready" {
		t.Errorf("mapping is not sorted by state: %+v", got)
	}
	if s, ok := StateForStatus(got, "todo"); !ok || s != "ready" {
		t.Errorf("StateForStatus(todo) = %q,%v", s, ok)
	}
	if s, ok := StatusForState(got, "done"); !ok || s != "Shipped" {
		t.Errorf("StatusForState(done) = %q,%v", s, ok)
	}
}

func TestStatusMappingFromMapRefusesAmbiguity(t *testing.T) {
	_, err := StatusMappingFromMap(map[string]string{"Todo": "ready", "Next": "ready"})
	if err == nil {
		t.Fatal("want an error: two columns on one state make the reverse direction ambiguous")
	}
	for _, want := range []string{"injective", "ready"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name the collision (%q missing): %q", want, err)
		}
	}
	if _, err := StatusMappingFromMap(nil); err == nil {
		t.Error("an empty map must be an error, not a silent fallback to the default")
	}
	if _, err := StatusMappingFromMap(map[string]string{"Todo": "  "}); err == nil {
		t.Error("an empty state must be an error")
	}
}

func TestSlugifyLabelValue(t *testing.T) {
	for in, want := range map[string]string{
		"cloud/ops":   "cloud-ops",
		"engine":      "engine",
		"In progress": "in-progress",
		"P0":          "p0",
		"  spaced  ":  "spaced",
		"a--b":        "a-b",
		"-lead":       "lead",
		"trail-":      "trail",
		"":            "",
		"///":         "",
		"v1.2_x":      "v1.2_x",
	} {
		if got := SlugifyLabelValue(in); got != want {
			t.Errorf("SlugifyLabelValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFieldLabel(t *testing.T) {
	if got := FieldLabel("area:", "cloud/ops"); got != "area:cloud-ops" {
		t.Errorf("FieldLabel = %q", got)
	}
	if got := FieldLabel("area:", "   "); got != "" {
		t.Errorf("an empty value must yield no label, got %q — never a dangling prefix", got)
	}
}

func TestProjectFieldLookups(t *testing.T) {
	p := Project{Fields: []ProjectField{
		{ID: "f1", Name: "Status", Options: []ProjectFieldOption{{ID: "o1", Name: "In progress"}}},
		{ID: "f2", Name: "Target", DataType: "DATE"},
	}}
	f, ok := p.Field("status")
	if !ok || f.ID != "f1" || !f.SingleSelect() {
		t.Fatalf("Field(status) = %+v,%v", f, ok)
	}
	if opt, ok := f.Option("IN PROGRESS"); !ok || opt.ID != "o1" {
		t.Errorf("Option = %+v,%v", opt, ok)
	}
	if opt, ok := f.OptionByID("o1"); !ok || opt.Name != "In progress" {
		t.Errorf("OptionByID = %+v,%v", opt, ok)
	}
	if d, _ := p.Field("Target"); d.SingleSelect() {
		t.Error("a DATE field is not single-select")
	}
	if _, ok := p.Field("Nope"); ok {
		t.Error("a missing field must report false")
	}
}

func TestProjectItemFieldLookup(t *testing.T) {
	it := ProjectItem{Fields: []ProjectItemField{
		{FieldID: "f1", FieldName: "Status", Value: "Done"},
	}}
	if fv, ok := it.Field("STATUS"); !ok || fv.Value != "Done" {
		t.Errorf("Field = %+v,%v", fv, ok)
	}
	if _, ok := it.Field("Area"); ok {
		t.Error("an unset field must report false")
	}
}

// TestDefaultLabelFieldsNamespaces pins the three namespaces the rest of the
// engine treats as board-local; renaming one without updating that list would
// let the next issue import strip the labels.
func TestDefaultLabelFieldsNamespaces(t *testing.T) {
	var prefixes []string
	for _, lf := range DefaultLabelFields() {
		if lf.Field == "" || lf.Prefix == "" {
			t.Fatalf("incomplete label field: %+v", lf)
		}
		prefixes = append(prefixes, lf.Prefix)
	}
	for _, want := range []string{"area:", "mode:", "prio:"} {
		if !slices.Contains(prefixes, want) {
			t.Errorf("prefixes %v missing %q", prefixes, want)
		}
	}
}
