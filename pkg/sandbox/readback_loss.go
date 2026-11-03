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
	// ReadBackOverCap: the dir's set outgrew its cap (ReadBackDir.Cap) — the
	// set the host holds, every attempt of a retried drain counted
	// together, never one archive alone. The member the meter stopped on
	// is dropped, what landed before it stays, and the rest stays in the
	// pod.
	ReadBackOverCap ReadBackLossReason = "over_cap"
	// ReadBackUnsettled: the tree kept changing under the pod's archiver
	// past the retry budget — a tool was still writing at teardown. When
	// more than one attempt ran, what stays on the host is the UNION of
	// every attempt's members (tar never deletes and no attempt settled
	// for the sweep to keep against), as far as the dir's cap holds it — a
	// later attempt's member that needed the room swept what only earlier
	// attempts landed: a stale superset of the pod's final tree, so a file
	// the run removed between attempts may be among what uploads
	// (ReadBackLoss.Stale). A single attempt — a retry budget of one, or a
	// drain whose context ended before the second — leaves its own raced
	// archive and no union: not stale.
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
	// on principle (ReadBackDeclineWhy), every one counted on the loss
	// (ReadBackLoss.DeclinedCount) and the first ReadBackDeclinedListed
	// named (ReadBackLoss.Declined); every other member landed. A drain
	// that declined members and then failed carries its failure's own
	// reason, with the declined members beside it.
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

// ReadBackDeclinedListed bounds how many declined members a drain names one
// by one — on its loss (ReadBackLoss.Declined, per declared dir), on the
// sandbox_runfiles_drain_failed event across the dirs, and in the runner's
// log: the count carries every one (ReadBackLoss.DeclinedCount). An archive
// is the pod's to fill, and every name it holds is a record somewhere — an
// archive of a hundred thousand fifos must cost a counter, not a hundred
// thousand entries in memory, in the run's log and on its stream.
const ReadBackDeclinedListed = 100

// compareDeclined is the one order declined members are named in — dir,
// then name, then why — whatever order the dirs drained or the archives
// carried them in: the bound keeps the first ones in it, the event lists
// them in it.
func compareDeclined(a, b ReadBackDeclinedMember) int {
	return cmp.Or(cmp.Compare(a.Dir, b.Dir), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Why, b.Why))
}

// ReadBackDeclinedSet collects the members one drain declines, bounded
// however many the archive carries: Count is every one, List the first
// ReadBackDeclinedListed in dir, name and why order — the same members
// whichever order they arrived in. The zero value is ready to use.
type ReadBackDeclinedSet struct {
	count int
	// kept holds at most twice the bound between two trims: a trim keeps
	// the bound's first in order, so what was dropped can never be among
	// the first again — every later arrival only competes with them.
	kept []ReadBackDeclinedMember
}

// Add records one declined member.
func (s *ReadBackDeclinedSet) Add(m ReadBackDeclinedMember) {
	s.count++
	s.kept = append(s.kept, m)
	if len(s.kept) >= 2*ReadBackDeclinedListed {
		s.trim()
	}
}

// Count is how many members were declined in all.
func (s *ReadBackDeclinedSet) Count() int { return s.count }

// List returns the first ReadBackDeclinedListed declined members in dir,
// name and why order — a slice the caller owns.
func (s *ReadBackDeclinedSet) List() []ReadBackDeclinedMember {
	s.trim()
	return slices.Clone(s.kept)
}

func (s *ReadBackDeclinedSet) trim() {
	slices.SortStableFunc(s.kept, compareDeclined)
	s.kept = slices.Clip(s.kept[:min(len(s.kept), ReadBackDeclinedListed)])
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
	// declined members and then failed carries both facts. At most the
	// first ReadBackDeclinedListed in dir, name and why order
	// (ReadBackDeclinedSet); DeclinedCount says how many there were.
	Declined []ReadBackDeclinedMember
	// DeclinedCount is how many members that attempt declined in all —
	// Declined's length when it named every one. Read through
	// ReadBackDeclinedCountOf, which takes the larger of the two, so a loss
	// built without the count still counts what it names.
	DeclinedCount int
	// Stale marks a set that may hold members the attempt that ended the
	// drain did not land: an earlier attempt raced a writer, tar never
	// deletes, and no settled attempt swept what it landed (a later member
	// that needed their room under the cap may have swept some of it) — so
	// a file the pod's final tree no longer has may upload. True for any
	// loss that followed a raced attempt, ReadBackUnsettled of more than
	// one attempt included; false where no earlier attempt ran — a first
	// attempt's failure, or an unsettled drain whose one attempt was all it
	// had.
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

// ReadBackDeclinedOf lists the members the host declined that the branches
// of a drain failure name, ordered by dir, name and why — the order the
// event lists them in, whatever order the archives carried them in. Each
// branch names at most its own first ReadBackDeclinedListed
// (ReadBackLoss.Declined), so the first ReadBackDeclinedListed of this
// list are the first of every declined member. A nil error, or one no
// branch of which declined anything, lists none.
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
	slices.SortStableFunc(out, compareDeclined)
	return out
}

// ReadBackDeclinedCountOf is how many members the host declined across the
// branches of a drain failure — every one, named or not: each branch counts
// its DeclinedCount, or the members it names when that is larger.
func ReadBackDeclinedCountOf(err error) int {
	if err == nil {
		return 0
	}
	total := 0
	eachReadBackBranch(err, func(loss *ReadBackLoss) {
		if loss != nil {
			total += max(loss.DeclinedCount, len(loss.Declined))
		}
	})
	return total
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
