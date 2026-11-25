package schedgate

import (
	"errors"
	"fmt"
)

// ErrMaintenanceHold means no launch was attempted. Durable consumers retain
// the obligation without spending their failure budget.
var ErrMaintenanceHold = errors.New("maintenance_hold")

// ErrMaintenanceFenceArmed is the hold of the operator's armed maintenance
// fence. It wraps ErrMaintenanceHold; the other holds — an unreadable fence
// record, a launch whose run could not be persisted — do not carry it.
var ErrMaintenanceFenceArmed = fmt.Errorf("%w: fence armed", ErrMaintenanceHold)
