package forge

import (
	"context"
	"errors"
	"strings"
)

// ---------------------------------------------------------------------------
// Workflow dispatch — trigger one of a repository's own CI workflows on
// demand. The external trigger a forge-side cron that does not fire reliably
// needs: the server dispatches the workflow file a schedule names, through
// the deterministic publish surface, with a credential minted for that one
// call.
//
// Unlike MergeApprover, the server does not discover this capability by
// asserting it on a connection's client: that client holds the connection's
// runtime credential, and a dispatch is never made with it. The server builds
// the dispatcher per provider WITH the narrow per-call credential that
// provider can mint (pkg/server workflowDispatcherFor — today the GitHub App
// installation token), and answers every other connection with a typed
// "unsupported", never a silent no-op. A provider gaining a dispatch path
// implements this interface on its client AND brings its per-call credential
// there.
// ---------------------------------------------------------------------------

// WorkflowDispatch reports a dispatch the forge accepted.
type WorkflowDispatch struct {
	// Ref is the branch or tag the workflow was dispatched on — the
	// repository's default branch when the caller named none.
	Ref string
}

// WorkflowDispatcher dispatches the workflow file `workflow` (a basename
// under the repository's workflow directory, e.g. "renovate.yml") on ref.
// An empty ref dispatches on the repository's default branch, read from the
// forge rather than assumed.
//
// A refusal the forge ANSWERED — the workflow does not exist, it declares no
// manual trigger, the ref is unknown, the credential lacks the grant — is an
// error wrapping ErrWorkflowNotDispatched: nothing was queued. Any other
// error (no answer, a redirected write, a server-side 5xx) leaves the outcome
// unknown.
type WorkflowDispatcher interface {
	DispatchWorkflow(ctx context.Context, repo, workflow, ref string) (WorkflowDispatch, error)
}

// ErrWorkflowNotDispatched marks a dispatch the forge answered with a
// refusal, or one never sent because a read it depends on was refused:
// nothing was queued. It travels in the error chain so a caller tells "the
// forge said no" from "the forge may have queued it" without parsing text.
var ErrWorkflowNotDispatched = errors.New("forge: workflow not dispatched")

// ValidWorkflowFile reports whether name is a dispatchable workflow file
// name: a plain basename — no directory, no traversal, no URL syntax —
// ending in .yml or .yaml. The name lands in a forge API path, so anything
// that could address another resource is refused here rather than escaped.
// The character set is what makes it a basename: no separator, no
// percent-escape, no query or fragment can be spelled in it. A leading dot is
// refused on top (a hidden name, and the ".." of a traversal).
func ValidWorkflowFile(name string) bool {
	if len(name) > 255 || strings.HasPrefix(name, ".") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml")
	if stem == name || stem == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
