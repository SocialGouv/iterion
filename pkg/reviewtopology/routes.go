package reviewtopology

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// The family inventory: the credential-level fact a bot cannot probe —
// which review families a host (or a sealed run bundle) can back — as the
// plan-review pair and llm_families read it.
//
// The fold (operator decision): {anthropic, zai} are the SAME family
// "claude" — there is no true cross-family verification between them — and
// {openai} is "gpt". A backend credential backs the family its nodes run
// (the claude_code forfait backs claude, codex backs gpt). Cloud providers
// (foundry/bedrock/vertex) do not participate.

// FamilyInventory is the fact set about the review families on one launch
// surface: what a build read (a host detection report, or a run's sealed
// credential names).
type FamilyInventory struct {
	facts routeFacts
}

// routeFacts is what a launch surface knows about credentials and
// backends, flattened to the family questions.
type routeFacts struct {
	claudeForfait  bool // claude_code OAuth (the claude forfait)
	anthropicKey   bool
	zaiKey         bool
	openaiCred     bool // OPENAI_API_KEY or the ChatGPT forfait
	chatGPTForfait bool
}

// Families derives the folded family set the plan-review pair and
// llm_families read.
func (inv FamilyInventory) Families() FamilySet {
	fams := make(FamilySet, 2)
	if f := inv.facts; f.claudeForfait || f.anthropicKey || f.zaiKey {
		fams[FamilyClaude] = true
	}
	if f := inv.facts; f.openaiCred || f.chatGPTForfait {
		fams[FamilyGPT] = true
	}
	return fams
}

// InventoryFromReport builds the inventory from a host detection report.
func InventoryFromReport(rep detect.Report) FamilyInventory {
	var f routeFacts
	for _, b := range rep.Backends {
		switch b.Name {
		case detect.BackendClaudeCode:
			f.claudeForfait = b.Available
		case detect.BackendCodex:
			f.chatGPTForfait = b.Available
		}
	}
	for _, p := range rep.Providers {
		if !p.Available {
			continue
		}
		switch p.Name {
		case "anthropic":
			f.anthropicKey = true
		case "zai":
			f.zaiKey = true
		case "openai":
			f.openaiCred = true
		}
	}
	return FamilyInventory{facts: f}
}

// InventoryFromCredentialNames is the cloud publisher's counterpart of
// InventoryFromReport: it knows what sealed into the run's bundle, not what
// a host probe would see.
func InventoryFromCredentialNames(providers, backends []string) FamilyInventory {
	var f routeFacts
	for _, p := range providers {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "anthropic":
			f.anthropicKey = true
		case "zai":
			f.zaiKey = true
		case "openai":
			f.openaiCred = true
		}
	}
	for _, b := range backends {
		switch strings.ToLower(strings.TrimSpace(b)) {
		case detect.BackendClaudeCode:
			f.claudeForfait = true
		case detect.BackendCodex:
			f.chatGPTForfait = true
		}
	}
	return FamilyInventory{facts: f}
}
