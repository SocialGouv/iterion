package sandbox

import (
	"cmp"
	"errors"
	"slices"
)

// ReadBackLossReason is the stable token a failed read-back drain carries
// — the `reason` of the sandbox_runfiles_drain_failed event — so a reader
// groups the losses of a run without parsing prose, the way
// run_attachment_lost's vocabulary lets it group lost attachments. A driver
// types each drain failure once, at the site that knows the reason
// (ReadBackLoss); the runtime reads the token back through every wrap
// above it (ReadBackLossReasonOf).
type ReadBackLossReason string

const (
	// ReadBackOverCap: the dir's set outgrew its cap (ReadBackDir.Cap). The
	// member the meter stopped on is dropped, what the attempt landed before
	// it stays, and the rest stays in the pod.
	ReadBackOverCap ReadBackLossReason = "over_cap"
	// ReadBackUnsettled: the tree kept changing under the pod's archiver
	// past the retry budget — a tool was still writing at teardown. What
	// stays on the host is the UNION of every attempt's members (tar never
	// deletes and no attempt settled for the sweep to keep against): a
	// stale superset of the pod's final tree, so a file the run removed
	// between attempts may be among what uploads (ReadBackLoss.Stale).
	ReadBackUnsettled ReadBackLossReason = "unsettled"
	// ReadBackArchiver: the pod's archiver failed for a cause no retry
	// cures — kubectl exec refused, tar died, the stream was cut or was no
	// archive.
	ReadBackArchiver ReadBackLossReason = "archiver"
	// ReadBackRefused: the host could not land the set — the host directory
	// could not be listed or written, a member could not be staged, landed
	// or replaced. A member the host declines on principle is not this:
	// that is ReadBackDeclined, and the drain goes on past it.
	ReadBackRefused ReadBackLossReason = "refused"
	// ReadBackDeclined: the set landed short of members the host declined
	// on principle (ReadBackDeclineWhy), each one named on the loss
	// (ReadBackLoss.Declined); every other member landed. A drain that
	// declined members and then failed carries its failure's own reason,
	// with the declined members beside it.
	ReadBackDeclined ReadBackLossReason = "declined"
	// ReadBackMixed: the declared dirs were lost for different reasons; the
	// joined error names each dir's own.
	ReadBackMixed ReadBackLossReason = "mixed"
	// ReadBackUnknown: the failure carries no typed reason — a driver that
	// did not type it.
	ReadBackUnknown ReadBackLossReason = "unknown"
)

// ReadBackDeclineWhy is the stable token naming why the host declined one
// member of the pod's archive — the `why` of each entry the
// sandbox_runfiles_drain_failed event lists under `declined`.
type ReadBackDeclineWhy string

const (
	// DeclinedUnsafeName: the member's name cannot be made safe — absolute,
	// climbing out of the tree, or reaching through a link that leaves it.
	DeclinedUnsafeName ReadBackDeclineWhy = "unsafe_name"
	// DeclinedLinkLeavesTree: a symlink whose target resolves outside the
	// tree — the member that turns a later write into a write anywhere.
	DeclinedLinkLeavesTree ReadBackDeclineWhy = "link_leaves_tree"
	// DeclinedHardlinkTarget: a hardlink whose target is no regular file the
	// same archive landed — absent, unsafe, declined itself, or another
	// kind. A hardlink to a file the archive did land lands as a copy.
	DeclinedHardlinkTarget ReadBackDeclineWhy = "hardlink_target"
	// DeclinedKind: a kind the host does not write back — a device, a fifo:
	// each a way to point a later write somewhere it was never meant to
	// reach, and none a file a tool produces as its product.
	DeclinedKind ReadBackDeclineWhy = "kind"
	// DeclinedPreDrainTree: a tree the host dir held before the drain stands
	// where the archive carries a file or a link — not the drain's to take.
	DeclinedPreDrainTree ReadBackDeclineWhy = "pre_drain_tree"
)

