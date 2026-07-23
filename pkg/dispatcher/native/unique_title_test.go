package native

import (
	"strings"
	"sync"
	"testing"
)

// TestCreateUniqueTitle_SequentialPrefixes proves the atomic helper leaves a
// free title verbatim and prefixes "#N - " for each subsequent clash.
func TestCreateUniqueTitle_SequentialPrefixes(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"}, nil)
	if err != nil {
		t.Fatalf("CreateUniqueTitle #1: %v", err)
	}
	if first.Title != "Episode" {
		t.Fatalf("first title = %q; want %q", first.Title, "Episode")
	}
	second, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"}, nil)
	if err != nil {
		t.Fatalf("CreateUniqueTitle #2: %v", err)
	}
	if second.Title != "#2 - Episode" {
		t.Fatalf("second title = %q; want %q", second.Title, "#2 - Episode")
	}
	third, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"}, nil)
	if err != nil {
		t.Fatalf("CreateUniqueTitle #3: %v", err)
	}
	if third.Title != "#3 - Episode" {
		t.Fatalf("third title = %q; want %q", third.Title, "#3 - Episode")
	}
}

// TestCreateUniqueTitle_ConcurrentNoDuplicates is the M4 regression: N
// goroutines racing to create the SAME desired title must all land distinct
// titles. The list-then-check approach that CreateUniqueTitle replaces would
// let two of them observe the same "taken" set and duplicate a title.
func TestCreateUniqueTitle_ConcurrentNoDuplicates(t *testing.T) {
	s := newTestStore(t)
	const n = 24
	var wg sync.WaitGroup
	titles := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			iss, err := s.CreateUniqueTitle(Issue{Title: "Race", Bot: "b"}, nil)
			if err != nil {
				errs[idx] = err
				return
			}
			titles[idx] = iss.Title
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("CreateUniqueTitle[%d]: %v", i, errs[i])
		}
		seen[titles[i]]++
	}
	for title, count := range seen {
		if count != 1 {
			t.Errorf("title %q assigned %d times; want exactly 1", title, count)
		}
	}
	if len(seen) != n {
		t.Errorf("got %d distinct titles across %d creates; want %d", len(seen), n, n)
	}
	// One of them keeps the bare desired title; the rest are "#N - Race".
	if _, ok := seen["Race"]; !ok {
		t.Errorf("no create kept the bare desired title %q; got %v", "Race", keysOf(seen))
	}
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCreateUniqueTitle_EmptyTitleRejected keeps createLocked's validation on
// the atomic path.
func TestCreateUniqueTitle_EmptyTitleRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateUniqueTitle(Issue{Title: "   ", Bot: "b"}, nil); err == nil {
		t.Fatalf("CreateUniqueTitle(blank): expected error, got nil")
	}
}

// TestCreateUniqueTitle_NormalizeKeepsBudget proves the normalizer re-truncates
// each "#N - " candidate so prefixing an already-max-length title never
// overflows the caller's rune budget — the bug where a compacted 80-rune board
// title became 85 once the atomic path prepended "#2 - ".
func TestCreateUniqueTitle_NormalizeKeepsBudget(t *testing.T) {
	s := newTestStore(t)
	const max = 20
	norm := func(x string) string {
		r := []rune(x)
		if len(r) <= max {
			return x
		}
		return strings.TrimSpace(string(r[:max-1])) + "…"
	}
	long := strings.Repeat("é", max+10) // over the cap

	first, err := s.CreateUniqueTitle(Issue{Title: long, Bot: "b"}, norm)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if n := len([]rune(first.Title)); n != max {
		t.Fatalf("first title runes=%d, want %d (%q)", n, max, first.Title)
	}
	second, err := s.CreateUniqueTitle(Issue{Title: long, Bot: "b"}, norm)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if n := len([]rune(second.Title)); n != max || !strings.HasPrefix(second.Title, "#2 - ") {
		t.Fatalf("second title = %q (runes=%d), want a #2- prefix within %d runes", second.Title, n, max)
	}
	if first.Title == second.Title {
		t.Fatalf("unique variant collided with the base: %q", first.Title)
	}
}
