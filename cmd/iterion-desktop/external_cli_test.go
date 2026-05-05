package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLooker implements looker for tests, returning canned results.
type fakeLooker struct {
	paths    map[string]string
	versions map[string]string
	pathErr  map[string]error
	runErr   map[string]error
}

func (f fakeLooker) LookPath(name string) (string, error) {
	if err := f.pathErr[name]; err != nil {
		return "", err
	}
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", errors.New("not found")
}

func (f fakeLooker) Run(_ context.Context, name string, _ ...string) (string, error) {
	if err := f.runErr[name]; err != nil {
		return "", err
	}
	if v, ok := f.versions[name]; ok {
		return v, nil
	}
	return "", errors.New("no canned version")
}

func TestDetectWith_Found(t *testing.T) {
	l := fakeLooker{
		paths:    map[string]string{"claude": "/usr/local/bin/claude", "git": "/usr/bin/git", "codex": "/usr/local/bin/codex"},
		versions: map[string]string{"/usr/local/bin/claude": "claude 1.2.3", "/usr/bin/git": "git version 2.42.0", "/usr/local/bin/codex": "codex 0.0.1"},
	}
	out := detectWith(l)
	if len(out) != len(knownCLIs) {
		t.Fatalf("len = %d, want %d", len(out), len(knownCLIs))
	}
	for _, st := range out {
		if !st.Found {
			t.Errorf("%s: expected Found", st.Name)
		}
		if st.Path == "" {
			t.Errorf("%s: missing Path", st.Name)
		}
		if st.Version == "" {
			t.Errorf("%s: missing Version", st.Name)
		}
	}
}

func TestDetectWith_Missing(t *testing.T) {
	l := fakeLooker{} // all paths missing
	out := detectWith(l)
	for _, st := range out {
		if st.Found {
			t.Errorf("%s: expected not found", st.Name)
		}
		if st.InstallURL == "" {
			t.Errorf("%s: missing InstallURL", st.Name)
		}
	}
}

func TestDetectWith_VersionTimeout(t *testing.T) {
	l := fakeLooker{
		paths:  map[string]string{"git": "/usr/bin/git"},
		runErr: map[string]error{"/usr/bin/git": context.DeadlineExceeded},
	}
	out := detectWith(l)
	for _, st := range out {
		if st.Name != "git" {
			continue
		}
		if !st.Found {
			t.Errorf("git: expected Found despite version error")
		}
		if st.Version != "" {
			t.Errorf("git: expected empty Version on error, got %q", st.Version)
		}
	}
}

func TestDetectExternalCLIs_Cache(t *testing.T) {
	// Drop the global cache; replace the looker; ensure the second call
	// doesn't re-run LookPath when force=false.
	cliCacheMu.Lock()
	cliCache = nil
	cliCachedAt = time.Time{}
	prev := cliCacheLooker
	calls := 0
	cliCacheLooker = countingLooker{looker: fakeLooker{}, count: &calls}
	cliCacheMu.Unlock()
	defer func() {
		cliCacheMu.Lock()
		cliCacheLooker = prev
		cliCache = nil
		cliCachedAt = time.Time{}
		cliCacheMu.Unlock()
	}()

	_ = DetectExternalCLIs(false)
	first := calls
	_ = DetectExternalCLIs(false)
	if calls != first {
		t.Errorf("expected cache hit on second call; calls grew %d → %d", first, calls)
	}
	_ = DetectExternalCLIs(true) // force
	if calls == first {
		t.Errorf("expected cache miss with force=true; calls did not grow")
	}
}

type countingLooker struct {
	looker
	count *int
}

func (c countingLooker) LookPath(name string) (string, error) {
	*c.count++
	return c.looker.LookPath(name)
}
