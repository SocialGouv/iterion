package runview

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LocalConnectors builds the connector catalog + guarded client an IN-PROCESS
// run resolves its `tool … action:` nodes through, or (nil, nil, nil) when
// there is nothing to wire.
//
// It lives here, beside ExecutorSpec, because every local launch surface needs
// the same answer and only two of them had it: `iterion run` and `iterion
// resume`. The studio, the launch API, a board card and a subbot built their
// ExecutorSpec without it, so the SAME `.bot`, on the same machine, with the
// same catalog and the same sealer, ran from the CLI and failed at its first
// action node everywhere else. A capability wired on one launch surface and
// silently inert on the others is a defect, not a staging decision.
//
// Gated on the WORKFLOW rather than built always, exactly like the local
// secret store and for the same reason: constructing the resolver reads the
// catalog roots and the connection store, and a run with no `action:` node has
// no use for either.
//
// The SEALER is a parameter rather than built here: a surface that already
// holds one (the studio's, the dispatcher's lazy one) must not open a second,
// and a surface with none is a cloud one, whose connections come from
// elsewhere entirely.
func LocalConnectors(wf *ir.Workflow, workDir, storeDir string, sealer secrets.Sealer) (model.ConnectorResolver, *http.Client, error) {
	if sealer == nil || !WorkflowHasAction(wf) {
		return nil, nil, nil
	}
	// The workspace root is where a project may ship or pin its own
	// connectors; the global data dir is where an operator installs one for
	// every project. An empty workDir leaves the project tier out rather than
	// guessing at a cwd — on a server that cwd is wherever the process was
	// started, which is not the operator's project.
	paths := connection.LocalCatalogPaths(workDir, store.GlobalIterionDataDir())
	r, err := connection.LocalResolver(paths, storeDir, sealer)
	if err != nil {
		return nil, nil, err
	}
	if r == nil {
		// Nothing wired at all. A nil INTERFACE, not a nil *Resolver: the
		// executor asks `e.connectors == nil` to produce "this process has no
		// connector catalog wired", which names the fix — and a typed nil
		// inside the interface is not nil, so it would answer with the
		// resolver's own "not wired (catalog or store missing)" instead,
		// pointing at the connector rather than at the install.
		return nil, nil, nil
	}
	return r, connection.LocalHTTPClient(), nil
}

// localConnectors is the Service's own binding of LocalConnectors: its sealer
// (nil in cloud mode, where connections come from the tenant's store) and its
// workspace, with the same per-launch override precedence the engine's
// WorkDir already follows — the dispatcher points it at the per-issue
// worktree, so a project tier belongs to the issue's checkout and not to the
// daemon's cwd.
func (s *Service) localConnectors(wf *ir.Workflow, workDirOverride string) (model.ConnectorResolver, *http.Client, error) {
	workDir := s.workDir
	if workDirOverride != "" {
		workDir = workDirOverride
	}
	return LocalConnectors(wf, workDir, s.storeDir, s.localSealer)
}

// WorkflowHasAction reports whether any tool node declares a connector action.
func WorkflowHasAction(wf *ir.Workflow) bool {
	if wf == nil {
		return false
	}
	for _, n := range wf.Nodes {
		if t, ok := n.(*ir.ToolNode); ok && t.Action != "" {
			return true
		}
	}
	return false
}
