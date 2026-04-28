package observability

import "github.com/ethpandaops/agent-sdk-observability/errclass"

// SDK-local error classes. Define only where distinguishing the class on
// dashboards is worth the extra cardinality. The upstream classes
// (errclass.Timeout, errclass.RateLimited, errclass.Auth, errclass.Upstream5xx,
// errclass.Network, errclass.InvalidRequest, errclass.PermissionDenied,
// errclass.Canceled, errclass.Unknown) cover the cross-SDK cases.
const (
	ClassCLINotFound   errclass.Class = "cli_not_found"
	ClassProcessError  errclass.Class = "process_error"
	ClassParseError    errclass.Class = "parse_error"
	ClassOverload      errclass.Class = "overload"
	ClassPromptTooLong errclass.Class = "prompt_too_long"
	ClassBilling       errclass.Class = "billing"
	ClassExecution     errclass.Class = "execution"
)
