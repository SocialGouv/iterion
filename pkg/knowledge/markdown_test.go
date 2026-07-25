package knowledge

import (
	"reflect"
	"testing"
)

func TestParseMarkdownMeta(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		title, desc string
		tags        []string
	}{
		{
			"frontmatter",
			"---\ntitle: Brief\ndescription: a note\ntags: [a, b]\n---\nbody",
			"Brief", "a note", []string{"a", "b"},
		},
		{"h1 fallback", "# Heading\n\nbody", "Heading", "", nil},
		{"quoted title", "---\ntitle: \"Quoted X\"\n---\n", "Quoted X", "", nil},
		{"empty", "", "", "", nil},
		{"plain body no title", "just text\nmore", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, desc, tags := ParseMarkdownMeta([]byte(c.in))
			if title != c.title || desc != c.desc || !reflect.DeepEqual(tags, c.tags) {
				t.Fatalf("got (%q,%q,%v) want (%q,%q,%v)", title, desc, tags, c.title, c.desc, c.tags)
			}
		})
	}
}

func TestParseMarkdownMetaEdgeCases(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		title, desc string
		tags        []string
	}{
		{
			"tags without brackets",
			"---\ntags: a, b\n---\n",
			"", "", []string{"a", "b"},
		},
		{
			"single-quoted values",
			"---\ntitle: 'Solo'\ndescription: 'd'\n---\n",
			"Solo", "d", nil,
		},
		{
			"colon inside value kept",
			"---\ntitle: a: b\n---\n",
			"a: b", "", nil,
		},
		{
			"frontmatter without title falls back to body h1",
			"---\ndescription: d\n---\n# Body Title\n",
			"Body Title", "d", nil,
		},
		// Characterization: the H1 fallback scans up to 30 lines even when
		// the document starts with plain (non-heading) prose.
		{
			"h1 later in body",
			"intro line\n# Later\n",
			"Later", "", nil,
		},
		// Characterization: an unterminated frontmatter block still yields
		// its parsed keys (the whole doc is treated as frontmatter).
		{
			"unterminated frontmatter",
			"---\ntitle: X",
			"X", "", nil,
		},
		{"h1 without space is not a title", "#NoSpace\n", "", "", nil},
		{"h2 is not a title", "## Sub\n", "", "", nil},
		{"empty bracket tags", "---\ntags: []\n---\n", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, desc, tags := ParseMarkdownMeta([]byte(c.in))
			if title != c.title || desc != c.desc || !reflect.DeepEqual(tags, c.tags) {
				t.Fatalf("got (%q,%q,%v) want (%q,%q,%v)", title, desc, tags, c.title, c.desc, c.tags)
			}
		})
	}
}

func TestParseTagListWhitespaceBrackets(t *testing.T) {
	// Characterization: "[]" yields nil, but "[ ]" yields an empty
	// NON-nil slice (the whitespace survives the bracket trim).
	if got := parseTagList("[]"); got != nil {
		t.Errorf("parseTagList(%q) = %v, want nil", "[]", got)
	}
	got := parseTagList("[ ]")
	if got == nil || len(got) != 0 {
		t.Errorf("parseTagList(%q) = %v, want empty non-nil slice", "[ ]", got)
	}
}

func TestChecksumHex(t *testing.T) {
	a := ChecksumHex([]byte("hello"))
	if len(a) != 64 || a != ChecksumHex([]byte("hello")) {
		t.Fatalf("checksum not stable/hex: %q", a)
	}
	if a == ChecksumHex([]byte("world")) {
		t.Fatal("distinct content must differ")
	}
}
