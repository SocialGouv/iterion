package model

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/llmtypes"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// yes and no read a capability flag the way a consumer must: a nil flag is
// neither — unknown.
func yes(p *bool) bool { return p != nil && *p }
func no(p *bool) bool  { return p != nil && !*p }

// flagWord is the word a flag reads as: yes, no, or unknown.
func flagWord(p *bool) string {
	switch {
	case p == nil:
		return "unknown"
	case *p:
		return "yes"
	}
	return "no"
}

// sameCapabilities compares two records by value — the flags by their word,
// so a pointer to the same verdict compares equal.
func sameCapabilities(a, b ModelCapabilities) bool {
	return flagWord(a.Reasoning) == flagWord(b.Reasoning) && flagWord(a.ToolCall) == flagWord(b.ToolCall) &&
		flagWord(a.Temperature) == flagWord(b.Temperature) && a.Source == b.Source &&
		a.ContextWindow == b.ContextWindow && a.MaxOutputTokens == b.MaxOutputTokens &&
		a.InputCostPerM == b.InputCostPerM && a.OutputCostPerM == b.OutputCostPerM
}

// capabilitiesWords renders a record for a failure message, the flags by word.
func capabilitiesWords(c ModelCapabilities) string {
	return fmt.Sprintf("{reasoning=%s tool_call=%s temperature=%s source=%s context=%d max_output=%d}",
		flagWord(c.Reasoning), flagWord(c.ToolCall), flagWord(c.Temperature), c.Source, c.ContextWindow, c.MaxOutputTokens)
}

// capabilitiesLine renders a record the way testdata/vendor_capabilities.golden
// holds it — one line per spec, every flag by its word.
func capabilitiesLine(spec string, rc ResolvedCapabilities) string {
	return fmt.Sprintf("%s reasoning=%s tool_call=%s temperature=%s context=%d max_output=%d source=%s\n",
		spec, flagWord(rc.Reasoning), flagWord(rc.ToolCall), flagWord(rc.Temperature), rc.ContextWindow, rc.MaxOutputTokens, rc.Source)
}

// vendorGoldenSpecs are the vendor ids whose curated verdict the golden
// holds — one per branch and heuristic of the curated table.
var vendorGoldenSpecs = []string{
	"anthropic/claude-opus-5-5", "anthropic/claude-opus-5", "anthropic/claude-opus-5[1m]", "anthropic/claude-sonnet-4-6",
	"anthropic/claude-haiku-4-5-20251001", "anthropic/claude-3-5-sonnet-20241022", "anthropic/claude-3.5-sonnet",
	"anthropic/claude-3-opus-20240229", "anthropic/claude-2.1", "anthropic/claude-instant",
	"anthropic/glm-5.3", "anthropic/glm-5.2", "anthropic/glm-5.1", "anthropic/glm-4.6", "anthropic/glm-9.9-unknown",
	"openai/gpt-6-sol", "openai/gpt-6-astra", "openai/gpt-6-luna", "openai/gpt-4o", "openai/gpt-4.1", "openai/gpt-5",
	"openai/o1-preview", "openai/o3-mini", "openai/o4-mini",
	"xai/grok-3", "xai/grok-3-mini", "xai/grok/grok-3-mini", "xai/grok-4",
	"moonshot/kimi-k2", "zai/glm-5.3", "mistral/mistral-large", "unknownprovider/any-model",
}

// TestVendorVerdictsDoNotMove: the flags gained an unknown state for the
// gateway's sake; no VENDOR verdict moves — the golden was written by the
// code before the change, with the registry empty so the curated table alone
// answers, and every vendor id still reads the same words.
func TestVendorVerdictsDoNotMove(t *testing.T) {
	seedSpecs(t, nil)
	var got strings.Builder
	for _, spec := range vendorGoldenSpecs {
		provider, id, err := ParseModelSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(capabilitiesLine(spec, ResolveCapabilities(provider, id)))
	}
	want, err := os.ReadFile("testdata/vendor_capabilities.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("a vendor verdict moved:\n--- golden\n%s--- now\n%s", want, got.String())
	}
	for _, spec := range vendorGoldenSpecs {
		if strings.Contains(got.String(), spec+" reasoning=unknown") || strings.Contains(got.String(), "tool_call=unknown") || strings.Contains(got.String(), "temperature=unknown") {
			t.Fatalf("a vendor id reads unknown: the curated table answers every flag\n%s", got.String())
		}
	}
}

