package connectors_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

func init() {
	registerVendor("slack", vendorDouble{
		// Slack: HMAC-SHA256 over "v0:<timestamp>:<body>", hex behind "v0=",
		// in X-Slack-Signature, with the instant in X-Slack-Request-Timestamp.
		sign: func(secret string, now time.Time, body []byte) map[string]string {
			ts := strconv.FormatInt(now.Unix(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte("v0:" + ts + ":"))
			mac.Write(body)
			return map[string]string{
				"X-Slack-Request-Timestamp": ts,
				"X-Slack-Signature":         "v0=" + hex.EncodeToString(mac.Sum(nil)),
			}
		},
		installation: "T024BE7LD",
		// Slack signs the instant and puts it in X-Slack-Request-Timestamp.
		timestamp: true,
		// Slack verifies the request URL as soon as it is saved: a
		// `url_verification` body whose challenge must come back.
		handshake: func() (map[string]any, string) {
			return map[string]any{
				"type":      "url_verification",
				"challenge": "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P",
				"token":     "verification-token",
			}, "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"
		},
		// The inner event carries the vendor's name; the outer `event_callback`
		// is the same for all of them.
		eventPath: "payload.event.type",
		// A message the app itself posted carries its bot id on the event.
		echo: func(payload map[string]any) {
			payload["event"].(map[string]any)["bot_id"] = "B0APPSELF"
		},
	})
}
