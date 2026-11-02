package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The credential plan (#2288): the launch-frozen answer to "which sealed
// credential does each node of this run spend" — the producer half of the
// fine door-lease booking. The broker's consumer (Outcome.ByFingerprint +
// effectiveCharge) slices an attempt's spend by the LEASE credential's
// fingerprint; the runner's own readings (the delegate's channel label, the
// (backend, model) precedence re-derivation) cannot always name that
// fingerprint — a label names a channel, and a re-derivation diverges from
// the executor exactly on the override-pinned nodes a door rung serves.
// Only the publisher holds both facts at once: the walk that resolved each
// node's providers, and the bundle it sealed. CredentialPlan joins them,
// once, at the launch.

// PlanKeySep separates a plan key's node half from its provider half. The
// provider half is required: one node can spend two credentials (its own
// route on the primary, the door donor's on an authored rung), and what
// distinguishes the observations at report time is the provider the
// serving chain element resolved — stamped on every delegate event
// (#2772). The plan is keyed per (node, provider), never per node alone.
const PlanKeySep = "|"

// CredentialPlan derives the node-and-provider → credential-fingerprint
// plan a publisher stamps on the RunMessage. Nil unless the walk is
// NarrowSafe: a route it could not read may spend anything, so no per-node
// statement this derivation could make is total, and a partial plan would
// turn the runner's fine table into a silent under-booking of the donor
// (spend on an unnamed node books nobody). With the walk narrow-safe every
// route names its providers, and the plan covers the run's whole program.
//
// Nil also when a provider the plan would name is funded TWICE — a pinned
// key beside its own forfait — on a walk that is not forfait-first for it:
// which credential a route spends there depends on the backend it runs
// (claude_code and codex read the forfait first, claw bills it as extra
// usage, pi has no bridge to it), and one (node, provider) key cannot
// carry two answers. res.ForfaitFirst — the same walk's own reading —
// licenses the single answer where every route declares the forfait's
// precedence; every other mix stays unstamped, and the broker keeps the
// coarse whole-attempt booking, the conservative-on-ignore direction.
//
// The keys are `<node>|<provider>`; the values are the audit fingerprints
// of the sealed bundle — the same strings the runner reads back from its
// own credentials (RunBundle's fingerprint maps, mirrored into
// Credentials.Fingerprints at claim) and the lease carries, so producer
// and consumer meet in one space. A Verified Action's rung-4 recovery keys
// a second row under the synthetic agent's runtime id
// (`<tool>__recover`), the id the recovery dispatch's own events carry
// (agentRecoveryPlanNode). The run-level fallback chain names its
// providers on no node of its own (`run-fallback#<stage>` sites): the
// derivation folds it onto EVERY node that has a site of its own — a
// superset of where ir.ApplyRunFallback actually lands the chain (agent
// nodes declaring no routes of their own; a judge, or an agent with
// authored `fallbacks:`, never takes it). The extra rows stay inert: an
// observation joins on the provider its serving element resolved, which a
// node the chain never reached names only from its own routes, and a
// twice-funded chain provider refuses the plan whole — the conservative
// direction. Spreading wider than the application is a booking-safe
// over-claim, never a misattribution.
func CredentialPlan(res ProviderResolution, bundle secrets.RunBundle) map[string]string {
	if !res.NarrowSafe || len(res.Sites) == 0 {
		return nil
	}
	chain := make(map[string]bool)
	nodeProvs := make(map[string][]string)
	for site, provs := range res.Sites {
		node, isChain := siteNode(site)
		if isChain {
			for _, p := range provs {
				chain[p] = true
			}
			continue
		}
		nodeProvs[node] = append(nodeProvs[node], provs...)
		// Rung 4 dispatches under the synthetic agent's runtime id, not the
		// tool node's: the same providers key a second row there, or every
		// fired recovery observation joins nothing and books nobody under
		// real spend.
		if rung := agentRecoveryPlanNode(site); rung != "" {
			nodeProvs[rung] = append(nodeProvs[rung], provs...)
		}
	}
	if len(nodeProvs) == 0 {
		return nil
	}
	forfaitFirst := make(map[string]bool, len(res.ForfaitFirst))
	for _, p := range res.ForfaitFirst {
		forfaitFirst[p] = true
	}
	plan := make(map[string]string, len(nodeProvs))
	for node, provs := range nodeProvs {
		for p := range chain {
			if !containsProvider(provs, p) {
				provs = append(provs, p)
			}
		}
		for _, p := range provs {
			if twiceFunded(bundle, p) && !forfaitFirst[p] {
				return nil
			}
			if fp := sealedFingerprint(bundle, p, forfaitFirst[p]); fp != "" {
				plan[node+PlanKeySep+p] = fp
			}
		}
	}
	if len(plan) == 0 {
		return nil
	}
	return plan
}

// siteNode names the node a walk site belongs to, and whether the site is
// the run-level fallback chain's (which belongs to every node at once).
// An unsuffixed site is its own node — a supervisor site keeps its whole
// id, and no event of the run ever carries it, which leaves its plan keys
// inert rather than misattributed.
func siteNode(site string) (node string, isRunFallback bool) {
	if site == ir.RunFallbackName || strings.HasPrefix(site, ir.RunFallbackName+"#") {
		return "", true
	}
	if idx := strings.Index(site, "#"); idx > 0 {
		return site[:idx], false
	}
	return site, false
}

// agentRecoverySiteSuffix is the walk's site for a Verified Action's rung-4
// agent recovery (override_fold's agent-recovery arm) — the launch
// vocabulary the pins and the refusals read.
const agentRecoverySiteSuffix = "#agent_recovery"

// agentRecoveryPlanNode names the plan-key node half a rung-4 walk site
// rows under — the synthetic agent's runtime id (agentRecoveryNodeID,
// recoveryAgentNode), the id every event of a recovery dispatch carries —
// and "" for any other site. The walk records the rung at
// `<tool>#agent_recovery`; keying its rows on the tool node alone would
// leave every fired recovery observation joining `<tool>__recover|…`, a key
// the plan never stamped.
//
// An authored fallback route whose author named it "agent_recovery" spells
// the same site: its extra `<tool>__recover` rows stay inert (nothing
// dispatches under that id on a run without the rung) and its own node rows
// are unchanged — the duplication only ever adds, the same booking-safe
// over-claim the run-level chain's spread is.
func agentRecoveryPlanNode(site string) string {
	if !strings.HasSuffix(site, agentRecoverySiteSuffix) {
		return ""
	}
	return agentRecoveryNodeID(strings.TrimSuffix(site, agentRecoverySiteSuffix))
}

// containsProvider reports whether provs names p.
func containsProvider(provs []string, p string) bool {
	for _, q := range provs {
		if q == p {
			return true
		}
	}
	return false
}

// twiceFunded reports whether the bundle funds provider p TWICE: a key a
// shared tier pinned for the route beside the provider's own OAuth forfait.
// Only that pair splits the spend by backend — a default key outranks the
// forfait on every backend (the delegates rank an API key above an OAuth
// dir on the same wire), and the fill's one-credential-per-family rule
// keeps every other combination single. The plan asks before it names a
// fingerprint, because in this shape the answer is the route backend's,
// not the bundle's.
func twiceFunded(bundle secrets.RunBundle, p string) bool {
	if bundle.PinnedAPIKeys[secrets.Provider(strings.ToLower(p))] == "" {
		return false
	}
	kind := OAuthKindOfProvider(p)
	return kind != "" && bundle.OAuthCredentials[kind] != nil
}

// sealedFingerprint names the audit fingerprint the RUNNER will hold for
// provider p's credential — the same reading Credentials.Fingerprints
// builds at claim (loop_credentials' stamped()): the bundle's own stamp
// when the server sent one (a gateway key folds its URL in and cannot be
// recomputed), else the hash of the secret. The default channel's key
// reads first — one credential per provider across the two channels, so
// the first that holds the key holds the identity — and the OAuth forfait
// answers for a provider no key funds, its SUBSCRIPTION fingerprint the
// only stable identity a refresh never rewrites.
//
// The one shape where a provider holds TWO credentials at once is a pinned
// key beside its own forfait — the pair the shared tiers seal on purpose
// when a route pins the forfait's own provider (claw bills a Claude forfait
// as extra usage, pi has no bridge to it). There the delegate precedence
// decides, not the channel order: a forfait-first backend spends the
// forfait (its declared precedence reads key, forfait, pinned key), so
// forfaitFirst names the subscription's fingerprint. CredentialPlan refuses
// the shape for every other backend mix, where no per-(node, provider)
// answer is total; the default key never meets the forfait (the fill's
// one-credential-per-family rule), so forfaitFirst is inert beside it —
// the key outranks the forfait on every backend.
func sealedFingerprint(bundle secrets.RunBundle, p string, forfaitFirst bool) string {
	prov := secrets.Provider(strings.ToLower(p))
	if forfaitFirst && twiceFunded(bundle, p) {
		if kind := OAuthKindOfProvider(p); kind != "" {
			return bundle.OAuthFingerprints[kind]
		}
	}
	var key, baseURL string
	switch {
	case bundle.APIKeys[prov] != "":
		key, baseURL = bundle.APIKeys[prov], bundle.APIBaseURLs[prov]
	case bundle.PinnedAPIKeys[prov] != "":
		key, baseURL = bundle.PinnedAPIKeys[prov], bundle.PinnedAPIBaseURLs[prov]
	default:
		if kind := OAuthKindOfProvider(p); kind != "" && bundle.OAuthCredentials[kind] != nil {
			return bundle.OAuthFingerprints[kind]
		}
		return ""
	}
	if fp := bundle.APIKeyFingerprints[prov]; fp != "" {
		return fp
	}
	return secrets.APIKeyFingerprint(prov, key, baseURL)
}

// OAuthKindOfProvider maps a provider to the OAuth slot kind that funds it,
// "" when none — the direction the plan derivation reads (a route naming
// anthropic spends the Claude forfait when no key holds).
func OAuthKindOfProvider(p string) string {
	switch secrets.Provider(strings.ToLower(p)) {
	case secrets.ProviderAnthropic:
		return string(secrets.OAuthKindClaudeCode)
	case secrets.ProviderOpenAI:
		return string(secrets.OAuthKindCodex)
	}
	return ""
}

// OAuthProvider maps an OAuth slot kind to the provider it authenticates
// against, "" for a kind the plan does not know — the direction the
// runner's label reading and the stamp's own providerOfOAuthKind read.
// One table here, so the plan's keys and the report's joins cannot
// disagree about which provider a forfait serves.
func OAuthProvider(kind string) string {
	switch secrets.OAuthKind(kind) {
	case secrets.OAuthKindClaudeCode:
		return string(secrets.ProviderAnthropic)
	case secrets.OAuthKindCodex:
		return string(secrets.ProviderOpenAI)
	}
	return ""
}
