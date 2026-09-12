package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A floor's presence is not enough: a profile-2 bundle declaring `>= 0.0.1`
// admits every runner that cannot read it. The predicate compares the
// declared floor with the release that reads the profile, and is what the
// author's C252 and the deployment's 409 both ask.
func TestCheckProfileFloorReadsTheFloorsHeight(t *testing.T) {
	with := func(req string) *Manifest {
		if req == "" {
			return &Manifest{Name: "p"}
		}
		return &Manifest{Name: "p", Requires: &Requires{Iterion: req}}
	}
	cases := []struct {
		name     string
		m        *Manifest
		profile  int
		ok       bool
		declared string
	}{
		{"profile 1 needs nothing", with(""), 1, true, ""},
		{"profile 2, no manifest", nil, 2, false, ""},
		{"profile 2, no floor", with(""), 2, false, ""},
		{"profile 2, a floor below the release", with(">= 0.0.1"), 2, false, ">= 0.0.1"},
		{"profile 2, the release itself", with(">= 3.141.0"), 2, true, ">= 3.141.0"},
		{"profile 2, a bare version at the release", with("v3.141.0"), 2, true, "v3.141.0"},
		{"profile 2, a floor above", with(">= 3.200.0"), 2, true, ">= 3.200.0"},
		{"profile 2, one patch below", with(">= 3.140.9"), 2, false, ">= 3.140.9"},
		{"a profile with no release on record takes any floor", with(">= 0.0.1"), 7, true, ">= 0.0.1"},
		{"a profile with no release on record still needs one", with(""), 7, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pf := CheckProfileFloor(c.m, c.profile)
			if pf.OK != c.ok || pf.Declared != c.declared || pf.Profile != c.profile {
				t.Fatalf("CheckProfileFloor = %+v, want ok=%v declared=%q", pf, c.ok, c.declared)
			}
			if c.profile == 2 && pf.Need != "3.141.0" {
				t.Fatalf("need %q", pf.Need)
			}
		})
	}
}

// A sibling bundle (`../other/main.bot`) is a child shape the runner resolves
// within the bundle's collection: on disk the walk reads it and its profile
// counts; a files map cannot reach it and names it unread instead of
// counting it as profile 1. A link out of the collection is unread too.
func TestMaxSyntaxProfileReadsASiblingAndNamesWhatItCannot(t *testing.T) {
	coll := t.TempDir()
	main := filepath.Join(coll, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(coll, "sib", "kids"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainSrc := "subbot s:\n  source: \"../sib/main.bot\"\n\nworkflow w:\n  entry: s\n  s -> done\n"
	_ = os.WriteFile(filepath.Join(main, "main.bot"), []byte(mainSrc), 0o644)
	_ = os.WriteFile(filepath.Join(coll, "sib", "main.bot"), []byte("subbot k:\n  source: \"kids/k.bot\"\n\nworkflow w:\n  entry: k\n  k -> done\n"), 0o644)
	_ = os.WriteFile(filepath.Join(coll, "sib", "kids", "k.bot"), []byte("dsl: 2\nagent a:\n  description: \"x\"\n"), 0o644)

	profile, by, unread := MaxSyntaxProfileDir(main)
	if profile != 2 || !reflect.DeepEqual(by, []string{"../sib/kids/k.bot"}) || len(unread) != 0 {
		t.Fatalf("dir: profile %d by %v unread %v", profile, by, unread)
	}
	// The same bundle as a files map: the sibling is beyond it.
	profile, by, unread = MaxSyntaxProfile(map[string]string{"main.bot": mainSrc})
	if profile != 1 || len(by) != 0 || !reflect.DeepEqual(unread, []string{"../sib/main.bot"}) {
		t.Fatalf("map: profile %d by %v unread %v", profile, by, unread)
	}

	// A link that leaves the collection is not followed, and is named.
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "evil.bot"), []byte("dsl: 2\nagent a:\n  description: \"x\"\n"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "evil.bot"), filepath.Join(main, "link.bot")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_ = os.WriteFile(filepath.Join(main, "main.bot"), []byte("subbot l:\n  source: \"link.bot\"\n\nworkflow w:\n  entry: l\n  l -> done\n"), 0o644)
	profile, by, unread = MaxSyntaxProfileDir(main)
	if profile != 1 || len(by) != 0 || !reflect.DeepEqual(unread, []string{"link.bot"}) {
		t.Fatalf("symlink out: profile %d by %v unread %v", profile, by, unread)
	}
	// Two levels up leave the collection by construction; an absolute path
	// is never read.
	_ = os.WriteFile(filepath.Join(main, "main.bot"), []byte("subbot up:\n  source: \"../../x/main.bot\"\n\nsubbot abs:\n  source: \"/etc/x.bot\"\n\nworkflow w:\n  entry: up\n  up -> abs\n  abs -> done\n"), 0o644)
	_, _, unread = MaxSyntaxProfileDir(main)
	if !reflect.DeepEqual(unread, []string{"../../x/main.bot", "/etc/x.bot"}) {
		t.Fatalf("escapes: unread %v", unread)
	}
}
