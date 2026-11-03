package reviewtopology

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The route-level publication (#1221). A family NAME folds every credential
// that backs it into one bit (zai folds into claude): a GLM-only host reads
// "claude backed" and the resolver green-lights a slot whose routes may all
// be dead — the run pays a doomed primary, or dies at its first review
// node. The inventory publishes each family as the SET of routes its slot
// can take, each with its launch-time state, so the launch judges the
// routes and a refusal can say WHICH one fails and why.
//
// The state is spelled in llmroute's failure-class vocabulary (ADR-121) —
// the class the first call on the route would fail with. Only two classes
// are decidable at launch: auth (no credential the route spends) and
// unavailable (the backend the route pins is not usable on this host).
// usage_window and transient_exhausted are runtime classes no launch probe
// can see; the bots' fallbacks: chains carry them.

// SlotRoute is one candidate route of a review family's slot, as the
// shipped bots declare it: the backend the node pins and the credential
// the route spends. State "" means the route can serve at launch.
type SlotRoute struct {
	Backend string
	Spends  string
	State   string // "" or an llmroute trigger class
	Detail  string // the human reason for a non-empty state
}

// Servable reports whether the route can serve at launch.
func (r SlotRoute) Servable() bool { return r.State == "" }

// FamilyInventory is the route-level truth about the review families on
// one launch surface: the facts a build read (a host detection report, or
// a run's sealed credential names), and the routes it implies per family.
type FamilyInventory struct {
	facts routeFacts
}

// routeFacts is what a launch surface knows about credentials and
// backends, flattened to the questions the slot routes ask.
type routeFacts struct {
	claudeCodeCLI  bool // the claude_code CLI is on the host (always true for the cloud runner image)
	claudeForfait  bool // claude_code OAuth (the claude forfait)
	anthropicKey   bool
	zaiKey         bool
	openaiCred     bool // OPENAI_API_KEY or the ChatGPT forfait
	chatGPTForfait bool
}

// claudeSlotRoutes is the claude family's slot as the shipped bots declare
// it: the primary pins claude_code and spends an Anthropic credential; the
// rescue stays on claude_code and spends the z.ai facade pair (review-pr's
// declared GLM fallback — where a bot declares none, the CLI's own BYOK
// precedence spends the same pair, so the servability answer holds).
func (f routeFacts) claudeSlotRoutes() []SlotRoute {
	routes := []SlotRoute{
		{Backend: detect.BackendClaudeCode, Spends: "an Anthropic credential (ANTHROPIC_API_KEY or the claude_code forfait)"},
		{Backend: detect.BackendClaudeCode, Spends: "the z.ai facade pair (ZAI_API_KEY, or a z.ai ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN) — the slot's GLM route"},
	}
	for i := range routes {
		switch {
		case !f.claudeCodeCLI:
			routes[i].State = llmroute.TriggerUnavailable
			routes[i].Detail = "the claude_code CLI is not on this host"
		}
	}
	if routes[0].State == "" && !f.anthropicKey && !f.claudeForfait {
		routes[0].State = llmroute.TriggerAuth
		routes[0].Detail = "no Anthropic credential and no claude_code forfait"
	}
	if routes[1].State == "" && !f.zaiKey {
		routes[1].State = llmroute.TriggerAuth
		routes[1].Detail = "no z.ai credential"
	}
	return routes
}

// gptSlotRoutes is the gpt family's slot as shipped: claw serving an
// OpenAI route, which an OPENAI_API_KEY or the ChatGPT forfait funds (the
// registry's openAIFromCtxForfait spends the latter in process).
func (f routeFacts) gptSlotRoutes() []SlotRoute {
	r := SlotRoute{Backend: detect.BackendClaw, Spends: "an OpenAI credential (OPENAI_API_KEY or the codex forfait)"}
	if !f.openaiCred && !f.chatGPTForfait {
		r.State = llmroute.TriggerAuth
		r.Detail = "no OpenAI credential and no codex forfait"
	}
	return []SlotRoute{r}
}

// Routes returns the slot's candidate routes for a family, with their
// states — nil for a family the topology does not know.
func (inv FamilyInventory) Routes(family string) []SlotRoute {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case FamilyClaude:
		return inv.facts.claudeSlotRoutes()
	case FamilyGPT:
		return inv.facts.gptSlotRoutes()
	default:
		return nil
	}
}

