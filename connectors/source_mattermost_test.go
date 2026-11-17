package connectors_test

import (
	"encoding/json"
	"time"
)

func init() {
	registerVendor("mattermost", vendorDouble{
		// Mattermost signs nothing: an outgoing webhook proves itself with the
		// token the operator set on the hook, carried in the BODY under
		// `token` (server/public/model/outgoing_webhook.go;
		// https://docs.mattermost.com/developers/integrate/webhooks/outgoing).
		// The double therefore sends no header; the sample's `token` is
		// whatever the hook was created with, and the stamp below replaces it
		// with the secret the lane holds.
		sign: func(string, time.Time, []byte) map[string]string { return nil },
		// The credential is a BODY field the hook's sender fills in from the
		// token the operator configured, so the double stamps it there before
		// the delivery is signed and read — never in a header.
		stamp: func(secret string, _ time.Time, body []byte) []byte {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				panic("the mattermost delivery body is not an object: " + err.Error())
			}
			m["token"] = secret
			out, err := json.Marshal(m)
			if err != nil {
				panic("encode the mattermost delivery body: " + err.Error())
			}
			return out
		},
		// The team the delivery names; a system-console hook serves several, so
		// the connection is bound to one.
		installation: "k3y8b7nqj7n8xr9e5t6m1wz1ho",
	})
}
