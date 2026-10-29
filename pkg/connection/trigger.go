package connection

import "errors"

// The `trigger` capability: what may be BELIEVED about an inbound delivery,
// decided when the connection is entrusted (#2761, the P1 lot of #1072).
//
// A connection says what may be SENT with it — `action` for a deterministic
// node, `agent` for the MCP facade. An inbound event is the same question read
// backwards: a delivery arrives claiming to be a vendor's, and something has
// to have decided, before it arrived, that this connection is the one whose
// secret may vouch for it. That decision belongs with the credential's, at the
// moment an operator entrusts the connection, and not in a handler that
// discovers it per request.
//
// WHERE THE SECRET LIVES is deliberately not here. A webhook's HMAC secret is
// already durable state with a cloud twin — webhooks.Config.HMACSecretSealed,
// sealed with an AAD bound to the webhook id, with pkg/webhooks/mongo.go
// beside it — while pkg/connection has file and memory stores only and its own
// doc (store.go:38-41) records the Mongo twin as still owed. Putting a second
// sealed secret on a record with no cloud half would be a durable seam that
// ships filesystem-only, which this repo calls a cloud hole rather than a
// known limitation. So the connection carries the GRANT and the webhook config
// carries the secret; the inbound half that binds one to the other is the next
// lot of #2761.
//
// Until that lot lands the grant is INERT, and it is read anyway — by the
// resolution chokepoint and by the CLI — for exactly the reason CapAgent is
// (see agent.go): a capability nobody checks is configuration that looks like
// a guarantee.

// ErrTriggerIngressUnwired is what a `trigger` grant is worth in this build.
//
// A SENTINEL for the same reason ErrAgentFacadeUnwired is one: "this
// connection declares the wrong capability" is fixed on the record by its
// operator, while "the capability it declares is one this build cannot
// honour" is fixed by shipping the ingress. A substring match on a message
// cannot carry that difference across a release.
var ErrTriggerIngressUnwired = errors.New("connection: the `trigger` capability is granted but inert — no connector webhook ingress reads it in this build (docs/adr/098-connector-catalog.md)")

// TriggerIngressWired reports whether this build can serve CapTrigger.
//
// The single source of the answer, like AgentFacadeWired: the day the inbound
// half exists there is one line to change and no second site still saying
// otherwise.
func TriggerIngressWired() bool { return false }
