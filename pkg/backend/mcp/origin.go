package mcp

// Origin records WHO controls an MCP server's definition. It is the
// input to the launcher's start policy: under an active sandbox the
// launcher starts a server only when the operator put it there, because
// everything else the workflow declares already executes inside the
// container.
//
// The zero value is OriginUnknown, and it is untrusted on purpose: a
// catalog entry nobody classified must not inherit the operator's
// authority by default.
type Origin string

const (
	// OriginUnknown is the zero value: the catalog entry carries no
	// classification, so it is treated as workflow-controlled.
	OriginUnknown Origin = ""

	// OriginProject is a server the workflow's source tree declares —
	// a `.mcp.json` next to the `.bot` locally, or at the root of the
	// repository the cloud runner cloned.
	OriginProject Origin = "project"

	// OriginWorkflow is a server the bot author declares in the DSL
	// (`mcp_server:`).
	OriginWorkflow Origin = "workflow"

	// OriginPlugin is a server contributed by an enabled plugin whose
	// manifest was read from the operator's own plugin root — see
	// plugin.TrustedRoot: the root is captured from the inherited
	// environment before a project `.env` can point it elsewhere.
	OriginPlugin Origin = "plugin"
)

// OperatorControlled reports whether the operator chose this server's
// definition. It is the only origin the launcher starts while a sandbox
// is active.
func (o Origin) OperatorControlled() bool { return o == OriginPlugin }

// String renders the origin for diagnostics, naming the zero value
// rather than printing an empty string.
func (o Origin) String() string {
	if o == OriginUnknown {
		return "unknown"
	}
	return string(o)
}
