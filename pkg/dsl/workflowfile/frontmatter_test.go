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
	fm, extra, err := DecodeFrontmatter("name: first\ndescription: d\nname: last\nowner: jo\ntriggers: [a, b]\nowner: jo\nzeta:\n  deep: 1")
	if err != nil {
		t.Fatal(err)
	}
	if fm.Name != "last" || fm.Description != "d" || !reflect.DeepEqual(fm.Triggers, []string{"a", "b"}) || len(fm.Capabilities) != 0 {
		t.Fatalf("identity = %+v", fm)
	}
	if !reflect.DeepEqual(extra, []string{"owner", "zeta"}) {
		t.Fatalf("extra = %q, want owner and zeta once each, sorted", extra)
	}
	if _, _, err := DecodeFrontmatter("name: probe\ntriggers: just-one"); err == nil {
		t.Fatal("a scalar where a list is expected was read")
	}
	fm, extra, err = DecodeFrontmatter("")
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
