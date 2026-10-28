package runview

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

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

// ActionNode is one tool node declaring a connector action, reduced to what a
// refusal has to name: which node, which operation, which package has to be
// installed, and which connection would have authenticated the call.
type ActionNode struct {
	NodeID string
	// Action is the operation id, `connector.resource.verb`.
	Action string
	// Connector is the package half of Action — the thing a tier serves.
	Connector string
	// Connection is the alias the node authenticates with, empty when the
	// author wrote none (the compiler refuses that, C261; a hand-built IR can
	// still carry it).
	Connection string
}

// ActionNodes lists every tool node declaring a connector action, ordered by
// node id.
//
// SORTED rather than walked in map order. ir.Workflow.Nodes is a map, the list
// is read into a refusal an operator and a test both read, and a message whose
// order depends on which bucket Go visited first is a message nothing can
// assert on.
func ActionNodes(wf *ir.Workflow) []ActionNode {
	if wf == nil {
		return nil
	}
	out := make([]ActionNode, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		t, ok := n.(*ir.ToolNode)
		if !ok || t.Action == "" {
			continue
		}
		connector, _, _ := strings.Cut(t.Action, ".")
		out = append(out, ActionNode{NodeID: t.NodeID(), Action: t.Action, Connector: connector, Connection: t.Connection})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// UnservedActionsError is the admission refusal of a run this process cannot
// serve: the workflow declares `action:` nodes and no connector catalog is
// wired here.
//
// TYPED rather than a formatted string: "the plane serves no connector at all"
// is a deployment fact an operator fixes by installing a package, and a caller
// that wants to tell it apart from "this connection may not" (a grant fixed on
// the record) needs more than a substring.
type UnservedActionsError struct {
	// Nodes are the action nodes, ordered by node id.
	Nodes []ActionNode
}

func (e *UnservedActionsError) Error() string {
	var b strings.Builder
	b.WriteString("runview: this process wires no connector catalog, so the run is refused at admission — ")
	fmt.Fprintf(&b, "%d tool node(s) declare `action:`:", len(e.Nodes))
	for _, n := range e.Nodes {
		fmt.Fprintf(&b, " %q wants %s from connector %q", n.NodeID, n.Action, n.Connector)
		if n.Connection != "" {
			fmt.Fprintf(&b, " through connection %q", n.Connection)
		}
		b.WriteByte(';')
	}
	b.WriteString(" nothing here resolves a connector package or opens its credential." +
		" Locally: install the package in a tier this process reads" +
		" (`<iterion data dir>/connectors/<id>/`, or `<workspace>/connectors/<id>/` with ITERION_CONNECTOR_PROJECT_CATALOG=1 —" +
		" `iterion connectors gen` builds one) and bind a credential with `iterion connections add`." +
		" The platform and team tiers of docs/adr/098-connector-catalog.md, and the cloud connection store a server would" +
		" read, do not exist yet, so a cloud plane and a runner arrive here too." +
		" " + ConnectorAdmissionEnv + "=off keeps the old behaviour — the run starts and fails at the node.")
	return b.String()
}

// ConnectorAdmissionEnv turns the admission refusal below off for the process.
//
// A refusal that stops a run is a LOAD-BEARING bound, and a load-bearing bound
// keeps a greppable way out. Two legitimate uses, neither of them a defect in
// the refusal:
//
//   - a bot whose `action:` node is on a branch this launch will not take. It
//     ran to completion on a catalog-less plane before, and an operator who
//     knows that is entitled to keep it running while they install the
//     package.
//   - a caller that builds an executor to run ONE node rather than a run
//     (botreplay's scenario recorder), where a bot's unrelated action node is
//     not the node being executed.
//
// Spelled like ITERION_CONNECTOR_ALLOW_PRIVATE and for the same reason: the
// decision belongs to whoever runs the process, and the refusal names the
// variable so an operator does not have to find it.
const ConnectorAdmissionEnv = "ITERION_CONNECTOR_ADMISSION"

// admitConnectorActions refuses, at executor build, a workflow whose action
// nodes nothing here can resolve.
//
// At ADMISSION rather than at the node. The executor keeps its own refusal
// (executeToolNodeAction, "this process has no connector catalog wired") and
// must: the two guards protect different things, and only that one protects an
// IR built by a path that never came through here. But reaching it means the
// run already started, already spent whatever its earlier nodes cost, and
// already took whatever effects they had — for a verdict every one of its
// inputs was known before the first node ran. The honesty the catalog owes is
// that a `.bot` which cannot run here says so before it runs, naming the
// package to install rather than failing mid-flight at the node.
//
// The predicate is the executor's own — a nil INTERFACE, not a nil pointer
// inside one (LocalConnectors returns the former deliberately), so the two
// guards cannot disagree about what "wired" means.
func admitConnectorActions(wf *ir.Workflow, connectors model.ConnectorResolver) error {
	if connectors != nil {
		return nil
	}
	if os.Getenv(ConnectorAdmissionEnv) == "off" {
		return nil
	}
	nodes := ActionNodes(wf)
	if len(nodes) == 0 {
		return nil
	}
	return &UnservedActionsError{Nodes: nodes}
}
