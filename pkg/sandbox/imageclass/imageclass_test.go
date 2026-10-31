package imageclass

import (
	"slices"
	"testing"
)

// The vocabulary is the contract three packages read: a class the tree
// builds (sandbox/<class>/Dockerfile) must be listed, and a listed class
// must build — pkg/runtime's TestTheStockSandboxFamilyIsTheOneTheTreeBuilds
// holds the list to the tree; this test pins the list's own content so a
// rename fails loudly on both sides.
func TestTheClassVocabularyIsThePublishedFamily(t *testing.T) {
	want := []string{"slim", "full", "sec", "browser", "rgaa"}
	if got := Classes(); !slices.Equal(got, want) {
		t.Fatalf("Classes() = %v, want %v", got, want)
	}
	// A mutation through the returned slice must not move the vocabulary.
	Classes()[0] = "mutated"
	if !Known("slim") {
		t.Fatal("a caller moved the vocabulary through the returned slice")
	}
}

func TestClassLookup(t *testing.T) {
	for _, class := range Classes() {
		if !Known(class) {
			t.Errorf("Known(%q) = false on a listed class", class)
		}
	}
	for _, not := range []string{"", "slim2", "SLIM", " slim"} {
		if Known(not) {
			t.Errorf("Known(%q) = true — the vocabulary is exact, not fuzzy", not)
		}
	}
	if !CarriesBrowser("browser") || !CarriesBrowser("rgaa") {
		t.Error("browser and rgaa carry Chromium — rgaa builds FROM the browser base")
	}
	if CarriesBrowser("slim") {
		t.Error("slim carries no browser")
	}
}

func TestLooksLikeClassName(t *testing.T) {
	for _, yes := range []string{"slim", "rgaa", "my-local-image", "alpine"} {
		if !LooksLikeClassName(yes) {
			t.Errorf("LooksLikeClassName(%q) = false, want true — a bare word", yes)
		}
	}
	for _, no := range []string{"", "ghcr.io/x/y:1", "alpine:3", "img@sha256:ab", "${IMG}", "{{vars.img}}", "two words"} {
		if LooksLikeClassName(no) {
			t.Errorf("LooksLikeClassName(%q) = true — a reference or indirection is never a class", no)
		}
	}
}
