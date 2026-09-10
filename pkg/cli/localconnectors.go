package cli

import (
	"net/http"
	"os"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// localConnectorsForRun builds the connector resolver a CLI run uses, or
// (nil, nil, nil) when the workflow declares no connector action.
//
// Gated on the WORKFLOW rather than built always, exactly like
// localSecretsForRun and for the same reason: building the resolver opens the
// local sealer, which touches the OS keychain. A run with no `action:` node
// must not prompt for a keychain unlock it has no use for.
func localConnectorsForRun(wf *ir.Workflow, storeDir string, logger *iterlog.Logger) (*connection.Resolver, *http.Client, error) {
	if !workflowHasAction(wf) {
		return nil, nil, nil
	}
	var warn func(string, ...any)
	if logger != nil {
		warn = logger.Warn
	}
	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), warn)
	if err != nil {
		return nil, nil, err
	}
	// The workspace root is where a project may ship its own connectors; the
	// global data dir is where an operator installs them for every project.
	wd, _ := os.Getwd()
	paths := connection.LocalCatalogPaths(wd, store.GlobalIterionDataDir())
	r, err := connection.LocalResolver(paths, storeDir, sealer)
	if err != nil {
		return nil, nil, err
	}
	if r == nil {
		// Nothing wired at all. Left nil so the node's own diagnostic says so
		// — "this process has no connector catalog wired" points at the fix,
		// where a resolution error would read as though the connector were
		// the problem.
		return nil, nil, nil
	}
	return r, connection.LocalHTTPClient(), nil
}

// workflowHasAction reports whether any tool node declares a connector action.
func workflowHasAction(wf *ir.Workflow) bool {
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
