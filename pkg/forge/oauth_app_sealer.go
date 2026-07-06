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
	return sealPlaintext(sealer, forgeOAuthAppAAD(appID), clientSecret)
}

// OpenOAuthAppSecret returns an OAuth app's client_secret from its sealed blob.
func OpenOAuthAppSecret(sealer secrets.Sealer, appID string, sealed []byte) (string, error) {
	return openPlaintext(sealer, forgeOAuthAppAAD(appID), sealed, "open oauth app secret")
}

func forgeOAuthAppAAD(appID string) []byte {
	return []byte("forge_oauth_app:" + appID)
}

// SealForgeAppPrivateKey seals a manifest-created GitHub App's private key (PEM),
// bound to the app record id via a distinct AAD so it can't be confused with the
// OAuth client_secret sealed under the same record. Enables the least-privilege
// github_app (installation-token) path for per-tenant Apps.
func SealForgeAppPrivateKey(sealer secrets.Sealer, appID, pem string) ([]byte, error) {
	return sealPlaintext(sealer, forgeAppKeyAAD(appID), pem)
}

// OpenForgeAppPrivateKey returns the App's private key PEM from its sealed blob.
func OpenForgeAppPrivateKey(sealer secrets.Sealer, appID string, sealed []byte) (string, error) {
	return openPlaintext(sealer, forgeAppKeyAAD(appID), sealed, "open app private key")
}

func forgeAppKeyAAD(appID string) []byte {
	return []byte("forge_oauth_app_key:" + appID)
}

// sealPlaintext is the shared nil-check + Seal body behind
// SealOAuthAppSecret and SealForgeAppPrivateKey.
func sealPlaintext(sealer secrets.Sealer, aad []byte, plaintext string) ([]byte, error) {
	if sealer == nil {
		return nil, errNilSealer
	}
	return sealer.Seal([]byte(plaintext), aad)
}

// openPlaintext is the shared nil-check + Open + error-wrap body behind
// OpenOAuthAppSecret and OpenForgeAppPrivateKey. errCtx names the
// operation in the wrapped error message (e.g. "open oauth app secret").
func openPlaintext(sealer secrets.Sealer, aad []byte, sealed []byte, errCtx string) (string, error) {
	if sealer == nil {
		return "", errNilSealer
	}
	raw, err := sealer.Open(sealed, aad)
	if err != nil {
		return "", fmt.Errorf("forge: %s: %w", errCtx, err)
	}
	return string(raw), nil
}
