package smoke

import (
	"sync"
	"testing"
)

// The image must link actual libc code and initialize the race runtime as the
// non-root runner user, not merely have a binary called gcc on PATH.
func TestRaceAndCCompiler(t *testing.T) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sum := 0
	for range 8 {
		wg.Go(func() { mu.Lock(); sum += absolute(-7); mu.Unlock() })
	}
	wg.Wait()
	if sum != 56 {
		t.Fatalf("sum=%d", sum)
	}
}
