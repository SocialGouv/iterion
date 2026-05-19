package store

import (
	"fmt"
	"regexp"
	"testing"
)

var runNameFormat = regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+-[0-9a-f]{4}$`)

// uuidV7Format matches the canonical hyphenated form of UUIDv7. The
// third group must start with `7` (version) and the fourth must start
// with one of `8`, `9`, `a`, `b` (RFC 9562 variant bits).
var uuidV7Format = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

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
		{"pool1", runNamePool1},
		{"pool2", runNamePool2},
		{"pool3", runNamePool3},
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
	for _, list := range []struct {
		name  string
		items []string
	}{
		{"pool1", runNamePool1},
		{"pool2", runNamePool2},
		{"pool3", runNamePool3},
	} {
		if len(list.items) == 0 {
			t.Fatalf("%s must not be empty", list.name)
		}
		if len(list.items) > 65535 {
			t.Errorf("%s must fit a uint16 mod (got %d)", list.name, len(list.items))
		}
	}
}

func TestGenerateRunID_FormatAndUniqueness(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := GenerateRunID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !uuidV7Format.MatchString(id) {
			t.Fatalf("id %q does not match UUIDv7 format", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("collision: %q produced twice in %d calls", id, n)
		}
		seen[id] = struct{}{}
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
		{"", "obsidian-ripple-pizzazap-1c14"},
		{"hello", "retro-hunt-riotchord-a30e"},
		{"examples/x.iter:1", "meteor-vroom-arctickazoo-03cc"},
	}
	for _, c := range cases {
		got := GenerateRunName(c.seed)
		if got != c.want {
			t.Errorf("seed %q: got %q, want %q", c.seed, got, c.want)
		}
	}
}
