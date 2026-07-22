package native

import (
	"sync"
	"testing"
)

// TestCreateUniqueTitle_SequentialPrefixes proves the atomic helper leaves a
// free title verbatim and prefixes "#N - " for each subsequent clash.
func TestCreateUniqueTitle_SequentialPrefixes(t *testing.T) {
	s := newTestStore(t)
	first, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"})
	if err != nil {
		t.Fatalf("CreateUniqueTitle #1: %v", err)
	}
	if first.Title != "Episode" {
		t.Fatalf("first title = %q; want %q", first.Title, "Episode")
	}
	second, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"})
	if err != nil {
		t.Fatalf("CreateUniqueTitle #2: %v", err)
	}
	if second.Title != "#2 - Episode" {
		t.Fatalf("second title = %q; want %q", second.Title, "#2 - Episode")
	}
	third, err := s.CreateUniqueTitle(Issue{Title: "Episode", Bot: "b"})
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
			iss, err := s.CreateUniqueTitle(Issue{Title: "Race", Bot: "b"})
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
	if _, err := s.CreateUniqueTitle(Issue{Title: "   ", Bot: "b"}); err == nil {
		t.Fatalf("CreateUniqueTitle(blank): expected error, got nil")
	}
}
