package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func TestResolveCapabilities_CuratedSourceWhenAggregatorEmpty(t *testing.T) {
	// Enabled registry but with no specs → curated must be the source.
	seedSpecs(t, nil)

	rc := ResolveCapabilities("anthropic", "glm-5.2")
	if rc.Source != SourceCurated {
		t.Errorf("Source = %q, want %q", rc.Source, SourceCurated)
	}
	if rc.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000 (curated glm-5.2)", rc.ContextWindow)
	}
	if !rc.Reasoning || !rc.ToolCall || !rc.Temperature {
		t.Errorf("flags = %+v, want all true", rc)
	}
	if rc.Spec != "anthropic/glm-5.2" {
		t.Errorf("Spec = %q, want anthropic/glm-5.2", rc.Spec)
	}
}

func TestResolveCapabilities_AggregatorSourceWhenFetched(t *testing.T) {
	seedSpecs(t, map[string]modelspecs.Spec{
		"anthropic/claude-sonnet-4-6": {ContextWindow: 1_000_000, MaxOutputTokens: 64000, InputCostPerM: 3, OutputCostPerM: 15},
	})

	rc := ResolveCapabilities("anthropic", "claude-sonnet-4-6")
	if rc.Source != SourceAggregator {
		t.Errorf("Source = %q, want %q", rc.Source, SourceAggregator)
	}
	if rc.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want 1000000 (aggregator)", rc.ContextWindow)
	}

	// A model NOT in the fixture falls back to curated.
	miss := ResolveCapabilities("openai", "o3")
	if miss.Source != SourceCurated {
		t.Errorf("o3 Source = %q, want %q (not in fixture)", miss.Source, SourceCurated)
	}
}

func TestResolveCapabilities_DisabledRegistryIsCurated(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.New(modelspecs.Options{Disabled: true})))

	rc := ResolveCapabilities("anthropic", "claude-sonnet-4-6")
	if rc.Source != SourceCurated {
		t.Errorf("Source = %q, want %q (registry disabled)", rc.Source, SourceCurated)
	}
}

func TestResolveSpec_MalformedSpec(t *testing.T) {
	for _, bad := range []string{"", "noprovider", "/missing-provider", "missing-model/"} {
		if _, err := ResolveSpec(bad); err == nil {
			t.Errorf("ResolveSpec(%q) error = nil, want error", bad)
		}
	}
	rc, err := ResolveSpec("anthropic/glm-5.1")
	if err != nil {
		t.Fatalf("ResolveSpec(valid) error: %v", err)
	}
	if rc.Provider != "anthropic" || rc.Model != "glm-5.1" {
		t.Errorf("parsed = %q/%q, want anthropic/glm-5.1", rc.Provider, rc.Model)
	}
}

func TestRefreshModelSpecs_UpdatesGlobalRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"anthropic":{"models":{"claude-sonnet-4-6":{"limit":{"context":1000000,"output":64000},"cost":{"input":3,"output":15}}}}}`))
	}))
	defer srv.Close()

	t.Cleanup(modelspecs.SetDefault(modelspecs.New(modelspecs.Options{
		URL:         srv.URL,
		CachePath:   filepath.Join(t.TempDir(), "model-specs-cache.json"),
		NoAutoFetch: true,
	})))

	// Before refresh: nothing fetched → curated source.
	if got := ResolveCapabilities("anthropic", "claude-sonnet-4-6").Source; got != SourceCurated {
		t.Fatalf("pre-refresh Source = %q, want curated", got)
	}
	if err := RefreshModelSpecs(context.Background()); err != nil {
		t.Fatalf("RefreshModelSpecs: %v", err)
	}
	// After refresh: the fixture model resolves from the aggregator.
	if got := ResolveCapabilities("anthropic", "claude-sonnet-4-6").Source; got != SourceAggregator {
		t.Errorf("post-refresh Source = %q, want aggregator", got)
	}
}

func TestKnownModelSpecs_AllParseable(t *testing.T) {
	got := KnownModelSpecs()
	if len(got) == 0 {
		t.Fatal("KnownModelSpecs() is empty")
	}
	for _, spec := range got {
		if _, _, err := ParseModelSpec(spec); err != nil {
			t.Errorf("KnownModelSpecs entry %q does not parse: %v", spec, err)
		}
	}
}

// Max output reaches the operator-facing view only through the aggregator —
// there is no curated table of completion caps — so the two states worth
// pinning are "the aggregator knows it" and "nothing does".
func TestResolveCapabilities_MaxOutputTokens(t *testing.T) {
	seedSpecs(t, map[string]modelspecs.Spec{
		"anthropic/claude-sonnet-4-6": {ContextWindow: 1_000_000, MaxOutputTokens: 64000},
	})

	rc := ResolveCapabilities("anthropic", "claude-sonnet-4-6")
	if rc.MaxOutputTokens != 64000 {
		t.Errorf("MaxOutputTokens = %d, want 64000 (aggregator)", rc.MaxOutputTokens)
	}

	// A model the aggregator does not carry reports zero — unknown, never
	// "uncapped".
	miss := ResolveCapabilities("openai", "o3")
	if miss.MaxOutputTokens != 0 {
		t.Errorf("curated-only MaxOutputTokens = %d, want 0", miss.MaxOutputTokens)
	}
}
