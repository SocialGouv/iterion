package cloudpublisher

import (
	"slices"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The content boundary producer (ADR-131 §4, #2005 PR-C'): which credential
// class a run's LLM content is held to, frozen on the run at launch
// (store.Run.ContentBoundary) and replayed by every resume, fork and subbot
// lane exactly as frozen. Its consumers landed first (PR-B'2): the
// server-side helpers and the supervisors refuse a team_gateway run's
// auxiliary specs (pkg/runview), the runner carries it from the message.
//
// One walk decision, three carriages: the stamp on the run document, the
// bundle's AmbientLLMRefused (every env fallback of the run answers a typed
// refusal), and the policy ladder's screen (a node routed to the team's
// gateway takes no crossing to a vendor credential — the publisher's
// advisory screen, its fallback door's probe and the runner's authoritative
// one all read the run's boundary).

// boundarySource is what a credential resolution knows of the run's content
// boundary before it walks. A launch derives it from its own walk
// (boundaryDerived); a resume replays the one the run document froze
// (boundaryFrozen), whatever its walk finds now — re-deriving would move a
// running run onto whichever answer the stores give today, and its bundle's
// ambient refusal with it. A parameter of resolveAndSealCredentials rather
// than something read off the context, so no caller can omit it.
type boundarySource struct {
	replay bool
	frozen *store.RunContentBoundary
}

// boundaryDerived is the launch's source: the walk decides.
var boundaryDerived = boundarySource{}

// boundaryFrozen is the resume's source: the run document's stamp, nil
// included — a run launched before the producer existed stays unstamped.
func boundaryFrozen(b *store.RunContentBoundary) boundarySource {
	return boundarySource{replay: true, frozen: b}
}

// resolve is the run's boundary under this source: the frozen stamp on a
// replay, derive's answer otherwise.
func (s boundarySource) resolve(derive func() *store.RunContentBoundary) *store.RunContentBoundary {
	if s.replay {
		return s.frozen
	}
	return derive()
}

// teamGatewayBoundary is the predicate the operator arbitrated (#2005 PR-C',
// arbitrations 1 and 3): the run is held to its team's gateway when the walk
// ATTRIBUTES a gateway route to a key (attributedRoutes on the view the walk
// derived, model.RouteView.GatewayKeyFunded) AND the gateway key is held —
// sealed on the launch, predicted on the preview. A mixed run (a gateway
// route attributed beside vendor routes, their keys sealed too) is held all
// the same: its vendor-funded auxiliaries are refused with the rest. A key
// sealed for a route the walk cannot decide (an env-templated dial, ADR-129
// §4) attributes nothing, so such a run stays own_keys. One helper for the
// launch and the preview, so neither reads a boundary the other does not.
func teamGatewayBoundary(view model.RouteView, holdsGatewayKey bool) bool {
	if !holdsGatewayKey || view.Workflow == nil {
		return false
	}
	return slices.Contains(attributedRoutes(view).providers, string(secrets.ProviderOpenAICompatible))
}

// sealedGatewayKey reports whether the bundle holds a gateway key, in either
// channel, and the canonical base URL it was sealed for.
func sealedGatewayKey(bundle secrets.RunBundle) (held bool, baseURL string) {
	gw := secrets.ProviderOpenAICompatible
	if bundle.APIKeys[gw] != "" {
		return true, bundle.APIBaseURLs[gw]
	}
	if bundle.PinnedAPIKeys[gw] != "" {
		return true, bundle.PinnedAPIBaseURLs[gw]
	}
	return false, ""
}

// derivedContentBoundary is the stamp a launch writes: team_gateway or
// own_keys, explicitly — a run the walk resolved is never left unstamped —
// with, whenever the launch sealed a gateway key (gatewaySealed), the id of
// the key row that filled the gateway slot and the host its sealed URL dials
// (the host the runner pins on the run's proxy: compatgw's reading, the same
// call pkg/runtime makes). An own_keys stamp names them for a key sealed for a
// route the walk could not decide (a runner pool's env dial): a replay reads
// them to know its launch sealed one (launchSealedGateway), and seals none a
// stamp naming none never sealed.
func derivedContentBoundary(teamGateway, gatewaySealed bool, keyID, baseURL string) *store.RunContentBoundary {
	b := &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}
	if teamGateway {
		b.Kind = store.ContentBoundaryTeamGateway
	}
	if teamGateway || gatewaySealed {
		b.KeyID = keyID
		b.GatewayHost = (compatgw.Config{BaseURL: baseURL}).Host()
	}
	return b
}
