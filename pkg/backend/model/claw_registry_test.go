package model

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// ---------------------------------------------------------------------------
// Registry unit tests (dedicated file for claw migration coverage)
// ---------------------------------------------------------------------------

// TestClawRegistry_ResolveSuccess verifies basic resolve and caching.
func TestClawRegistry_ResolveSuccess(t *testing.T) {
	r := NewRegistry()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("custom", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	m, err := r.Resolve("custom/my-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil client")
	}
}

// TestClawRegistry_ResolveCache verifies that repeated Resolve calls return
// the same cached instance.
func TestClawRegistry_ResolveCache(t *testing.T) {
	r := NewRegistry()
	callCount := 0
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("test", func(modelID string) (api.APIClient, error) {
		callCount++
		return mock, nil
	})

	m1, _ := r.Resolve("test/cached")
	m2, _ := r.Resolve("test/cached")

	if m1 != m2 {
		t.Error("expected same instance from cache")
	}
	if callCount != 1 {
		t.Errorf("factory called %d times, want 1 (cached)", callCount)
	}
}

// TestClawRegistry_ResolveUnknownProvider verifies error on unknown provider.
func TestClawRegistry_ResolveUnknownProvider(t *testing.T) {
	r := NewRegistry()
	_, err := r.Resolve("nonexistent/model")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// TestClawRegistry_ParseModelSpecValid verifies valid spec parsing.
func TestClawRegistry_ParseModelSpecValid(t *testing.T) {
	tests := []struct {
		spec     string
		provider string
		model    string
	}{
		{"anthropic/claude-sonnet-4-6", "anthropic", "claude-sonnet-4-6"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"bedrock/us.amazon.nova-pro-v1:0", "bedrock", "us.amazon.nova-pro-v1:0"},
	}

	for _, tt := range tests {
		p, m, err := ParseModelSpec(tt.spec)
		if err != nil {
			t.Errorf("ParseModelSpec(%q): unexpected error: %v", tt.spec, err)
			continue
		}
		if p != tt.provider || m != tt.model {
			t.Errorf("ParseModelSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, p, m, tt.provider, tt.model)
		}
	}
}

// TestClawRegistry_ParseModelSpecInvalid verifies error on invalid specs.
func TestClawRegistry_ParseModelSpecInvalid(t *testing.T) {
	invalid := []string{
		"no-slash",
		"/missing-provider",
		"trailing/",
		"",
	}
	for _, spec := range invalid {
		_, _, err := ParseModelSpec(spec)
		if err == nil {
			t.Errorf("ParseModelSpec(%q): expected error", spec)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency tests for the per-key sync.Once cache (BUG 5 fix).
// ---------------------------------------------------------------------------

// TestClawRegistry_ResolveConcurrentSameKey verifies that concurrent Resolve
// calls for the same spec invoke the factory exactly once, regardless of how
// long the factory takes. This is the core invariant of the per-key
// sync.Once cache that replaces the old write-lock-during-factory pattern.
func TestClawRegistry_ResolveConcurrentSameKey(t *testing.T) {
	r := NewRegistry()

	var calls int32
	const factoryDelay = 50 * time.Millisecond
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hi", "end_turn")}}
	r.Register("test", func(modelID string) (api.APIClient, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(factoryDelay)
		return mock, nil
	})

	const goroutines = 10
	var wg sync.WaitGroup
	results := make([]api.APIClient, goroutines)
	errs := make([]error, goroutines)

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = r.Resolve("test/model")
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("factory called %d times, want exactly 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
		if results[i] != mock {
			t.Errorf("goroutine %d: got different client instance than expected", i)
		}
	}
}

// TestClawRegistry_ResolveConcurrentDifferentKeys verifies that concurrent
// Resolve calls for distinct specs run their factories in parallel — i.e.,
// the registry never serializes them on a global write lock. Every factory
// must enter before any factory is allowed to return.
func TestClawRegistry_ResolveConcurrentDifferentKeys(t *testing.T) {
	r := NewRegistry()

	const goroutines = 10
	entered := make(chan string, goroutines)
	release := make(chan struct{})

	// Hold every factory open until all distinct providers have entered.
	for i := 0; i < goroutines; i++ {
		// Capture loop variable.
		idx := i
		mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hi", "end_turn")}}
		providerName := providerNameForIdx(idx)
		r.Register(providerName, func(modelID string) (api.APIClient, error) {
			entered <- providerName
			<-release
			return mock, nil
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	clients := make([]api.APIClient, goroutines)

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			clients[i], errs[i] = r.Resolve(providerNameForIdx(i) + "/m")
		}(i)
	}

	close(start)
	// This deadline only bounds a broken barrier. Concurrency is proved by
	// all factories entering while none can return, not by elapsed time.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	seen := make(map[string]bool, goroutines)
waitForFactories:
	for len(seen) < goroutines {
		select {
		case provider := <-entered:
			seen[provider] = true
		case <-timer.C:
			t.Errorf("only %d/%d distinct factories entered before release — resolves may be serialized", len(seen), goroutines)
			break waitForFactories
		}
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
		if clients[i] == nil {
			t.Errorf("goroutine %d: nil client", i)
		}
	}
}

// TestClawRegistry_ResolveFactoryError verifies that factory errors are
// surfaced and that the error is sticky for the cached entry (sync.Once).
func TestClawRegistry_ResolveFactoryError(t *testing.T) {
	r := NewRegistry()
	var calls int32
	wantErr := errors.New("boom")
	r.Register("err", func(modelID string) (api.APIClient, error) {
		atomic.AddInt32(&calls, 1)
		return nil, wantErr
	})

	for i := 0; i < 5; i++ {
		_, err := r.Resolve("err/x")
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("call %d: got err=%v, want wrap of %v", i, err, wantErr)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("factory called %d times, want exactly 1 (errors are sticky)", got)
	}
}

// TestClawRegistry_RegisterInvalidatesCache verifies that re-registering a
// provider invalidates any cached entries created by the previous factory,
// so subsequent Resolve calls go through the new factory.
func TestClawRegistry_RegisterInvalidatesCache(t *testing.T) {
	r := NewRegistry()
	mock1 := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("v1", "end_turn")}}
	mock2 := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("v2", "end_turn")}}

	r.Register("p", func(modelID string) (api.APIClient, error) { return mock1, nil })
	got, err := r.Resolve("p/m")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if got != mock1 {
		t.Fatalf("first resolve returned wrong client")
	}

	// Re-register with a new factory; the cached mock1 should be evicted.
	r.Register("p", func(modelID string) (api.APIClient, error) { return mock2, nil })
	got, err = r.Resolve("p/m")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got != mock2 {
		t.Errorf("second resolve returned old cached client; expected new factory's client")
	}
}

// TestClawRegistry_ResolveAfterUnknownProviderRegister verifies that Resolve
// after an unknown-provider failure no longer leaves a poisoned cache entry —
// once Register adds the provider, Resolve must succeed.
func TestClawRegistry_ResolveAfterUnknownProviderRegister(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Resolve("late/m"); err == nil {
		t.Fatal("expected error for unknown provider")
	}

	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hi", "end_turn")}}
	r.Register("late", func(modelID string) (api.APIClient, error) { return mock, nil })

	got, err := r.Resolve("late/m")
	if err != nil {
		t.Fatalf("resolve after register: %v", err)
	}
	if got != mock {
		t.Errorf("got wrong client after registering provider")
	}
}

// providerNameForIdx returns a deterministic provider name for the given
// index. Used by TestClawRegistry_ResolveConcurrentDifferentKeys.
func providerNameForIdx(i int) string {
	// Avoid fmt to keep this tiny: ASCII offsets work for i in [0,10).
	return "p" + string(rune('0'+i))
}
