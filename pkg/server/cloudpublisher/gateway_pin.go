package cloudpublisher

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §6: a pin on a gateway key is exclusive. A webhook's key_overrides
// entry for openai_compatible states which key pays the run's gateway routes,
// and nothing else may pay them in its place: not the team's other gateway
// key, not a shared tier's, not an attached pool's entry, not the runner
// pod's environment. A vendor pin keeps its warn-only contract
// (warnRefusedPins): the run spends the walk's next key and the line says so.
// The rule lives in the walk, so every launch that carries the pin here obeys
// it — the webhook lane, the trigger spine (whose subscription pins the same
// write validator admits) and a resume replaying the run's pins; the
// scheduled and board lanes rebuilding the pin from the integration's webhook
// are PR-R's.
//
// The walk alone cannot keep the promise. secrets.Resolve emits no candidate
// for a pinned key it cannot see (deleted, re-scoped) and gives up an
// undecryptable one, then walks on to the next key; an audience withholds the
// pin before its exemption; and with the key gone from the probe's sight a run
// whose every route rides the gateway is env-funded and consults no tier at
// all. Each of those ends in a sealed bundle that never names the pin — so
// the refusal reads what the walk SEALED, at one site every launch passes.

// GatewayPinCause names why a pinned gateway key did not fill the run's
// gateway slot.
type GatewayPinCause string

const (
	// GatewayPinWithheld: an audience of the key (its bots or its
	// repositories) does not admit this launch; a pin does not lift one.
	GatewayPinWithheld GatewayPinCause = "withheld"
	// GatewayPinMissing: the team holds no key with the pinned id — deleted
	// since the pin was written, its sweep not yet run or failed.
	GatewayPinMissing GatewayPinCause = "missing"
	// GatewayPinInvisible: the key exists but is not one this launch may
	// spend — another provider's, or scoped where the launch's identity
	// cannot see it.
	GatewayPinInvisible GatewayPinCause = "invisible"
	// GatewayPinHeldOut: one of the run's frozen lists (the providers
	// allowlist, the routing pair order) holds the gateway slot out, so no
	// key may seal there.
	GatewayPinHeldOut GatewayPinCause = "held_out"
	// GatewayPinUndecryptable: the key's sealed secret does not open.
	GatewayPinUndecryptable GatewayPinCause = "undecryptable"
	// GatewayPinUnread: the key store could not answer for the key.
	GatewayPinUnread GatewayPinCause = "unread"
	// GatewayPinUnsealed: the key reads spendable now, yet the walk sealed it
	// nowhere — it changed while the launch walked.
	GatewayPinUnsealed GatewayPinCause = "unsealed"
)

// GatewayPinRefusedError is the typed refusal of a launch or a resume whose
// pinned gateway key cannot fund the run's gateway routes. It carries the
// facts — which key, which cause — so a reader answers with errors.As instead
// of parsing the sentence.
type GatewayPinRefusedError struct {
	// Tenant is the team the run resolved credentials for.
	Tenant string
	// KeyID is the pinned key's id, as key_overrides names it.
	KeyID string
	// Cause is why the key did not fill the gateway slot.
	Cause GatewayPinCause
	// Detail spells the cause out for the operator: the audience that
	// refused, the list that held the slot out, the open error.
	Detail string
}

func (e *GatewayPinRefusedError) Error() string {
	return fmt.Sprintf("cloudpublisher: tenant %s pins api key %s for the %s gateway, but %s — a pin on a gateway key is exclusive (ADR-131 §6): no other key, no shared tier and not the runner's environment serves the run's gateway routes in its place; repair the key, or repin or clear the %s entry of key_overrides on the webhook or trigger subscription that launched the run",
		e.Tenant, e.KeyID, secrets.ProviderOpenAICompatible, e.Detail, secrets.ProviderOpenAICompatible)
}

