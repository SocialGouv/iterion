package cli

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LocalSecretStores builds the local (non-cloud) secret stores: a required
// machine-global store at <GlobalIterionDataDir>/secrets.json and an optional
// per-project store at <projectStoreDir>/secrets.json when that directory is
// distinct from the global one. The project layer overrides the global by
// name (LayeredGenericSecretStore).
//
// projectStoreDir is the run's resolved store dir (the `.iterion` the studio /
// `--store-dir` points at). Pass "" to build a global-only store.
func LocalSecretStores(projectStoreDir string) (*secrets.LayeredGenericSecretStore, error) {
	return secrets.NewLocalLayeredStore(store.GlobalIterionDataDir(), projectStoreDir)
}

// localSecretsForRun builds the layered store + sealer for a CLI run. Returns
// (nil, nil, nil) when the workflow declares no secrets — so a plain run never
// touches the OS keychain. Any failure to build the sealer (e.g. a corrupt
// keychain entry) surfaces explicitly rather than silently proceeding without
// the declared secrets.
func localSecretsForRun(hasSecrets bool, projectStoreDir string, logger *iterlog.Logger) (secrets.GenericSecretStore, secrets.Sealer, error) {
	if !hasSecrets {
		return nil, nil, nil
	}
	var warn func(string, ...any)
	if logger != nil {
		warn = logger.Warn
	}
	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), warn)
	if err != nil {
		return nil, nil, err
	}
	st, err := LocalSecretStores(projectStoreDir)
	if err != nil {
		return nil, nil, err
	}
	return st, sealer, nil
}
