package runview

import (
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ResumeAdmission is what a resume's admission gate judged and metered: the
// run's trust, and on an untrusted run the fork author's metering subject the
// gate charged beside the org's budget (ADR-162 decision 5). A surface that
// crossed the launch gate stamps it on ResumeSpec.Admitted; a surface that
// crossed none leaves it nil.
type ResumeAdmission struct {
	Trust             store.RunTrust
	ForkAuthorSubject string
}

// ResumeAdmissionOf is the admission a resume of run must carry: the trust
// the document records and, on an untrusted run, the author subject its
// launch metered. A trusted run names no author subject whatever its document
// holds — the gate meters none on a trusted subject.
func ResumeAdmissionOf(run *store.Run) ResumeAdmission {
	a := ResumeAdmission{Trust: run.Trust}
	if !run.Trust.Trusted() {
		a.ForkAuthorSubject = run.ForkAuthorSubject
	}
	return a
}

// ErrResumeNotAdmitted reports a resume whose admission does not match the
// run it would re-enter: an untrusted run resumed by a surface that crossed
// no admission gate (answer-human, an async answer's auto-resume, an
// assistant mission), or an admission judged on a trust or an author subject
// the run document does not carry. Nothing was claimed or published.
var ErrResumeNotAdmitted = errors.New("runview: resume not admitted for this run's trust")

// CheckResumeAdmitted is the one comparison every resume crosses — the run
// service before it compiles or spawns anything, and the cloud publisher
// against the document it republishes from — so an untrusted run re-enters
// the engine only on an admission that metered its own bound, whichever
// surface asked (ADR-162 decision 5: "whichever surface enters"). A trusted
// run with no admission keeps the run-by-id ladder's unmetered resume
// unchanged; an admission, when present, must name exactly the run's own.
func CheckResumeAdmitted(run *store.Run, admitted *ResumeAdmission) error {
	want := ResumeAdmissionOf(run)
	if admitted == nil {
		if run.Trust.Trusted() {
			return nil
		}
		return fmt.Errorf("%w: run %s is %q and this resume crossed no admission gate, so neither the org budget nor the fork author's bound was metered — resume it through POST /api/runs/%s/resume (answers ride its body) or the studio's answer, which admit it", ErrResumeNotAdmitted, run.ID, string(run.Trust), run.ID)
	}
	if *admitted != want {
		return fmt.Errorf("%w: run %s carries trust %q and author subject %q, the admission judged trust %q and author subject %q — the run document moved under the gate, retry the resume", ErrResumeNotAdmitted, run.ID, string(want.Trust), want.ForkAuthorSubject, string(admitted.Trust), admitted.ForkAuthorSubject)
	}
	return nil
}
