package secrets

import (
	"context"
	"fmt"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ResolveLocalCredentials resolves the named secrets from a local (file-backed)
// GenericSecretStore into a Credentials value ready to stamp into a run ctx via
// WithCredentials. It is the in-process, single-hop equivalent of the cloud
// runner's injectCredentials (which decrypts a sealed per-run bundle shipped
// over NATS): here the store IS local, so resolution reads + opens the sealed
// values directly.
//
// names is typically the workflow's declared `secrets:` keys — the only names a
// `{{secrets.X}}` reference can legally use (compile-checked). Returns an empty
// (non-nil-map) Credentials when store/sealer is nil or names is empty, so
// callers can WithCredentials unconditionally.
func ResolveLocalCredentials(ctx context.Context, store GenericSecretStore, sealer Sealer, names []string, logger *iterlog.Logger) (Credentials, error) {
	creds := Credentials{Generic: map[string]string{}, GenericHosts: map[string][]string{}}
	if store == nil || sealer == nil || len(names) == 0 {
		return creds, nil
	}
	resolved, err := ResolveGeneric(ctx, store, LocalScopeTeam, "", names, sealer, logger)
	if err != nil {
		return Credentials{}, err
	}
	// Surface a decrypt failure rather than silently dropping the secret:
	// ResolveGeneric skips a name whose sealed blob fails to Open (corrupt
	// store or wrong master key). If a requested name IS present in the store
	// but absent from the result, it failed to decrypt — fail loudly at launch
	// instead of running the bot with the credential unset. A name simply not
	// in the store is left unresolved (the existing "optional/unset" behaviour).
	if len(resolved) < len(names) {
		present, lerr := store.ListByTeam(ctx, LocalScopeTeam, "")
		if lerr != nil {
			return Credentials{}, lerr
		}
		inStore := make(map[string]bool, len(present))
		for _, rec := range present {
			inStore[rec.Name] = true
		}
		for _, name := range names {
			if _, ok := resolved[name]; !ok && inStore[name] {
				return Credentials{}, fmt.Errorf("secrets: secret %q exists but could not be decrypted (corrupt store or wrong master key)", name)
			}
		}
	}
	for name, r := range resolved {
		creds.Generic[name] = string(r.Plaintext)
		if len(r.AllowedHosts) > 0 {
			creds.GenericHosts[name] = r.AllowedHosts
		}
	}
	return creds, nil
}
