package store

import (
	"fmt"
	"regexp"
	"testing"
)

var runNameFormat = regexp.MustCompile(`^[a-z]+-[a-z]+-[0-9a-f]{4}$`)

func TestGenerateRunName_Deterministic(t *testing.T) {
	seeds := []string{
		"",
		"foo",
		"examples/hello.iter:run_1777532137550",
		"workflows/review.iter:custom_id",
		"a very long seed string that is unusual",
	}
	for _, s := range seeds {
		got := GenerateRunName(s)
		again := GenerateRunName(s)
		if got != again {
			t.Errorf("non-deterministic for seed %q: %q vs %q", s, got, again)
		}
	}
}

func TestGenerateRunName_Format(t *testing.T) {
	for i := 0; i < 1000; i++ {
		seed := fmt.Sprintf("seed-%d", i)
		name := GenerateRunName(seed)
		if !runNameFormat.MatchString(name) {
			t.Errorf("seed %q produced %q which does not match %s", seed, name, runNameFormat)
		}
	}
}

func TestGenerateRunName_Uniqueness(t *testing.T) {
	// Spot-check that distinct seeds tend to produce distinct names.
	// Not a strict guarantee (collisions are mathematically possible)
	// but a tight bound catches accidental constant outputs.
	seen := make(map[string]string, 1000)
	for i := 0; i < 1000; i++ {
		seed := fmt.Sprintf("seed-%d", i)
		name := GenerateRunName(seed)
		if prev, ok := seen[name]; ok {
			t.Logf("collision at i=%d: seeds %q and %q both → %q", i, prev, seed, name)
		}
		seen[name] = seed
	}
	if len(seen) < 990 {
		t.Errorf("too many collisions: %d unique out of 1000", len(seen))
	}
}

func TestRunNameLists_NoDuplicates(t *testing.T) {
	for _, list := range []struct {
		name  string
		items []string
	}{
		{"adjectives", runNameAdjectives},
		{"nouns", runNameNouns},
	} {
		seen := make(map[string]int, len(list.items))
		for i, w := range list.items {
			if prev, ok := seen[w]; ok {
				t.Errorf("%s: duplicate %q at indices %d and %d", list.name, w, prev, i)
			}
			seen[w] = i
		}
	}
}

func TestRunNameLists_Sized(t *testing.T) {
	if len(runNameAdjectives) == 0 {
		t.Fatal("runNameAdjectives must not be empty")
	}
	if len(runNameNouns) == 0 {
		t.Fatal("runNameNouns must not be empty")
	}
	if len(runNameAdjectives) > 65535 {
		t.Errorf("adjectives must fit a uint16 mod (got %d)", len(runNameAdjectives))
	}
	if len(runNameNouns) > 65535 {
		t.Errorf("nouns must fit a uint16 mod (got %d)", len(runNameNouns))
	}
}

func TestGenerateRunName_Golden(t *testing.T) {
	// Golden cases pin the seed → name mapping. If a future change to
	// the word lists or algorithm shifts these mappings, this test
	// fails — making the regression visible. Update only when the
	// shift is intentional.
	cases := []struct {
		seed string
		want string
	}{
		{"", "wide-elm-98fc"},
		{"hello", "mellow-orion-5fb0"},
		{"examples/x.iter:1", "gentle-steel-6ed5"},
	}
	for _, c := range cases {
		got := GenerateRunName(c.seed)
		if got != c.want {
			t.Errorf("seed %q: got %q, want %q", c.seed, got, c.want)
		}
	}
}
