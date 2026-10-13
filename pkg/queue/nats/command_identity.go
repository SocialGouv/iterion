package nats

import (
	"errors"
	"fmt"

	natsgo "github.com/nats-io/nats.go"

	"github.com/SocialGouv/iterion/pkg/store"
)

// HeaderAdmittedTenant / HeaderAdmittedPool carry the run's admitted
// identity on the control-plane commands (cancel, steer): the server
// stamps them from the run document's frozen stamp; the pod holding
// the run verifies them against the message it admitted before acting.
const (
	HeaderAdmittedTenant = "iterion-admitted-tenant"
	HeaderAdmittedPool   = "iterion-admitted-pool"
)

// ErrCommandIdentityMismatch marks a control-plane command that
// carries another run's admitted identity — or none. The holding pod
// ignores such a command: an honest server always stamps it, so an
// unstamped or mismatched one is an old binary or a forged one, and
// neither gets to cancel or steer a run across the boundary.
var ErrCommandIdentityMismatch = errors.New("queue/nats: control-plane command belongs to a different admitted identity")

// stampAdmitted writes the run's admitted identity onto a command's
// headers. Both fields are written even when empty: the pod verifies
// the pair, so a missing stamp is refused rather than skipped.
func stampAdmitted(h natsgo.Header, admitted store.LeaseIdentity) {
	h.Set(HeaderAdmittedTenant, admitted.TenantID)
	h.Set(HeaderAdmittedPool, admitted.Pool)
}

// verifyAdmitted checks a received command against the identity the
// receiving pod admitted for the run — the control-plane twin of the
// run lease's check (plan v2 §P5-c). Inter-pool commands are closed by
// code; intra-pool ones are the team boundary (one team per pool).
// The headers are operator-writable at the broker, so this is a guard
// against confusion and misrouting, not an active forger — the same
// accepted residual as the rest of the boundary.
func verifyAdmitted(m *natsgo.Msg, runID string, admitted store.LeaseIdentity) error {
	tenant, pool := m.Header.Get(HeaderAdmittedTenant), m.Header.Get(HeaderAdmittedPool)
	if tenant != admitted.TenantID || pool != admitted.Pool {
		return fmt.Errorf("queue/nats: command for run %s carries tenant %q pool %q, this pod admitted tenant %q pool %q: %w",
			runID, tenant, pool, admitted.TenantID, admitted.Pool, ErrCommandIdentityMismatch)
	}
	return nil
}
