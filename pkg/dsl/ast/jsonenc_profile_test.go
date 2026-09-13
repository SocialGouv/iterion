package ast

import (
	"strings"
	"testing"
)

// The syntax profile rides the JSON transport: a document that came from a
// `dsl: 2` file is written back as one (the studio's save path), and a
// profile-1 document carries no field at all, so an older reader of the
// transport sees exactly what it saw before the field existed.
func TestProfileSurvivesTheJSONTransport(t *testing.T) {
	f := &File{Profile: 2, Prompts: []*PromptDecl{{Name: "p", Body: "x"}}}
	data, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"profile": 2`) {
		t.Fatalf("profile not written: %s", data)
	}
	back, err := UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Profile != 2 || back.EffectiveProfile() != 2 {
		t.Fatalf("profile read back as %d", back.Profile)
	}

	v1 := &File{Prompts: []*PromptDecl{{Name: "p", Body: "x"}}}
	data, err = MarshalFile(v1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "profile") {
		t.Fatalf("a profile-1 document must carry no profile field: %s", data)
	}
	back, err = UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Profile != 0 || back.EffectiveProfile() != DefaultProfile {
		t.Fatalf("headerless document read back as profile %d (effective %d)", back.Profile, back.EffectiveProfile())
	}
}

// A document with no header — or built in memory — is profile 1; the
// zero value never means "no profile".
func TestEffectiveProfileOfAZeroValueFileIsOne(t *testing.T) {
	var f File
	if f.EffectiveProfile() != 1 {
		t.Fatalf("zero value reads as profile %d", f.EffectiveProfile())
	}
	f.Profile = -3
	if f.EffectiveProfile() != 1 {
		t.Fatalf("a negative profile reads as %d", f.EffectiveProfile())
	}
}
