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
// (nil, nil, nil) when the workflow declares no connector action.
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
// unlock it has no use for.
func localConnectorsForRun(wf *ir.Workflow, storeDir string, logger *iterlog.Logger) (model.ConnectorResolver, *http.Client, error) {
	if !runview.WorkflowHasAction(wf) {
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
	// A CLI run's workspace is the operator's own checkout, which is
	// store.RunTrustDefault by definition — `iterion run` has no trust flag
	// and nothing here reads a fork's head. Stated explicitly all the same:
	// the resolver refuses a mutating action until somebody says, so the one
	// surface whose answer is a property of the surface says it out loud
	// rather than relying on a zero value (#1802).
	return runview.LocalConnectors(wf, wd, storeDir, sealer, connection.TrustOfRun(store.RunTrustDefault))
}
