package git

import "testing"

// ParseStatusPorcelainZ is the one reader of git's status porcelain (#1577).
// The witnesses are the #1577 defect cases in the `-z` form the parser
// consumes: a rename carries destination and source as two NUL-separated
// fields (destination first), so a source literally named `x -> y.md` — or
// holding a non-ASCII byte — can no longer be cut at the wrong " -> ", the
// misread the two line-oriented parsers disagreed on.
func TestParseStatusPorcelainZ(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []StatusRecord
	}{
		{"empty", "", nil},
		{
			"plain records, untracked and ignored",
			" M devbox.lock\x00?? .claude/skills/a.md\x00!! .claude/\x00",
			[]StatusRecord{
				{Status: " M", Path: "devbox.lock"},
				{Status: "??", Path: ".claude/skills/a.md"},
				{Status: "!!", Path: ".claude/"},
			},
		},
		{
			"rename: destination first, source second",
			"R  src/new/name.txt\x00old/name.txt\x00",
			[]StatusRecord{{Status: "R ", Path: "src/new/name.txt", Source: "old/name.txt"}},
		},
		{
			"rename whose source holds an arrow, unquoted (#1577)",
			"R  z.md\x00x -> y.md\x00",
			[]StatusRecord{{Status: "R ", Path: "z.md", Source: "x -> y.md"}},
		},
		{
			"rename whose source holds an arrow and a non-ASCII byte (#1577)",
			"R  z.md\x00x -> é.md\x00",
			[]StatusRecord{{Status: "R ", Path: "z.md", Source: "x -> é.md"}},
		},
		{
			"rename destination the probes must not read as the mirror (#1577)",
			"R  z.md\x00x -> .claude/evil.md\x00",
			[]StatusRecord{{Status: "R ", Path: "z.md", Source: "x -> .claude/evil.md"}},
		},
		{
			"a rename followed by another record",
			"R  b.md\x00a.md\x00 M c.md\x00",
			[]StatusRecord{
				{Status: "R ", Path: "b.md", Source: "a.md"},
				{Status: " M", Path: "c.md"},
			},
		},
		{
			"a path holding a newline survives",
			"?? line\nbreak.md\x00",
			[]StatusRecord{{Status: "??", Path: "line\nbreak.md"}},
		},
		{
			"no trailing NUL on the last record",
			"?? a.md",
			[]StatusRecord{{Status: "??", Path: "a.md"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStatusPorcelainZ(tc.out)
			if err != nil {
				t.Fatalf("ParseStatusPorcelainZ: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseStatusPorcelainZ(%q) = %+v, want %+v", tc.out, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("record %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseStatusPorcelainZMalformed(t *testing.T) {
	for _, out := range []string{
		"?\x00",       // shorter than XY SP path
		"R  z.md\x00", // a rename missing its source record
		"R  z.md",     // same, unterminated
	} {
		if _, err := ParseStatusPorcelainZ(out); err == nil {
			t.Errorf("ParseStatusPorcelainZ(%q) = nil error, want malformed", out)
		}
	}
}
