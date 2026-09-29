package mcp

import (
	"errors"
	"fmt"
)

// StartPolicy decides which of the catalog's servers this launcher may
// start. It exists because a launcher-started MCP server is the one
// placement a sandbox cannot contain: the process runs beside the
// launcher — an operator's laptop, or the cloud runner pod — with the
// launcher's environment, whatever isolation the run declared.
//
// The zero value refuses everything the operator did not put there, so
// a manager nobody armed never starts a workflow-controlled server.
type StartPolicy int

const (
	// StartPolicyUnknown is the zero value: the run's sandbox question
	// is not settled (or nobody answered it), so only
	// operator-controlled servers start. It is deliberately identical
	// in effect to StartOperatorServersOnly and distinct in name: a
	// manager that was never armed must be greppable as such, not read
	// as a deliberate restriction.
	StartPolicyUnknown StartPolicy = iota

	// StartOperatorServersOnly is the armed restriction: a sandbox is
	// active for this run, so only operator-controlled servers start
	// here. Everything else the workflow declares runs in the
	// container, where the CLI backends start their own servers.
	StartOperatorServersOnly

	// StartAllServers is the unsandboxed case: the run executes beside
	// the launcher anyway, so a workflow-controlled server on the
	// launcher adds no exposure the run does not already have.
	StartAllServers
)

// Allows reports whether a server of this origin may be started.
func (p StartPolicy) Allows(o Origin) bool {
	if p == StartAllServers {
		return true
	}
	return o.OperatorControlled()
}

// String renders the policy for diagnostics.
func (p StartPolicy) String() string {
	switch p {
	case StartOperatorServersOnly:
		return "operator servers only (sandbox active)"
	case StartAllServers:
		return "all servers (no sandbox)"
	default:
		return "operator servers only (sandbox undecided)"
	}
}

// ServerNotStartableError is the typed refusal to start a server on the
// launcher. Callers distinguish it from a boot failure: a boot failure
// says the server is broken, this says it must not run here.
type ServerNotStartableError struct {
	Server string
	Origin Origin
	Policy StartPolicy
	// Cause is a reason this server could not have started anyway, known
	// before the policy was consulted — a malformed auth block, say. It is
	// carried INSIDE the refusal rather than returned instead of it, so a
	// caller asking "may this run here" gets the same typed answer whether or
	// not the server is also broken: the placement question does not depend
	// on the server's health.
	Cause error
}

func (e *ServerNotStartableError) Unwrap() error { return e.Cause }

func (e *ServerNotStartableError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf(
			"mcp: server %q (origin: %s) is not started by the launcher — %s "+
				"(it could not have started here in any case: %v)",
			e.Server, e.Origin, e.Policy, e.Cause)
	}
	return fmt.Sprintf(
		"mcp: server %q (origin: %s) is not started by the launcher — %s. "+
			"Its process would run beside the launcher, outside the run's sandbox. "+
			"Route the node to a backend that starts it inside the container (claude_code, pi), "+
			"or run the workflow unsandboxed (`sandbox: none` / `--sandbox none`)",
		e.Server, e.Origin, e.Policy)
}

// ServerNotStartable reports whether err is (or wraps) the refusal.
// The sites that must tell a refusal from a boot failure — the ambient
// splice, the explicit tool references, the health check — ask here
// rather than matching on message text.
func ServerNotStartable(err error) bool {
	var target *ServerNotStartableError
	return errors.As(err, &target)
}
