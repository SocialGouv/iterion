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
// The keys are `<node>|<provider>`; the values are the audit fingerprints
// of the sealed bundle — the same strings the runner reads back from its
// own credentials (RunBundle's fingerprint maps, mirrored into
// Credentials.Fingerprints at claim) and the lease carries, so producer
// and consumer meet in one space. The run-level fallback chain names its
// providers on no node of its own (`run-fallback#<stage>` sites): its
// providers land on EVERY node that has a route of its own, which is how
// ir.ApplyRunFallback applies the chain.
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
	}
	if len(nodeProvs) == 0 {
		return nil
	}
	plan := make(map[string]string, len(nodeProvs))
	for node, provs := range nodeProvs {
		for p := range chain {
			if !containsProvider(provs, p) {
				provs = append(provs, p)
			}
		}
		for _, p := range provs {
			if fp := sealedFingerprint(bundle, p); fp != "" {
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

// containsProvider reports whether provs names p.
func containsProvider(provs []string, p string) bool {
	for _, q := range provs {
		if q == p {
			return true
		}
	}
	return false
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
func sealedFingerprint(bundle secrets.RunBundle, p string) string {
	prov := secrets.Provider(strings.ToLower(p))
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
