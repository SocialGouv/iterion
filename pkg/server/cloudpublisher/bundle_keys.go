package cloudpublisher

import "github.com/SocialGouv/iterion/pkg/secrets"

// setAPIKey is the ONE writer of the bundle's API-key maps. Every lane that
// seals a key — the tenant's BYOK tier, the org and platform tiers, the
// credential pool, the fallback door, the refused-key restore — passes
// through it, so a gateway key is written together with what travels with
// it: the root it authenticates against (the record's canonical base_url,
// paired map for map so a key is never read beside another channel's URL)
// and the server's fingerprint, which the runner stamps rather than
// recomputing (a gateway key's identity folds its URL in). A source-scanning
// test refuses the write forms it knows — index assignments, deletes, map
// aliases, address-taking, map builtins — anywhere else in the package.
//
// pinned chooses the channel: the run's default maps, or the pinned ones a
// shared tier fills for a route that NAMES the provider (RunBundle.
// PinnedAPIKeys). The helper never creates an entry for a provider it is
// not funding — holdsLLMCredential reads the maps' LENGTH — so an empty
// key writes nothing.
func setAPIKey(bundle *secrets.RunBundle, prov secrets.Provider, key, baseURL, fingerprint string, pinned bool) {
	if key == "" {
		return
	}
	keys, urls := &bundle.APIKeys, &bundle.APIBaseURLs
	if pinned {
		keys, urls = &bundle.PinnedAPIKeys, &bundle.PinnedAPIBaseURLs
	}
	if *keys == nil {
		*keys = map[secrets.Provider]string{}
	}
	(*keys)[prov] = key
	// A stale URL must never outlive the key it was paired with: written
	// with the key, or removed with it.
	if baseURL != "" {
		if *urls == nil {
			*urls = map[secrets.Provider]string{}
		}
		(*urls)[prov] = baseURL
	} else if *urls != nil {
		delete(*urls, prov)
	}
	// The server's fingerprint rides only with a gateway key: a vendor
	// key's identity is sha256 of the secret, which the runner recomputes
	// as it always has, so a bundle holding vendor keys only serialises
	// byte for byte as before this field existed. One entry per provider,
	// and a provider lives in one channel (every fill skips a provider the
	// other channel holds): the first channel to hold the key keeps its
	// fingerprint.
	if prov != secrets.ProviderOpenAICompatible {
		return
	}
	other := bundle.PinnedAPIKeys
	if pinned {
		other = bundle.APIKeys
	}
	if fingerprint != "" && other[prov] == "" {
		if bundle.APIKeyFingerprints == nil {
			bundle.APIKeyFingerprints = map[secrets.Provider]string{}
		}
		bundle.APIKeyFingerprints[prov] = fingerprint
	}
}

// clearAPIKey removes a provider's key from one channel together with its
// URL; the fingerprint goes when no channel holds a key for the provider
// any more. The twin of setAPIKey for the restore lane, which takes a
// shared tier's pinned key back out before the tenant's own refused key
// returns as the default.
func clearAPIKey(bundle *secrets.RunBundle, prov secrets.Provider, pinned bool) {
	if pinned {
		delete(bundle.PinnedAPIKeys, prov)
		delete(bundle.PinnedAPIBaseURLs, prov)
	} else {
		delete(bundle.APIKeys, prov)
		delete(bundle.APIBaseURLs, prov)
	}
	if bundle.APIKeys[prov] == "" && bundle.PinnedAPIKeys[prov] == "" {
		delete(bundle.APIKeyFingerprints, prov)
	}
}
