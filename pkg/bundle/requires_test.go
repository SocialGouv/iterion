package bundle

import "testing"

// A manifest may state the engine it needs. The contract is greppable and
// total: one accepted operator, dotted numeric versions, and an explicit
// error on anything else — a requirement iterion cannot read must never be
// a requirement iterion ignores.

func TestRequiresParsesTheAcceptedGrammar(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{
		{">= 3.112.14", []int{3, 112, 14}},
		{">=3.112.14", []int{3, 112, 14}},
		{">= v3.112.14", []int{3, 112, 14}},
		{"3.112.14", []int{3, 112, 14}},
		{"  >=   3.116  ", []int{3, 116}},
	} {
		got, err := ParseEngineConstraint(tc.in)
		if err != nil {
			t.Fatalf("ParseEngineConstraint(%q): %v", tc.in, err)
		}
		if len(got.Min) != len(tc.want) {
			t.Fatalf("ParseEngineConstraint(%q).Min = %v, want %v", tc.in, got.Min, tc.want)
		}
		for i := range tc.want {
			if got.Min[i] != tc.want[i] {
				t.Fatalf("ParseEngineConstraint(%q).Min = %v, want %v", tc.in, got.Min, tc.want)
			}
		}
	}
}

func TestRequiresRefusesWhatItCannotEnforce(t *testing.T) {
	for _, in := range []string{
		"", "  ",
		"< 3.112.14",  // an upper bound is a different contract
		"^3.112.14",   // caret/tilde ranges are not a grammar we enforce
		"~> 3.112",    //
		"== 3.112.14", // equality would pin, not floor
		">= latest",   // not a version
		">= 3.112.14-rc1",
		">= ",
	} {
		if got, err := ParseEngineConstraint(in); err == nil {
			t.Errorf("ParseEngineConstraint(%q) = %+v, nil — an unreadable requirement must be an explicit error, never an ignored field", in, got)
		}
	}
}

func TestDecodeManifestRefusesAnUnreadableRequirement(t *testing.T) {
	_, err := DecodeManifest([]byte("name: x\nrequires:\n  iterion: \"^3.1\"\n"), "t")
	if err == nil {
		t.Fatal("decodeManifest accepted requires.iterion \"^3.1\" — an unparsable requirement is exactly the failure mode the field exists to close")
	}
}

func TestDecodeManifestRefusesAnUnknownRequirementKey(t *testing.T) {
	// A requirement this build does not know must refuse the bundle, not be
	// dropped: silence would make the contract a decoration.
	_, err := DecodeManifest([]byte("name: x\nrequires:\n  features: [expr.variadic_minmax]\n"), "t")
	if err == nil {
		t.Fatal("decodeManifest accepted an unknown key under requires: — an unknown requirement must be an explicit error")
	}
}

func TestCheckEngineVerdicts(t *testing.T) {
	m, err := DecodeManifest([]byte("name: x\nrequires:\n  iterion: \">= 3.112.14\"\n"), "t")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tc := range []struct {
		build string
		want  EngineVerdict
	}{
		{"v3.112.7+abc123", EngineTooOld},
		{"v3.112.14", EngineOK},
		{"v3.116.4+deadbeef", EngineOK},
		{"3.113", EngineOK},
		{"dev", EngineUnknown},
		{"", EngineUnknown},
	} {
		got, reason := CheckManifestEngine(m, tc.build)
		if got != tc.want {
			t.Errorf("CheckManifestEngine(build %q) = %v (%s), want %v", tc.build, got, reason, tc.want)
		}
		if got != EngineOK && reason == "" {
			t.Errorf("CheckManifestEngine(build %q) = %v with an empty reason — a refusal must say what it wanted and what it found", tc.build, got)
		}
	}
}

func TestCheckManifestEngineIsQuietWithoutARequirement(t *testing.T) {
	m, err := DecodeManifest([]byte("name: x\n"), "t")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := CheckManifestEngine(m, "dev"); got != EngineOK {
		t.Errorf("a manifest with no requires: got %v, want EngineOK — the field is opt-in", got)
	}
	if got, _ := CheckManifestEngine(nil, "dev"); got != EngineOK {
		t.Errorf("a nil manifest got %v, want EngineOK", got)
	}
}
