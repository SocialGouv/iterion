package workflowfile

// Snapshot contains the source unit and the resources its compiler reads.
// Paths are relative to one root; Main selects the executed entry, including
// bundle exports. It travels with the compiled program, never with a live path.
type Snapshot struct {
	Main   string            `json:"main"`
	Files  map[string]string `json:"files"`
	Bundle bool              `json:"bundle,omitempty"`
}