// Servable reports whether the family's slot has at least one route that
// can serve at launch.
func (inv FamilyInventory) Servable(family string) bool {
	for _, r := range inv.Routes(family) {
		if r.Servable() {
			return true
		}
	}
	return false
}

// Families derives the folded family set — the credential-level fact the
// mode resolution, the plan-review pair and llm_families read — from the
// same facts the routes are judged on, so the two publications can never
// disagree. The fold (operator decision): {anthropic, zai} are the SAME
// family "claude" — there is no true cross-family verification between
// them — and {openai} is "gpt"; a backend credential backs the family its
// nodes run (the claude_code forfait backs claude, codex backs gpt), which
// is what lets a forfait-only host resolve "claude". Cloud providers
// (foundry/bedrock/vertex) do not participate.
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
			f.claudeCodeCLI = b.Available || b.BinaryFound
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
// InventoryFromReport: it knows what sealed into the run's bundle, not
// what a host probe would see. The runner image ships the CLIs, so the
// claude_code CLI is present; the names answer the auth question only.
func InventoryFromCredentialNames(providers, backends []string) FamilyInventory {
	f := routeFacts{claudeCodeCLI: true}
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

// routeBreakdown renders a family's slot routes for a refusal: one line
// per route, its state class and the reason.
func routeBreakdown(routes []SlotRoute) string {
	if len(routes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range routes {
		state := r.State
		if state == "" {
			state = "servable"
		}
		fmt.Fprintf(&b, "\n    %s spending %s: %s", r.Backend, r.Spends, state)
		if r.Detail != "" {
			fmt.Fprintf(&b, " — %s", r.Detail)
		}
	}
	return b.String()
}

// judgeFamily is the one judgement every moment and provenance shares
// (#1221): the family a mono run will route to must have a SERVABLE route,
// not only a credential that backs its name. head says how the family came
// to be named (an operator's pin, the resolver's own choice); the refusal
// lists every candidate route with its state and the moment's fix.
//
// Read-only: it writes nowhere, so the resume moment can judge a run's
// FROZEN inputs — which it must not rewrite — with the same predicate the
// launch applies.
func judgeFamily(family, head string, inv FamilyInventory, when PinMoment) error {
	name := strings.ToLower(strings.TrimSpace(family))
	if name == "" {
		return nil
	}
	fams := inv.Families()
	if !fams[name] {
		return fmt.Errorf("%w: %s but no credential serves that family %s (available: %s) — the review slot it routes to dies at its first node, on every route:%s — %s",
			ErrUnservableFamily, head, when.clause, familyList(fams), routeBreakdown(inv.Routes(name)), when.fix)
	}
	if inv.Servable(name) {
		return nil
	}
	return fmt.Errorf("%w: %s, and a credential DOES hold the family name — but no route of the slot can serve it %s:%s — %s",
		ErrUnservableFamily, head, when.clause, routeBreakdown(inv.Routes(name)), when.fix)
}

// pinHead / resolvedHead phrase the provenance of the judged family.
// frozenHead is the resume's: the frozen value is pinned at launch BY
// SOMEONE — an operator's --var or the resolver's own choice — and the
// resume cannot tell which, so it names both rather than claim a pin.
func pinHead(family string) string {
	return fmt.Sprintf("mono_family %q is pinned", family)
}

func frozenHead(family string) string {
	return fmt.Sprintf("mono_family %q is frozen in this run's inputs (an operator's pin or the resolver's own choice at launch)", family)
}

func resolvedHead(family string) string {
	return fmt.Sprintf("mono_family %q was resolved from the credentials present", family)
}
