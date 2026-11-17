package connectors_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

func init() {
	registerVendor("jira", vendorDouble{
		// Jira Cloud: HMAC-SHA256 of the raw request body, hex behind
		// "sha256=", in X-Hub-Signature
		// (https://developer.atlassian.com/cloud/jira/platform/webhooks/).
		// It signs no timestamp: the body digest is the replay protection,
		// and Jira has no endpoint-verification handshake.
		sign: func(secret string, _ time.Time, body []byte) map[string]string {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			return map[string]string{"X-Hub-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil))}
		},
	})
}