// gatewayEnv isolates the gateway catalog: no forwarded record, the given
// operator table, the given catalog provider — and the memoised table reset
// around the test.
func gatewayEnv(t *testing.T, operatorTable, catalogProvider string) {
	t.Helper()
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv(compatgw.CatalogModelsEnv, operatorTable)
	t.Setenv(compatgw.CatalogProviderEnv, catalogProvider)
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// TestAGatewayIdResolvesWhatItsCatalogStatesAndNothingMore: a gateway id
// no table carries resolves to UNKNOWN flags (nil), not to false — the
// resolution the ticket closes (#2103); the operator's table may declare the
// flags, and a flag it leaves out stays unknown; the models.dev provider the
// operator named answers with its own flags and its provenance.
func TestAGatewayIdResolvesWhatItsCatalogStatesAndNothingMore(t *testing.T) {
	gatewayEnv(t, "", "")
	seedSpecs(t, nil)
	caps := capabilitiesForModel(modelroute.OpenAICompatible, "glm-5.2")
	if caps.Reasoning != nil || caps.ToolCall != nil || caps.Temperature != nil {
		t.Fatalf("an unknown gateway id answered flags %s/%s/%s, want unknown", flagWord(caps.Reasoning), flagWord(caps.ToolCall), flagWord(caps.Temperature))
	}
	if caps.Source != SourceUnknown || caps.ContextWindow != 0 {
		t.Fatalf("an unknown gateway id reads source %q, window %d; want unknown, 0", caps.Source, caps.ContextWindow)
	}
	rc := ResolveCapabilities(modelroute.OpenAICompatible, "glm-5.2")
	if rc.Source != SourceUnknown || rc.ToolCall != nil {
		t.Fatalf("the projection reads %q / tool_call=%s, want unknown / unknown", rc.Source, flagWord(rc.ToolCall))
	}

	gatewayEnv(t, `{"glm-5.2": {"context_window": 200000, "max_output_tokens": 16384, "input_usd_per_mtok": 1.8, "output_usd_per_mtok": 5.5, "reasoning": true, "tool_call": true}}`, "")
	caps = capabilitiesForModel(modelroute.OpenAICompatible, "glm-5.2")
	if !yes(caps.Reasoning) || !yes(caps.ToolCall) || caps.Temperature != nil {
		t.Fatalf("the operator's flags read %s/%s/%s, want yes/yes/unknown", flagWord(caps.Reasoning), flagWord(caps.ToolCall), flagWord(caps.Temperature))
	}
	if caps.Source != llmtypes.SourceOperator || caps.ContextWindow != 200000 {
		t.Fatalf("the operator's record reads source %q, window %d", caps.Source, caps.ContextWindow)
	}
	// The record's numbers travel too — a gateway spec answered zeros for
	// max output and both prices before the gateway catalog answered this
	// route, and a caller sizes a request and prices a run from them.
	if caps.MaxOutputTokens != 16384 {
		t.Errorf("the operator's max output = %d, want 16384", caps.MaxOutputTokens)
	}
	if caps.InputCostPerM != 1.8 || caps.OutputCostPerM != 5.5 {
		t.Errorf("the operator's prices = %v/%v, want 1.8/5.5", caps.InputCostPerM, caps.OutputCostPerM)
	}

	gatewayEnv(t, "", "scaleway")
	seedSpecs(t, map[string]modelspecs.Spec{
		"scaleway/glm-5.2": {ContextWindow: 128000, Reasoning: boolp(false), ToolCall: boolp(true)},
	})
	caps = capabilitiesForModel(modelroute.OpenAICompatible, "glm-5.2")
	if !no(caps.Reasoning) || !yes(caps.ToolCall) || caps.Temperature != nil {
		t.Fatalf("the catalog's flags read %s/%s/%s, want no/yes/unknown", flagWord(caps.Reasoning), flagWord(caps.ToolCall), flagWord(caps.Temperature))
	}
	if caps.Source != llmtypes.SourceCatalogLive || caps.ContextWindow != 128000 {
		t.Fatalf("the catalog's record reads source %q, window %d", caps.Source, caps.ContextWindow)
	}

	// The SNAPSHOT tier answers the numbers and NO flag: its schema holds
	// the four numerics only, so an id models.dev publishes as
	// `tool_call:false` reads unknown — a warning, not a block — whenever
	// the live table is not loaded. Honest for what the file holds, and
	// pinned here so widening the schema is a deliberate change (the flag
	// an operator needs offline goes in OPENAI_COMPATIBLE_MODELS).
	gatewayEnv(t, "", "scaleway")
	seedSpecs(t, nil)
	caps = capabilitiesForModel(modelroute.OpenAICompatible, "bge-multilingual-gemma2")
	if caps.Source != llmtypes.SourceCatalogSnapshot || caps.ContextWindow != 8191 {
		t.Fatalf("the snapshot's record reads source %q, window %d; want %q, 8191", caps.Source, caps.ContextWindow, llmtypes.SourceCatalogSnapshot)
	}
	if caps.Reasoning != nil || caps.ToolCall != nil || caps.Temperature != nil {
		t.Fatalf("the snapshot answered flags %s/%s/%s, want unknown for all three", flagWord(caps.Reasoning), flagWord(caps.ToolCall), flagWord(caps.Temperature))
	}
}

// TestCapabilitySourceSpellingsAgreeWithTheGateway: one spelling per source
// — the record's provenance is the gateway catalog's word, not a copy that
// drifts.
func TestCapabilitySourceSpellingsAgreeWithTheGateway(t *testing.T) {
	for _, pair := range [][2]string{
		{string(llmtypes.SourceOperator), compatgw.SourceOperator},
		{string(llmtypes.SourceCatalogLive), compatgw.SourceCatalogLive},
		{string(llmtypes.SourceCatalogSnapshot), compatgw.SourceCatalogSnapshot},
		{string(llmtypes.SourceUnknown), compatgw.SourceUnknown},
	} {
		if pair[0] != pair[1] {
			t.Errorf("source spelled %q here and %q in the gateway", pair[0], pair[1])
		}
	}
}

// capturedStderr redirects os.Stderr for the caller's scope and returns
// everything written to it. The gateway catalog's unknown-id line goes to
// the process stderr through a fallback logger, so the stream is the only
// place to read it.
func capturedStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	os.Stderr = orig
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	out, rerr := io.ReadAll(r)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if cerr := r.Close(); cerr != nil {
		t.Fatalf("close read side: %v", cerr)
	}
	return string(out)
}

