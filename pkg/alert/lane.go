package alert

import (
	"errors"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ObservedLane is the registration-scoped alert lane the manager publishes a
// born-registered alert through: the lane validates the identity and epoch of
// every publish against the registration it was handed out for, and Close
// ends that registration's epoch. The manager owns exactly one lane per
// registration; dispatch consumes the lane captured at the alert's birth —
// never a lane re-resolved by run ID (R3).
type ObservedLane interface {
	Publish(store.Event) error
	Close() error
}

// ErrNotRegistered reports a publish or registration against an episode that
// is over: the registration was released (or never adopted, or the manager
// already stopped). A refused publish is dropped with a warning, never
// re-routed by run ID.
var ErrNotRegistered = errors.New("alert: registration is over")
