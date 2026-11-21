package store

// CorrectionAccounting is the checkpoint carrier for the fenced correction
// protocol. It is separate from Parallel: retiring a branch or rewinding a
// cursor must not erase already-paid usage. No Checkpoint field activates it
// until the runtime's owned snapshot/restore callsites are composed.
// Mark keys are framed hashes. Daily scopes retain their actual ledger keys;
// the carrier must be written as a whole value, never as a dotted $set path.
type CorrectionAccounting struct {
	IterationMarks    map[string]bool                  `json:"iteration_marks,omitempty" bson:"iteration_marks,omitempty"`
	UnpricedNodeMarks map[string]bool                  `json:"unpriced_node_marks,omitempty" bson:"unpriced_node_marks,omitempty"`
	Version           int                              `json:"version" bson:"version"`
	UsageMarks        map[string]OutputCorrectionUsage `json:"usage_marks,omitempty" bson:"usage_marks,omitempty"`
	UnpricedMarks     map[string]int                   `json:"unpriced_marks,omitempty" bson:"unpriced_marks,omitempty"`
	DailyMarks        map[string]float64               `json:"daily_marks,omitempty" bson:"daily_marks,omitempty"`
	DailyScopeCostUSD map[string]float64               `json:"daily_scope_cost_usd,omitempty" bson:"daily_scope_cost_usd,omitempty"`
}
