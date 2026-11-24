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
// beside it — so the connection carries the GRANT and the webhook config
// carries the secret. The inbound half that binds one to the other is the
// connector webhook lane (#2761): webhooks.Config.ConnectorConnectionID names
// the connection, and the lane (pkg/server/webhooks_connector.go) resolves it
// tenant-scoped, requires this grant, and matches the delivery against the
// package's declared events — a config without a binding keeps the legacy
// provider lane, byte-identical.

// ErrTriggerIngressUnwired is what a `trigger` grant is worth in a build with
// no ingress. UNREACHABLE since the connector webhook lane landed (the second
// half of #2761) and kept as the sentinel the predicate arms again the day a
// build ships without it — the capability table in agent.go reads it, and a
// substring match on a message could not carry "fixed by a release" versus
// "fixed on the record" across one.
var ErrTriggerIngressUnwired = errors.New("connection: the `trigger` capability is granted but inert — no connector webhook ingress reads it in this build (docs/adr/098-connector-catalog.md)")

// TriggerIngressWired reports whether this build can serve CapTrigger.
//
// The single source of the answer, like AgentFacadeWired: the day the ingress
// is gone there is one line to change and no second site still saying
// otherwise. TRUE since #2761: the lane is pkg/server/webhooks_connector.go,
// entered from the Forgejo webhook handler when the config names a
// connection, believing the delivery on the connection's `trigger` grant and
// the package's declared events.
func TriggerIngressWired() bool { return true }
