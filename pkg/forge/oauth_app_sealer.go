package forge

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// SealOAuthAppSecret seals a forge OAuth app's client_secret, binding the
// sealed blob to the app id via AAD "forge_oauth_app:<id>" (same convention as
// forge_conn:<id> / generic_secret:<id>) so a sealed payload can't be silently
// transplanted to another app record.
func SealOAuthAppSecret(sealer secrets.Sealer, appID, clientSecret string) ([]byte, error) {
	if sealer == nil {
		return nil, fmt.Errorf("forge: nil sealer")
	}
	return sealer.Seal([]byte(clientSecret), forgeOAuthAppAAD(appID))
}

// OpenOAuthAppSecret returns an OAuth app's client_secret from its sealed blob.
func OpenOAuthAppSecret(sealer secrets.Sealer, appID string, sealed []byte) (string, error) {
	if sealer == nil {
		return "", fmt.Errorf("forge: nil sealer")
	}
	raw, err := sealer.Open(sealed, forgeOAuthAppAAD(appID))
	if err != nil {
		return "", fmt.Errorf("forge: open oauth app secret: %w", err)
	}
	return string(raw), nil
}

func forgeOAuthAppAAD(appID string) []byte {
	return []byte("forge_oauth_app:" + appID)
}

// SealForgeAppPrivateKey seals a manifest-created GitHub App's private key (PEM),
// bound to the app record id via a distinct AAD so it can't be confused with the
// OAuth client_secret sealed under the same record. Enables the least-privilege
// github_app (installation-token) path for per-tenant Apps.
func SealForgeAppPrivateKey(sealer secrets.Sealer, appID, pem string) ([]byte, error) {
	if sealer == nil {
		return nil, fmt.Errorf("forge: nil sealer")
	}
	return sealer.Seal([]byte(pem), forgeAppKeyAAD(appID))
}

// OpenForgeAppPrivateKey returns the App's private key PEM from its sealed blob.
func OpenForgeAppPrivateKey(sealer secrets.Sealer, appID string, sealed []byte) (string, error) {
	if sealer == nil {
		return "", fmt.Errorf("forge: nil sealer")
	}
	raw, err := sealer.Open(sealed, forgeAppKeyAAD(appID))
	if err != nil {
		return "", fmt.Errorf("forge: open app private key: %w", err)
	}
	return string(raw), nil
}

func forgeAppKeyAAD(appID string) []byte {
	return []byte("forge_oauth_app_key:" + appID)
}
