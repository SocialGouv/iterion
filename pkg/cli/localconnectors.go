package cli

import (
	"net/http"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// localConnectorsForRun builds the connector resolver a CLI run uses, or
// (nil, nil, nil) when the workflow declares no action the catalog serves.
//
// The CLI's own half is the two things that differ from every other surface:
// its workspace is the cwd (a `.bot` is run from the project it acts on), and
// it owns no sealer, so it opens one. The rest is runview.LocalConnectors,
// shared with the studio, the launch API, a subbot and the dispatcher —
// answering "which catalog does an in-process run read" once.
//
// Gated on the WORKFLOW rather than built always, exactly like
// localSecretsForRun and for the same reason: opening the local sealer touches
// the OS keychain, and a run with no `action:` node must not prompt for an
// unlock it has no use for. A native action (board.*) is no such node: its
// transport is registered by the executor's build, with no catalog and no
// credential, so a workflow whose only actions are native opens no sealer.
//
// The TRUST is the run's, stated by the caller that holds it: the launch
// trust of a root `iterion run` (cliLaunchTrust, lowered by a carried
// ITERION_RUN_TRUST), a sub-bot child's parent document, a resumed run's own
// document. A CLI run is not the operator's by virtue of the surface — a node
// process of an untrusted run re-enters the CLI in the outsider's checkout —
// so the resolver refuses a mutating action and skips the workspace tier on
// exactly the runs the compile and the engine already treat as untrusted
// (#1802).
func localConnectorsForRun(wf *ir.Workflow, storeDir string, logger *iterlog.Logger, trust connection.ActionTrust) (model.ConnectorResolver, *http.Client, error) {
	if !runview.WorkflowHasCatalogAction(wf) {
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
	wd, _ := os.Getwd()
	return runview.LocalConnectors(wf, wd, storeDir, sealer, trust)
}
