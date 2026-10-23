package workflowfile

import (
	"reflect"
	"testing"
)

// The one reading of a catalog identity: yaml.v2 into the four keys — a key
// named twice keeps its last value, as the catalogue has always read it; a
// scalar where a list is expected is a block the reading does not read; the
// keys beyond the four are returned by name, sorted, each once; an empty
// block reads as an empty identity, not a refusal.
func TestDecodeFrontmatterIsTheOneReading(t *testing.T) {
	fm, extra, err := DecodeFrontmatter("name: first\ndescription: d\nname: last\nowner: jo\ntriggers: [a, b]\nowner: jo\nzeta:\n  deep: 1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Name != "last" || fm.Description != "d" || !reflect.DeepEqual(fm.Triggers, []string{"a", "b"}) || len(fm.Capabilities) != 0 {
		t.Fatalf("identity = %+v", fm)
	}
	if !reflect.DeepEqual(extra, []string{"owner", "zeta"}) {
		t.Fatalf("extra = %q, want owner and zeta once each, sorted", extra)
	}
	if _, _, err := DecodeFrontmatter("name: probe\ntriggers: just-one", 1<<20); err == nil {
		t.Fatal("a scalar where a list is expected was read")
	}
	fm, extra, err = DecodeFrontmatter("", 1<<20)
	if err != nil || !fm.Empty() || len(extra) != 0 {
		t.Fatalf("an empty block: %v %+v %q", err, fm, extra)
	}
	if !(*Frontmatter)(nil).Empty() || !(&Frontmatter{}).Empty() {
		t.Fatal("nothing is not empty")
	}
	if (&Frontmatter{Capabilities: []string{"x"}}).Empty() {
		t.Fatal("a capability is an identity")
	}
}

// A whole-value block is refused on the type the reading decodes it into:
// the named struct, so the remedy table keyed on that target names one
// stable Go type, and the causes the reading returns carry it.
func TestFrontmatterRefusalNamesItsDecodeTarget(t *testing.T) {
	_, _, err := DecodeFrontmatter("- a\n", 1<<20)
	if err == nil {
		t.Fatal("a list where the block's mapping was expected was read")
	}
	causes := YAMLCauses(YAMLv2, err)
	if len(causes) != 1 || causes[0].Message != YAMLUnmarshalFormat {
		t.Fatalf("the refusal's causes are %v: one unmarshal cause was expected", causes)
	}
	if got, want := causes[0].Target, reflect.TypeOf(frontmatterFields{}).String(); got != want {
		t.Errorf("the refusal names the decode target %q, not the reading's own type %q", got, want)
	}
	if got := causes[0].Line; got != 1 {
		t.Errorf("the refusal names line %d, not the block's own line 1", got)
	}
}

// The causes of a multi-key refusal: one per value the reading could not
// read, each its own target and its own line, in the reader's order.
func TestFrontmatterCausesPerValueRefused(t *testing.T) {
	_, _, err := DecodeFrontmatter("name: [a]\ntriggers: 3\n", 1<<20)
	if err == nil {
		t.Fatal("two keys of the wrong shape were read")
	}
	causes := YAMLCauses(YAMLv2, err)
	if len(causes) != 2 {
		t.Fatalf("the refusal's causes are %v: one per key refused", causes)
	}
	if causes[0].Target != "string" || causes[0].Line != 1 {
		t.Errorf("the first cause is %v: the name refused into string, on the block's line 1", causes[0])
	}
	if causes[1].Target != "[]string" || causes[1].Line != 2 {
		t.Errorf("the second cause is %v: the list key refused into []string, on the block's line 2", causes[1])
	}
}