// gatewayPinRefusal is §6's one chokepoint. resolveAndSealCredentials asks it
// after the tier walk — every tier that could fill the gateway slot has had
// its turn, and an env-funded run's walk consulted none — and before the
// content boundary is derived from what sealed. A pin is honoured when the
// gateway slot holds the very key it names; otherwise the run is refused by
// name. mayRide is gatewayRoutes.mayRide: a run that rides no gateway route
// has no use for the key, and its pin is idle rather than broken.
func (p *Publisher) gatewayPinRefusal(ctx context.Context, tenantID, ownerID, audienceBotID string, keyOverrides map[string]string, mayRide bool, bundle secrets.RunBundle, sealed sealedKeyIDs, policy sharedTierPolicy, allowed map[string]bool) error {
	pin := keyOverrides[string(secrets.ProviderOpenAICompatible)]
	if pin == "" || !mayRide {
		return nil
	}
	if held, _ := sealedGatewayKey(bundle); held && sealed.of(secrets.ProviderOpenAICompatible) == pin {
		return nil
	}
	cause, detail := p.gatewayPinFault(ctx, tenantID, ownerID, audienceBotID, pin, policy, allowed)
	return &GatewayPinRefusedError{Tenant: tenantID, KeyID: pin, Cause: cause, Detail: detail}
}

// gatewayPinFault names why a pinned gateway key is not in the gateway slot.
// The refusal is already decided, on what the walk sealed; this reads the
// key's record for the sentence, in the order the walk meets each obstacle:
// the record, its visibility to the launch's identity, its audiences (the
// predicates secrets.Resolve applies — read here because an env-funded walk
// consulted no tier and recorded none), the run's frozen lists, the seal.
func (p *Publisher) gatewayPinFault(ctx context.Context, tenantID, ownerID, audienceBotID, keyID string, policy sharedTierPolicy, allowed map[string]bool) (GatewayPinCause, string) {
	if p.apiKeys == nil {
		return GatewayPinUnread, "no API key store is wired to read it"
	}
	k, err := p.apiKeys.Get(store.WithTenant(ctx, tenantID), keyID)
	switch {
	case errors.Is(err, secrets.ErrApiKeyNotFound):
		return GatewayPinMissing, "the team holds no key with that id — it was deleted since the pin was written"
	case err != nil:
		return GatewayPinUnread, fmt.Sprintf("the key could not be read (%v)", err)
	}
	if k.Provider != secrets.ProviderOpenAICompatible {
		return GatewayPinInvisible, fmt.Sprintf("it is a %s key, not a gateway key", k.Provider)
	}
	if !secrets.SpendableBy(k, tenantID, ownerID) {
		return GatewayPinInvisible, fmt.Sprintf("the launch's identity %q cannot spend it — it is scoped to a user or to another team", ownerID)
	}
	repo := secrets.LaunchRepoFromContext(ctx)
	if why := (secrets.AudienceRefusal{Bot: !k.ServesBotForLaunch(audienceBotID), Repo: !k.ServesRepoForLaunch(repo)}); why.Bot || why.Repo {
		said := newAudienceWithholdings(audienceBotID, repo)
		said.note(k, why)
		return GatewayPinWithheld, "the key is withheld from this launch: " + said.cause(k.ID)
	}
	if slot := slotOfProvider(secrets.ProviderOpenAICompatible); !policy.listsAdmit(slot) {
		if excluderOfSlot(allowed, slot) == excludedByProviderAllowlist {
			return GatewayPinHeldOut, fmt.Sprintf("this run's providers allowlist (%s) holds the %s slot out", model.ProviderAllowlistNames(allowed), slot)
		}
		return GatewayPinHeldOut, fmt.Sprintf("this run's frozen routing pair order names no pair for the %s slot", slot)
	}
	if _, err := secrets.OpenApiKey(p.sealer, k); err != nil {
		return GatewayPinUndecryptable, fmt.Sprintf("its sealed secret does not open (%v)", err)
	}
	return GatewayPinUnsealed, "the walk sealed it nowhere, though it reads spendable now — it changed while the launch walked"
}
