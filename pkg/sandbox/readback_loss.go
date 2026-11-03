package sandbox

import "errors"

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
	// member the meter stopped on is dropped, what landed before it stays,
	// and the rest stays in the pod.
	ReadBackOverCap ReadBackLossReason = "over_cap"
	// ReadBackUnsettled: the tree kept changing under the pod's archiver
	// past the retry budget — a tool was still writing at teardown. What
	// stays on the host is the UNION of every attempt's members (tar never
	// deletes and no attempt settled for the sweep to keep against): a
	// stale superset of the pod's final tree, so a file the run removed
	// between attempts may be among what uploads.
	ReadBackUnsettled ReadBackLossReason = "unsettled"
	// ReadBackArchiver: the pod's archiver failed for a cause no retry
	// cures — kubectl exec refused, tar died, the stream was cut or was no
	// archive.
	ReadBackArchiver ReadBackLossReason = "archiver"
	// ReadBackRefused: the host could not land the set — the host directory
	// could not be listed or written, a member could not be staged, landed
	// or replaced. A member the host declines on principle (a name that
	// cannot be made safe, a link that leaves the tree, a kind it does not
	// write back, a pre-drain tree where the archive carries a file or a
	// link) is not this: it is skipped and reported, and the drain goes on.
	ReadBackRefused ReadBackLossReason = "refused"
	// ReadBackMixed: the declared dirs were lost for different reasons; the
	// joined error names each dir's own.
	ReadBackMixed ReadBackLossReason = "mixed"
	// ReadBackUnknown: the failure carries no typed reason — a driver that
	// did not type it.
	ReadBackUnknown ReadBackLossReason = "unknown"
)

// ReadBackLoss is one drain failure typed with its reason. The driver wraps
// the cause at the site that knows why; the wraps above it — fmt.Errorf's
// %w, the errors.Join across declared dirs — keep it readable with
// errors.As, so the fact travels with the error instead of being read off
// its text.
type ReadBackLoss struct {
	Reason ReadBackLossReason
	Err    error
}

func (l *ReadBackLoss) Error() string { return l.Err.Error() }

func (l *ReadBackLoss) Unwrap() error { return l.Err }

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
	var walk func(error)
	walk = func(e error) {
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, branch := range joined.Unwrap() {
				walk(branch)
			}
			return
		}
		var loss *ReadBackLoss
		if errors.As(e, &loss) {
			reasons = append(reasons, loss.Reason)
			return
		}
		reasons = append(reasons, ReadBackUnknown)
	}
	walk(err)
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
