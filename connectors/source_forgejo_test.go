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
		eventHeader: "X-Forgejo-Event",
		// A comment iterion posted comes from the project bot's account
		// (connectors/forgejo/setup.md), named on the sender.
		echo: func(payload map[string]any) {
			payload["sender"].(map[string]any)["login"] = "iterion-bot"
		},
		// The issue's `labels` are label objects, each with its `name`.
		lists: map[string]func(map[string]any) []string{
			"label": func(p map[string]any) []string {
				return deliveryStrings(deliveryAt(p, "issue", "labels"), "name")
			},
		},
	})
}
