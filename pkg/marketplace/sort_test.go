package marketplace

import (
	"context"
	"testing"
)

// seedSortEntries populates the store with three entries whose popular /
// recent / name orders are all distinct, so each sort mode is
// distinguishable from the others.
func seedSortEntries(t *testing.T, s *JSONStore) {
	t.Helper()
	ctx := context.Background()
	entries := []Entry{
		{
			Slug:      "zeta",
			Name:      "zeta", // no DisplayName → name key "zeta"
			Installs:  5,
			UpdatedAt: "2026-07-01T00:00:00Z",
		},
		{
			Slug:        "beta-labeled",
			Name:        "zzz_internal",
			DisplayName: "Beta Label", // DisplayName wins → key "beta label"
			Installs:    1,
			UpdatedAt:   "2026-07-03T00:00:00Z",
		},
		{
			Slug:      "aardvark",
			Name:      "Aardvark", // case-insensitive → key "aardvark"
			Installs:  3,
			UpdatedAt: "2026-07-02T00:00:00Z",
		},
	}
	for _, e := range entries {
		if err := s.Upsert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

func listSlugs(t *testing.T, s *JSONStore, q Query) []string {
	t.Helper()
	out, err := s.List(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	slugs := make([]string, len(out))
	for i, e := range out {
		slugs[i] = e.Slug
	}
	return slugs
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestJSONStore_ListSortPopular(t *testing.T) {
	s := newTestStore(t)
	seedSortEntries(t, s)
	want := []string{"zeta", "aardvark", "beta-labeled"} // installs 5, 3, 1
	assertOrder(t, listSlugs(t, s, Query{}), want)
	assertOrder(t, listSlugs(t, s, Query{Sort: SortPopular}), want)
}

func TestJSONStore_ListSortRecent(t *testing.T) {
	s := newTestStore(t)
	seedSortEntries(t, s)
	// UpdatedAt desc: 07-03, 07-02, 07-01.
	assertOrder(t, listSlugs(t, s, Query{Sort: SortRecent}),
		[]string{"beta-labeled", "aardvark", "zeta"})
}

func TestJSONStore_ListSortName(t *testing.T) {
	s := newTestStore(t)
	seedSortEntries(t, s)
	// Case-insensitive DisplayName-or-Name asc: aardvark, beta label, zeta.
	assertOrder(t, listSlugs(t, s, Query{Sort: SortName}),
		[]string{"aardvark", "beta-labeled", "zeta"})
}

func TestJSONStore_ListUnknownSortErrors(t *testing.T) {
	s := newTestStore(t)
	seedSortEntries(t, s)
	if _, err := s.List(context.Background(), Query{Sort: "bogus"}); err == nil {
		t.Fatal("expected List with an unknown sort to error")
	}
}

func TestValidateSort(t *testing.T) {
	for _, ok := range []string{"", SortPopular, SortRecent, SortName} {
		if err := ValidateSort(ok); err != nil {
			t.Errorf("ValidateSort(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"bogus", "Popular", "installs"} {
		if err := ValidateSort(bad); err == nil {
			t.Errorf("ValidateSort(%q) = nil, want error", bad)
		}
	}
}
