package schedgate

import (
	"errors"
	"fmt"
)

// ErrMaintenanceHold means no launch was attempted. Durable consumers retain
// the obligation without spending their failure budget.
var ErrMaintenanceHold = errors.New("maintenance_hold")

// The typed holds below each wrap ErrMaintenanceHold, so every consumer that
// postpones on a hold keeps doing so; a scheduler that records a launch
// verdict on its row tells them apart. Their texts are stable: a recorded
// verdict built from one stays the same text across ticks.
var (
	// ErrMaintenanceFenceArmed is the hold of the operator's armed
	// maintenance fence.
	ErrMaintenanceFenceArmed = fmt.Errorf("%w: fence armed", ErrMaintenanceHold)
	// ErrMaintenanceFenceUnreadable is the hold of a maintenance fence
	// record that could not be read: no launch is admitted while the fence
	// state is unknown.
	ErrMaintenanceFenceUnreadable = fmt.Errorf("%w: maintenance.fence_launches could not be read", ErrMaintenanceHold)
	// ErrScheduleLeaseLost is the hold of a schedule launch whose holder no
	// longer owns the schedule's lease when it persists the run: a successor
	// owns the slot now, or will claim it on its next tick.
	ErrScheduleLeaseLost = fmt.Errorf("%w: schedule lease lost", ErrMaintenanceHold)
	// ErrLaunchNotPersisted is the hold of a schedule launch whose run
	// could not be persisted under the schedule's lease for any reason other
	// than a lost lease: the transaction could not start, or it failed.
	ErrLaunchNotPersisted = fmt.Errorf("%w: launched run could not be persisted", ErrMaintenanceHold)
)

// LaunchVerdictError is a launch refusal that names its stable class apart
// from its full text. The full text can embed a figure measured at the tick
// (the tenant's active-run count, a repository's monthly spend) that changes
// while the refusal lasts; a scheduler records LaunchVerdict on its row
// instead, so one refusal keeps one recorded verdict, hence one episode for
// the schedule-outcome watch. The full text stays in the tick's audit record.
// An empty LaunchVerdict means no class: the scheduler records the full text.
type LaunchVerdictError interface {
	error
	LaunchVerdict() string
}
