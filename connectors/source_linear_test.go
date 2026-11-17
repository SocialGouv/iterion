package connectors_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// linearOrganizationID is the installation the sample and the double agree
// on: the organisation the operator binds the connection to.
const linearOrganizationID = "e5f6a7b8-9c0d-4e1f-8a2b-3c4d5e6f7a8b"

func init() {
	registerVendor("linear", vendorDouble{
		// Linear: HMAC-SHA256 of the raw body, hex, in `Linear-Signature`
		// (https://linear.app/developers/webhooks#verify-webhook-signature).
		// The instant is a member of the body, `webhookTimestamp`, in
		// milliseconds — so the vendor stamps it before signing, and the
		// double does the same to the frozen sample it is handed.
		// The instant travels IN the body, so the double stamps it there
		// before signing; `sign` stays a pure function of the bytes it covers.
		stamp: func(_ string, now time.Time, body []byte) []byte {
			stampLinearTimestamp(body, now)
			return body
		},
		sign: func(secret string, _ time.Time, body []byte) map[string]string {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			return map[string]string{"Linear-Signature": hex.EncodeToString(mac.Sum(nil))}
		},
		installation: linearOrganizationID,
		// Linear has no endpoint-verification handshake: a new webhook sends
		// nothing until an event fires.
	})
}

// stampLinearTimestamp rewrites the body's `webhookTimestamp` to the instant
// being signed, the way Linear stamps a delivery: the sample is frozen, and
// the replay window is now-relative. The number keeps its width, so the body
// is patched in place and the signature covers exactly the bytes the lane
// will verify.
func stampLinearTimestamp(body []byte, now time.Time) {
	const key = `"webhookTimestamp":`
	i := bytes.Index(body, []byte(key))
	if i < 0 {
		// Loud: a sample with no `webhookTimestamp` would leave the replay
		// window reading the frozen instant, and the witness would pass or
		// fail by the calendar rather than by the package.
		panic("the linear sample carries no " + key + " to stamp")
	}
	start := i + len(key)
	for start < len(body) && (body[start] == ' ' || body[start] == '\t') {
		start++
	}
	end := start
	for end < len(body) && body[end] >= '0' && body[end] <= '9' {
		end++
	}
	stamp := strconv.FormatInt(now.UnixMilli(), 10)
	if end-start != len(stamp) {
		panic("the linear sample's webhookTimestamp has no room for a now-relative stamp: keep it 13 digits")
	}
	copy(body[start:end], stamp)
}
