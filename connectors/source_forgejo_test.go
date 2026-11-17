package connectors_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func init() {
	registerVendor("forgejo", vendorDouble{
		// Forgejo: HMAC-SHA256 of the body, raw hex, in X-Forgejo-Signature.
		// It signs no timestamp, so there is no replay window.
		sign: func(secret string, _ time.Time, body []byte) map[string]string {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			return map[string]string{"X-Forgejo-Signature": hex.EncodeToString(mac.Sum(nil))}
		},
	})
}
