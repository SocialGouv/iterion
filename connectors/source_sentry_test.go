package connectors_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func init() {
	registerVendor("sentry", vendorDouble{
		// Sentry: HMAC-SHA256 of the exact, unparsed request body, hexadecimal,
		// in Sentry-Hook-Signature, keyed by the internal integration's Client
		// Secret. Sentry signs no timestamp with it, so there is no replay
		// window and no instant header to send.
		// https://docs.sentry.io/organization/integrations/integration-platform/webhooks/
		sign: func(secret string, _ time.Time, body []byte) map[string]string {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			return map[string]string{"Sentry-Hook-Signature": hex.EncodeToString(mac.Sum(nil))}
		},
		// The installation a webhook body names: every payload carries
		// `installation.uuid`, and the internal integration is installed on
		// one organization.
		installation: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
	})
}
