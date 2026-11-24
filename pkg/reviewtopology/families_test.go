package reviewtopology

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// The folded family set is derived from the credential facts and nothing
// else: zai folds into claude, the claude_code forfait backs claude, codex
// backs gpt. It no longer decides a review topology (ADR-169) — it feeds the
// plan-review pair and the llm_families fact — but the folding itself is
// unchanged, and a drift here silently changes both.

func TestInventoryFamiliesKeepTheFolding(t *testing.T) {
	tests := []struct {
		name string
		inv  FamilyInventory
		want FamilySet
	}{
		{
			"zai folds into claude",
			InventoryFromReport(detect.Report{Providers: []detect.ProviderStatus{{Name: "zai", Available: true}}}),
			FamilySet{FamilyClaude: true},
		},
		{
			"an unavailable provider backs nothing",
			InventoryFromReport(detect.Report{Providers: []detect.ProviderStatus{{Name: "anthropic", Available: false}}}),
			FamilySet{},
		},
		{
			"the claude_code forfait backs claude",
			InventoryFromReport(detect.Report{Backends: []detect.BackendStatus{{Name: detect.BackendClaudeCode, Available: true}}}),
			FamilySet{FamilyClaude: true},
		},
		{
			"the codex forfait backs gpt",
			InventoryFromCredentialNames(nil, []string{detect.BackendCodex}),
			FamilySet{FamilyGPT: true},
		},
		{
			"cloud providers do not participate",
			InventoryFromReport(detect.Report{Providers: []detect.ProviderStatus{{Name: "bedrock", Available: true}, {Name: "vertex", Available: true}}}),
			FamilySet{},
		},
		{
			"both families",
			InventoryFromCredentialNames([]string{"anthropic", "openai"}, nil),
			FamilySet{FamilyClaude: true, FamilyGPT: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.inv.Families()
			if len(got) != len(tt.want) {
				t.Fatalf("families = %v, want %v", got, tt.want)
			}
			for f := range tt.want {
				if !got[f] {
					t.Fatalf("families = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
