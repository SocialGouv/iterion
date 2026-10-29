package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// The connector half of `iterion validate`: each `action:` node resolved
// against the catalogue INSTALLED ON THIS MACHINE (ADR-098), which a compile
// cannot do — pkg/dsl/ir checks the shape of an action id and says so in its
// own doc ("whether the operation exists is a question for the connector
// package, which `iterion validate` resolves separately").
//
// OPT-IN, and that is the load-bearing part. The catalogue is a property of
// the install, not of the program: the same `.bot` has a servable action on
// the laptop that generated the package and an unservable one in CI, so a
// default that consulted it would make `validate`'s answer depend on the
// machine it ran on. Hence two rules this file keeps: without the flag nothing
// here runs and the result is byte-identical to what it was before the flag
// existed, and with the flag the findings are NOT diagnostics — they do not
// decide `valid` and they do not move the exit code (same contract as
// `--exec`'s report, for the same reason). What an unservable action costs is
// told at the surfaces that can refuse it: the executor's admission refusal
// (pkg/runview) and the resolver's (pkg/connection).

// ConnectorsReport is what `--connectors` adds to a validate: what the
// installed catalogue answers for every `action:` node of the program.
type ConnectorsReport struct {
	// Nodes is how many `action:` nodes were resolved — zero for a program
	// that declares none, which is why the report is present and empty
	// rather than absent: "asked, and there was nothing to resolve".
	Nodes int `json:"nodes"`
	// NoCatalog says, ONCE, that this install has no connector catalog at
	// all, and names the roots it would have read. Empty when a tier was
	// built.
	//
	// Once, rather than a finding per node: with no catalog, every node has
	// the same single cause — nothing is installed — and repeating it per
	// node reads as N problems with the program instead of one with the
	// install. The sentence is the one `connections add` prints, from the
	// same function, so an operator reads one wording at both surfaces.
	NoCatalog string `json:"no_catalog,omitempty"`
	// Findings are the action nodes this install cannot serve, in node-id
	// order (runview.ActionNodes sorts them: ir.Workflow.Nodes is a map, and
	// a list whose order is a map's is a list nothing can assert on).
	//
	// A node the catalogue CAN serve says nothing: the report is the list of
	// what an operator has to fix, and a per-node "ok" line turns a report
	// read at a glance into one that has to be read whole.
	Findings []ConnectorFinding `json:"findings,omitempty"`
}

// ConnectorFinding is one `action:` node the installed catalogue cannot serve.
type ConnectorFinding struct {
	NodeID string `json:"node_id"`
	// Action is the operation id the node names, `connector.resource.verb`.
	Action string `json:"action"`
	// Connector is the package half of Action — what has to be installed.
	Connector string `json:"connector"`
	// Connection is the alias the node would authenticate with. Carried
	// because an operator reading a list of findings fixes a connector and a
	// binding together, and the binding's name is in the `.bot` only.
	Connection string `json:"connection,omitempty"`
	// Reason is the RESOLVER's own sentence (connection.ResolveOperation),
	// not a second wording of it: whichever tier was consulted and why it did
	// not answer, or the operation the package does not declare. A copy here
	// would be the one that drifts, and the drift would be an operator
	// sent to the wrong directory.
	Reason string `json:"reason"`
}

// resolveConnectorActions resolves every `action:` node of a compiled program
// against the installed catalogue.
//
// The ERROR return is for a catalogue that is there and cannot be read (an
// unreadable root, as pkg/connection distinguishes it from an absent one) —
// the operator asked for a resolution and got none, which is said and exits
// non-zero, like a dry run that could not run. An install with NO catalog is
// not that error: it is the report's NoCatalog sentence, because "nothing is
// installed here" is an answer.
func resolveConnectorActions(wf *ir.Workflow) (*ConnectorsReport, error) {
	nodes := runview.ActionNodes(wf)
	report := &ConnectorsReport{Nodes: len(nodes)}
	if len(nodes) == 0 {
		// Nothing to resolve, so nothing is READ: a program with no `action:`
		// node has no use for the catalogue, which is the rule the run's own
		// builder already follows (pkg/runview.LocalConnectors returns nil
		// before it touches a root). It also keeps an install fault that
		// cannot concern this program from failing a question about it: an
		// unreadable root matters to a `.bot` that asks the catalogue
		// something, and this one asks nothing.
		return report, nil
	}
	tiers, paths, err := localConnectorTiers()
	if err != nil {
		return nil, err
	}
	if len(tiers) == 0 {
		report.NoCatalog = noConnectorCatalogReason(paths)
		return report, nil
	}
	// Built once for the whole program, not per node: the layered catalog
	// caches a loaded package, so a `.bot` with four nodes on one connector
	// reads it once — and, more importantly, every node is answered by the
	// same tier order, so two nodes naming the same connector can never be
	// reported against two different packages.
	catalog := connection.NewLayeredCatalog(tiers...)
	for _, n := range nodes {
		if _, _, err := connection.ResolveOperation(catalog, n.Action); err != nil {
			report.Findings = append(report.Findings, ConnectorFinding{
				NodeID:     n.NodeID,
				Action:     n.Action,
				Connector:  n.Connector,
				Connection: n.Connection,
				Reason:     err.Error(),
			})
		}
	}
	return report, nil
}

// renderConnectors prints the report under the human output, beneath the
// diagnostics and the dry run — the same place and shape the dry run's report
// occupies, indented so it reads as an attachment to the verdict rather than
// as part of it.
func renderConnectors(p *Printer, report *ConnectorsReport) {
	p.Blank()
	switch {
	case report.Nodes == 0:
		p.Line("  connectors: no `action:` node in this program")
	case report.NoCatalog != "":
		p.Line("  connectors: " + report.NoCatalog)
	case len(report.Findings) == 0:
		p.Line(fmt.Sprintf("  connectors: the installed catalogue serves every `action:` node (%d)", report.Nodes))
	default:
		p.Line(fmt.Sprintf("  connectors: %d of %d `action:` node(s) not served by the installed catalogue", len(report.Findings), report.Nodes))
		for _, f := range report.Findings {
			p.Line("    " + f.NodeID + " (" + f.Action + "): " + f.Reason)
		}
	}
}
