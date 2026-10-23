package schedgate

import "errors"

// ErrMaintenanceHold means no launch was attempted. Durable consumers retain
// the obligation without spending their failure budget.
var ErrMaintenanceHold = errors.New("maintenance_hold")
