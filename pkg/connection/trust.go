package connection

import (
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WHO WROTE THE CODE of the run a resolution serves, and what that buys
// (#1802, for the fork review lane of #874).
//
// A `tool … action:` node authenticates with the tenant's binding, and nothing
// on that path asked whose workflow it came from. A run over a workspace
// authored outside the tenant — the lane's store.RunTrustFork — would
// therefore have spent the tenant's credential on whatever the fork's own
// graph declared: an issue closed, a release deleted, a comment posted as the
// tenant. The lane's contract is a REVIEW, so it reads.
//
// The refusal lives at the RESOLUTION rather than at the node executor,
// because this is the one place a connector credential is produced
// (openCredential has a single caller, exec.Executor a single non-test
// construction). Every future caller of an action — a second executor, the MCP
// facade docs/adr/098-connector-catalog.md owes — inherits the refusal instead
// of having to remember it.

// ErrUntrustedMutation is what a mutating action is worth on a run whose
// workspace the tenant does not control.
//
// A SENTINEL because the two facts a caller has to tell apart are fixed in
// different places: this one is a property of the RUN (the same connection
// serves the same node on the tenant's own branch), while a CapabilityError is
// a grant an operator widens on the record.
var ErrUntrustedMutation = errors.New("connection: a run whose workspace was authored outside the tenant resolves read operations only")

// ErrTrustUnstated is a WIRING defect, not a lane decision: something built a
// resolver without saying whose code the run holds, so no mutating action can
// be admitted — the answer is unknown, not "trusted".
var ErrTrustUnstated = errors.New("connection: the resolver was built without stating who wrote the code in this run's workspace")

// ActionTrust is the run's trust AS A RESOLUTION RECEIVES IT: store.RunTrust
// plus the one bit store.RunTrust cannot carry — whether anybody stated it.
//
// store.RunTrust's zero value IS the trusted one, deliberately: every run
// written before that field existed decodes to it, and those runs are the
// operator's own. That makes it the wrong type for a REQUIRED input here. A
// bare `store.RunTrust` field on the resolver would read "nobody told me" as
// "the tenant's own code", so a builder that forgot it would be
// indistinguishable from one that knows — and the day a plane that serves
// forks wires a catalog, the hole would open with nothing in the tree saying
// so. A guard whose condition can never be true is documented as working;
// this one has to be wrong-proof BEFORE anything sets RunTrustFork.
//
// So stated-ness is part of the value: a mutating action resolves only when
// somebody said whose code this run holds AND the answer was "the tenant's".
// TrustOfRun states it; TrustUnknown says, on purpose, that it cannot be read.
type ActionTrust struct {
	trust  store.RunTrust
	stated bool
}

// TrustOfRun is the run document's own answer, which is where the decision
// belongs: trust rides the run (store.RunTrust), not the surface that launched
// it, because a resume, a usage-window retry and a forked child all rebuild
// their credentials from that document long after the launch.
func TrustOfRun(t store.RunTrust) ActionTrust { return ActionTrust{trust: t, stated: true} }

// TrustUnknown is the honest value of a builder that cannot read the run's
// trust — a subbot whose parent document does not load.
//
// Named rather than left to the zero value so the sites that mean it are
// greppable, and so the refusal it produces can say "nobody stated this"
// instead of accusing a lane that is not involved.
func TrustUnknown() ActionTrust { return ActionTrust{} }

// tenantsOwnCode reports whether the code in this run's workspace is the
// tenant's own.
//
// Written in the POSITIVE form, like store.RunTrust.Trusted(): it admits
// exactly the two facts that must both hold, so an unstated trust and a trust
// value this binary does not recognise (a document written by a newer replica
// mid-rollout, a hand-edited row) are refused by construction rather than by
// enumeration.
//
// ONE predicate for the two decisions that rest on this single fact — whether a
// mutating action resolves (mayMutate) and whether the workspace's own
// connector packages may be BELIEVED about what an operation does
// (projectTierConsulted). Two copies would let a resolver refuse the mutation
// and still read the effect from the package the reviewed tree declared, which
// is #2769 exactly: the refusal's predicate is `effect:` as the catalog served
// it, so the tier that serves and the verdict that reads it cannot answer to
// different rules.
func (t ActionTrust) tenantsOwnCode() bool { return t.stated && t.trust.Trusted() }

// mayMutate reports whether a resolution serving this run may hand over a
// credential for an operation that changes remote state.
func (t ActionTrust) mayMutate() bool { return t.tenantsOwnCode() }

// TrustError is the resolution chokepoint's typed refusal of an action whose
// effect this run's trust does not buy.
//
// Typed, and separate from CapabilityError: a caller that could not tell them
// apart would send an operator to widen a capability that is already right.
type TrustError struct {
	// Action is the operation id the node named.
	Action string
	// Alias is the connection the node authenticates with, empty when the IR
	// carried none.
	Alias string
	// Effect is what the operation does to the remote system.
	Effect spec.Effect
	// Trust is the run's trust as the resolver received it.
	Trust ActionTrust
}

func (e *TrustError) Error() string {
	if e.unstated() {
		return fmt.Sprintf("operation %q %s, and nobody stated who wrote the code in this run's workspace, so connection %q hands over nothing:"+
			" whoever builds the resolver states the run's trust (connection.TrustOfRun(run.Trust))",
			e.Action, e.effectPhrase(), e.Alias)
	}
	return fmt.Sprintf("operation %q %s, and this run's workspace holds code the tenant does not control (trust=%q), so connection %q hands over nothing"+
		" — a review lane resolves read operations only",
		e.Action, e.effectPhrase(), string(e.Trust.trust), e.Alias)
}

// Unwrap tells the two apart for a caller that branches: a lane decision, or a
// resolver nobody told.
func (e *TrustError) Unwrap() error {
	if e.unstated() {
		return ErrTrustUnstated
	}
	return ErrUntrustedMutation
}

// unstated is the ONE predicate the message and the sentinel share. Two copies
// would let the sentence and the branch disagree, and the sentence is what an
// operator acts on while the branch is what code acts on.
func (e *TrustError) unstated() bool { return !e.Trust.stated }

// effectPhrase words the effect a refusal names.
//
// An effect this build does not know — a typo, a package from a newer schema —
// is mutating by spec.Effect.Mutating(), which fails safe; the sentence has to
// say THAT rather than assert what the operation does.
func (e *TrustError) effectPhrase() string {
	if e.Effect.Known() {
		return fmt.Sprintf("has effect %q, which changes remote state", string(e.Effect))
	}
	return fmt.Sprintf("declares effect %q, which this build does not know and therefore reads as mutating", string(e.Effect))
}
