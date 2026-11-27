package queue

import "time"

// Fleet roles a presence record names.
const (
	FleetRoleServer = "server"
	FleetRoleRunner = "runner"
)

// FleetMember is the presence record one live server or runner process keeps
// fresh in the fleet registry (pkg/queue/nats presence.go). The registry is
// what answers "which builds can execute a run launched NOW" — the engine
// floor a bot push is held against — so the record carries exactly what
// decides that: the build, the rollout epoch (a runner below every
// publisher's epoch refuses every new message), and whether a runner has
// stopped fetching (a draining runner takes no new run).
type FleetMember struct {
	Role     string `json:"role"`
	Instance string `json:"instance"`
	Build    string `json:"build"`
	Epoch    uint64 `json:"epoch"`
	Pool     string `json:"pool,omitempty"`
	// Draining is set by a runner once it has stopped fetching: it only
	// finishes the run it holds. A server never sets it — a server in its
	// lame-duck window still answers the requests routed to it.
	Draining  bool      `json:"draining"`
	StartedAt time.Time `json:"started_at"`
	// SeenAt is when the registry last stored the record: read off the KV
	// entry, never written by the process itself.
	SeenAt time.Time `json:"seen_at"`
}