// The capability READ path resolves an unknown gateway id QUIETLY: the
// studio's free-text model fields query GET /api/model-capabilities with
// whatever is typed so far, so a warning per distinct id is a warning per
// keystroke past the slash. The launch seams keep the warning — there an
// unknown id is about to cost a run. Red when the read path warns, or when
// the launch path stops warning.
func TestAnUnknownGatewayIdIsQuietOnTheReadPathAndLoudOnTheLaunchPath(t *testing.T) {
	// The verdict must not depend on the developer's ambient level: the
	// quiet branch's DEBUG line carries the same phrase this greps, and
	// `error` would filter the WARN the launch half counts.
	t.Setenv(iterlog.EnvLevel, "info")
	gatewayEnv(t, "", "scaleway")
	seedSpecs(t, nil)

	// Both roads a read takes for the same id: the capability resolution,
	// and the PRICE — modelcatalog.Build and the pricing audit resolve each
	// entry twice, and the second road ran through the loud pricer.
	typed := capturedStderr(t, func() {
		for _, id := range []string{"m", "my", "my-", "my-mo", "my-model"} {
			if _, err := ResolveSpec(modelroute.OpenAICompatible + "/" + id); err != nil {
				t.Fatalf("ResolveSpec(%q): %v", id, err)
			}
			cost.EffectiveRate(modelroute.OpenAICompatible + "/" + id)
			// The cost preview's road: a workflow priced in an editor.
			cost.EstimateUSDQuiet(modelroute.OpenAICompatible+"/"+id, 1000, 1000)
		}
	})
	if strings.Contains(typed, "is in no table") {
		t.Errorf("the read path logged while the operator typed:\n%s", typed)
	}

	// NO cache reset here: that is the point. A read must leave the per-id
	// warning budget alone, so the launch that follows in the same process
	// still says it — once.
	launched := capturedStderr(t, func() {
		for range 2 {
			compatgw.ResolveCatalog("my-model", modelspecs.Default(), os.Getenv)
		}
	})
	if n := strings.Count(launched, `id "my-model" is in no table`); n != 1 {
		t.Errorf("the launch path logged the unknown id %d times after the read path saw it, want exactly 1:\n%s", n, launched)
	}
}

// Loudness is a property of each launch SEAM, not of the function name: a
// seam switched to the quiet form would otherwise be invisible. One
// unknown id per seam (the warning is one line per id per process), one
// line each.
func TestEveryLaunchSeamSaysAnUnknownGatewayIdOutLoud(t *testing.T) {
	t.Setenv(iterlog.EnvLevel, "info")
	gatewayEnv(t, "", "scaleway")
	seedSpecs(t, nil)
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")

	for _, seam := range []struct {
		name string
		id   string
		call func(t *testing.T, id string)
	}{{
		name: "the mid-run compaction sizing",
		id:   "zz-seam-compaction",
		call: func(t *testing.T, id string) { gatewayCompactionConfig(id, 0, 0) },
	}, {
		name: "the sandbox hand-off",
		id:   "zz-seam-handoff",
		call: func(t *testing.T, id string) {
			if _, _, err := forwardableProviderEnv(context.Background(), modelroute.OpenAICompatible+"/"+id); err != nil {
				t.Fatalf("forwardableProviderEnv: %v", err)
			}
		},
	}, {
		// The usage reading of a finished call: the same estimator a
		// preview calls, in its LOUD form.
		name: "the usage estimate",
		id:   "zz-seam-usage",
		call: func(t *testing.T, id string) {
			cost.EstimateUSD(modelroute.OpenAICompatible+"/"+id, 1000, 1000)
		},
	}} {
		t.Run(seam.name, func(t *testing.T) {
			out := capturedStderr(t, func() { seam.call(t, seam.id) })
			if n := strings.Count(out, `id "`+seam.id+`" is in no table`); n != 1 {
				t.Errorf("%s logged the unknown id %d times, want exactly 1:\n%s", seam.name, n, out)
			}
		})
	}
}