// ReadBackDeclinedMember is one member of a pod's archive the host
// declined: missing from the set the run uploads, for the reason Why.
type ReadBackDeclinedMember struct {
	// Dir is the declared container path the member was drained from.
	Dir string
	// Name is the member's path under Dir, slash-separated — as the
	// archive wrote it ("./" dropped) when the name itself could not be
	// made safe, so the refusal reads what the archive carried.
	Name string
	// Why is the reason the host declined it.
	Why ReadBackDeclineWhy
}

// ReadBackLoss is one drain failure typed with its reason. The driver wraps
// the cause at the site that knows why; the wraps above it — fmt.Errorf's
// %w, the errors.Join across declared dirs — keep it readable with
// errors.As, so the fact travels with the error instead of being read off
// its text.
type ReadBackLoss struct {
	Reason ReadBackLossReason
	Err    error
	// Declined names the members the host declined in the attempt that
	// ended the dir's drain, whatever Reason ended it: a drain that
	// declined members and then failed carries both facts.
	Declined []ReadBackDeclinedMember
	// Stale marks a set that may hold members the attempt that ended the
	// drain did not land: an earlier attempt raced a writer, tar never
	// deletes, and no settled attempt swept what it landed — so a file the
	// pod's final tree no longer has may upload. Always true for
	// ReadBackUnsettled; true for any other loss that followed a raced
	// attempt.
	Stale bool
}

func (l *ReadBackLoss) Error() string { return l.Err.Error() }

func (l *ReadBackLoss) Unwrap() error { return l.Err }

// eachReadBackBranch calls visit with the outermost typed loss of each
// branch of err — one per declared dir under the join ExportRunFiles
// returns — and with nil for a branch that types nothing. The outermost
// one, because a dir's chain is typed where each fact was learned: an
// unsettled drain wraps the archiver's last cause, and unsettled is what
// happened.
func eachReadBackBranch(err error, visit func(*ReadBackLoss)) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, branch := range joined.Unwrap() {
			eachReadBackBranch(branch, visit)
		}
		return
	}
	var loss *ReadBackLoss
	if errors.As(err, &loss) {
		visit(loss)
		return
	}
	visit(nil)
}

// ReadBackLossReasonOf reads the reason a drain failure carries. One
// declared dir's failure is a chain whose OUTERMOST typed loss is its reason
// — an unsettled drain wraps the archiver's last cause, and unsettled is what
// happened. A join across declared dirs is read branch by branch: the token
// the branches agree on, ReadBackMixed when they differ, and a branch that
// types nothing counts as ReadBackUnknown. A nil error carries no reason.
func ReadBackLossReasonOf(err error) ReadBackLossReason {
	if err == nil {
		return ""
	}
	var reasons []ReadBackLossReason
	eachReadBackBranch(err, func(loss *ReadBackLoss) {
		if loss == nil {
			reasons = append(reasons, ReadBackUnknown)
			return
		}
		reasons = append(reasons, loss.Reason)
	})
	if len(reasons) == 0 {
		return ReadBackUnknown
	}
	for _, r := range reasons[1:] {
		if r != reasons[0] {
			return ReadBackMixed
		}
	}
	return reasons[0]
}

// ReadBackDeclinedOf lists every member the host declined across the
// branches of a drain failure, ordered by dir, name and why — the order the
// event lists them in, whatever order the archives carried them in. A nil
// error, or one no branch of which declined anything, lists none.
func ReadBackDeclinedOf(err error) []ReadBackDeclinedMember {
	if err == nil {
		return nil
	}
	var out []ReadBackDeclinedMember
	eachReadBackBranch(err, func(loss *ReadBackLoss) {
		if loss != nil {
			out = append(out, loss.Declined...)
		}
	})
	slices.SortStableFunc(out, func(a, b ReadBackDeclinedMember) int {
		return cmp.Or(cmp.Compare(a.Dir, b.Dir), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Why, b.Why))
	})
	return out
}

// ReadBackStaleOf reports whether any branch of a drain failure left a set
// that may hold members its ending attempt did not land (ReadBackLoss.Stale).
func ReadBackStaleOf(err error) bool {
	if err == nil {
		return false
	}
	stale := false
	eachReadBackBranch(err, func(loss *ReadBackLoss) {
		if loss != nil && loss.Stale {
			stale = true
		}
	})
	return stale
}
