package reviewtopology

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The route-level publication (#1221): a family is its slot's candidate
// routes with their launch states, and the folded family NAME can no longer
// green-light a slot whose routes are all dead. The matrix is the ticket's:
// a GLM-only host (a z.ai credential, nothing Anthropic).

// glmOnlyReport models the GLM-only host: a z.ai provider key, and the
// claude_code CLI present or not. The backend entry is always listed, as
// detect reports it (Available=false, BinaryFound=false when the CLI is
// absent) — an ABSENT entry would read as "not probed", a different host.
func glmOnlyReport(cli bool) detect.Report {
	return detect.Report{
		Providers: []detect.ProviderStatus{{Name: "zai", Available: true}},
		Backends:  []detect.BackendStatus{{Name: detect.BackendClaudeCode, BinaryFound: cli}},
	}
}

// The states are spelled in llmroute's failure-class vocabulary — the class
// the first call on the route would fail with — never a parochial re-spelling.
func TestSlotRouteStatesAreTheLLMRouteVocabulary(t *testing.T) {
	inv := InventoryFromReport(glmOnlyReport(false))
	var states []string
	for _, r := range inv.Routes(FamilyClaude) {
		states = append(states, r.State)
	}
	for _, st := range states {
		switch st {
		case llmroute.TriggerAuth, llmroute.TriggerUnavailable:
		default:
			t.Errorf("route state %q is outside the llmroute failure-class vocabulary", st)
		}
	}
}

// GLM-only WITH the claude_code CLI: the slot's Anthropic route is auth-dead,
// the z.ai facade route (the declared GLM fallback / the CLI's own BYOK
// precedence) serves — the family resolves claude and nothing refuses.
func TestGLMOnlyHostWithTheCLIResolvesClaudeThroughTheFacadeRoute(t *testing.T) {
	inv := InventoryFromReport(glmOnlyReport(true))
	routes := inv.Routes(FamilyClaude)
	if len(routes) != 2 {
		t.Fatalf("the claude slot publishes %d routes, want 2", len(routes))
	}
	if routes[0].State != llmroute.TriggerAuth {
		t.Errorf("the Anthropic-primary route: state %q, want %q (no Anthropic credential)", routes[0].State, llmroute.TriggerAuth)
	}
	if !routes[1].Servable() {
		t.Errorf("the z.ai facade route: state %q (%s), want servable — the z.ai key is right there", routes[1].State, routes[1].Detail)
	}
	if !inv.Servable(FamilyClaude) {
		t.Fatal("the claude family must be servable through the facade route")
	}
	if mode, family := ResolveFamilies(inv, "auto"); mode != ModeMono || family != FamilyClaude {
		t.Errorf("GLM-only with the CLI resolves %s/%q, want mono/claude", mode, family)
	}
}

// GLM-only WITHOUT the CLI: the family name is backed (zai folds into
// claude) and EVERY route of the slot is dead — the resolver's own choice
// is refused at launch, and the refusal says why per route, instead of the
// run dying at the reviewer node. This is the ticket's case.
func TestGLMOnlyHostWithoutTheCLIRefusesAtLaunchNamingTheRoutes(t *testing.T) {
	wf := wfWithVars(VarReviewMode, VarMonoFamily)
	inv := InventoryFromReport(glmOnlyReport(false))
	inputs := map[string]any{VarReviewMode: ModeAuto}
	_, _, _, err := InjectIfDeclaredFamilies(wf, inputs, inv, "")
	if err == nil {
		t.Fatal("a backed-but-unservable family must refuse the launch — the slot dies at its first node otherwise")
	}
	for _, want := range []string{
		`"claude"`, "resolved from the credentials present",
		"a credential DOES hold the family name",
		llmroute.TriggerUnavailable, "the claude_code CLI is not on this host",
		"remove the pin", // the launch moment's fix
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	// And a PIN on the same family is the same judgement with the pin's head.
	err = RefusePinnedFamily(FamilyClaude, inv, AtLaunch)
	if err == nil || !strings.Contains(err.Error(), "is pinned") {
		t.Errorf("the pinned arm must refuse with the pin's own head: %v", err)
	}
}

// GLM-only without the CLI but WITH an OpenAI credential: the route-aware
// preference falls to the family the slot CAN run, instead of dying on the
// folded name.
func TestRouteAwarePreferenceSkipsAnUnservableFamily(t *testing.T) {
	rep := glmOnlyReport(false)
	rep.Providers = append(rep.Providers, detect.ProviderStatus{Name: "openai", Available: true})
	mode, family := Resolve(rep, "auto")
	if mode != ModeMono || family != FamilyGPT {
		t.Errorf("zai+openai without the claude CLI resolves %s/%q, want mono/gpt", mode, family)
	}
}

// The cloud shape: the inventory built from sealed credential NAMES assumes
// the runner image's CLI, so a zai-only tenant's claude family is servable
// through the facade route — no refusal on the cloud lane.
func TestCloudGLMOnlyTenantIsServableThroughTheFacade(t *testing.T) {
	inv := InventoryFromCredentialNames([]string{"zai"}, nil)
	if !inv.Servable(FamilyClaude) {
		t.Fatalf("the cloud runner ships the claude_code CLI: the z.ai facade route must serve, got %+v", inv.Routes(FamilyClaude))
	}
}

// A codex-only host: the gpt family is backed by the forfait, and the slot's
// claw route spends it in process (openAIFromCtxForfait) — servable.
func TestCodexForfaitServesTheGPTSlot(t *testing.T) {
	inv := InventoryFromCredentialNames(nil, []string{detect.BackendCodex})
	if !inv.Servable(FamilyGPT) {
		t.Errorf("the codex forfait must serve the gpt slot's claw route, got %+v", inv.Routes(FamilyGPT))
	}
}

// The unbacked arm keeps its shape: the refusal names the family, the
// available ones, and now the routes that would have to serve.
func TestUnbackedPinRefusalCarriesTheRouteBreakdown(t *testing.T) {
	err := RefusePinnedFamily(FamilyGPT, InventoryFromCredentialNames([]string{"anthropic"}, nil), AtLaunch)
	if err == nil {
		t.Fatal("a pin on an unbacked family must refuse")
	}
	for _, want := range []string{`"gpt"`, "no credential serves that family", "claw", llmroute.TriggerAuth, "remove the pin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the unbacked refusal does not carry %q: %v", want, err)
		}
	}
}

// The folded set is DERIVED from the route facts: zai folds into claude, the
// claude_code forfait backs claude, codex backs gpt — the family-level
// contract of FamiliesFromReport, restated on the inventory so the two
// publications cannot drift.
func TestInventoryFamiliesKeepTheFolding(t *testing.T) {
	inv := InventoryFromReport(detect.Report{
		Providers: []detect.ProviderStatus{{Name: "zai", Available: true}, {Name: "anthropic", Available: false}},
		Backends:  []detect.BackendStatus{{Name: detect.BackendCodex, Available: true}},
	})
	fams := inv.Families()
	if !fams[FamilyClaude] || !fams[FamilyGPT] {
		t.Errorf("zai + codex forfait must back claude and gpt: %v", fams)
	}
}
