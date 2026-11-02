package runview

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Native action wiring (docs/adr/151, iterion#2834).

// Compile-time proof that the board's facade transport serves the engine's
// seam WITHOUT importing it: the seam's own doc requires structural
// satisfaction so the dispatcher never links the backend (iterion#2857),
// and this assertion is what fails the build the day the two signatures
// drift.
var _ model.ActionTransport = (*boardops.ActionTransport)(nil)

// NativeBoardTransport builds the board's native action transport over the
// instance's own store (#2465: the fs store a local surface opens, the
// tenant's Mongo board a cloud runner resolves through Config.BoardFor —
// never a forge's). sourceIssueID is the run's owning ticket, stamped as
// parent_id on a create the way every other board transport stamps it; the
// read-only facade of this lot never reads it, but the transport is built
// once for the facade's whole lifetime.
//
// The helper exists so the surfaces build the transport through ONE
// function: "a capability wired on one launch surface and silently inert on
// the others is a defect" is LocalConnectors' own lesson (ADR-098, round
// six), and it is cheaper to keep when the wiring is one call.
func NativeBoardTransport(store native.BoardStore, sourceIssueID string) model.ActionTransport {
	if store == nil {
		return nil
	}
	return boardops.NewActionTransport(store, boardops.CallEnv{SpawnParentID: sourceIssueID})
}

// actionTransportsOwn reports whether a registered transport already owns the
// connector id — the check that lets an explicit transport (the cloud
// runner's Mongo board) beat the fs-store default.
func actionTransportsOwn(transports []model.ActionTransport, connector string) bool {
	for _, t := range transports {
		if t != nil && t.Connector() == connector {
			return true
		}
	}
	return false
}

// WorkflowHasNativeBoardAction reports whether any tool node declares a
// board.* action — the gate a local surface reads BEFORE opening the fs
// store, because native.NewStore creates the directory and a run with no
// board action must not mint one.
func WorkflowHasNativeBoardAction(wf *ir.Workflow) bool {
	for _, n := range ActionNodes(wf) {
		if ir.NativeConnectorIDs[n.Connector] {
			return true
		}
	}
	return false
}
