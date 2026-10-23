package platformcfg

import "time"

// Maintenance controls admission of new runs. Its zero value leaves launches
// enabled. Existing runs may still resume: this is not a runner drain.
type Maintenance struct {
	FenceLaunches bool      `json:"fence_launches" bson:"fence_launches"`
	Reason        string    `json:"reason" bson:"reason"`
	UpdatedAt     time.Time `json:"updated_at" bson:"updated_at"`
	UpdatedBy     string    `json:"updated_by,omitempty" bson:"updated_by,omitempty"`
}
