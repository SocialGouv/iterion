package runview

import (
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// GateAdmission is what the launch gate judged and metered for one entry into
// the engine — a launch or a resume: the trust it admitted, and on an
// untrusted subject the fork author's metering subject it charged beside the
// org's budget (ADR-162 decision 5). A surface that crossed the gate stamps it
// on LaunchSpec.Admitted / ResumeSpec.Admitted; a surface that crossed none
// leaves it nil.
type GateAdmission struct {
	Trust             store.RunTrust
	ForkAuthorSubject string
}

// admissionFor is the admission an entry under trust, metered on subject, must
// carry. A trusted entry names no author subject whatever it was handed — the
// gate meters none on a trusted subject.
func admissionFor(trust store.RunTrust, subject string) GateAdmission {
	a := GateAdmission{Trust: trust}
	if !trust.Trusted() {
		a.ForkAuthorSubject = subject
	}
	return a
}

// ResumeAdmissionOf is the admission a resume of run must carry: the trust
// the document records and, on an untrusted run, the author subject its
// launch metered.
func ResumeAdmissionOf(run *store.Run) GateAdmission {
	return admissionFor(run.Trust, run.ForkAuthorSubject)
}

// LaunchAdmissionOf is the admission a launch must carry: the trust and the
// author subject Service.Launch publishes it under, read AFTER
// inheritSourceTrust lowered them to those of the runs it is launched from.
func LaunchAdmissionOf(spec LaunchSpec) GateAdmission {
	return admissionFor(spec.Trust, spec.ForkAuthorSubject)
}

// ErrResumeNotAdmitted reports a resume whose admission does not match the
// run it would re-enter: an untrusted run resumed by a surface that crossed
// no admission gate (answer-human, an async answer's auto-resume, an
// assistant mission), or an admission judged on a trust or an author subject
// the run document does not carry. Nothing was claimed or published.
var ErrResumeNotAdmitted = errors.New("runview: resume not admitted for this run's trust")

// ErrLaunchNotAdmitted reports a launch whose admission does not match the
// trust it would be published under: an untrusted launch no gate judged, or
// one the gate judged on another trust or author subject than the runs it is
// launched from hand it. Nothing was compiled, persisted or published.
var ErrLaunchNotAdmitted = errors.New("runview: launch not admitted for its trust")

// CheckResumeAdmitted is the one comparison every resume crosses — the run
// service before it compiles or spawns anything, and the cloud publisher
// against the document it republishes from — so an untrusted run re-enters
// the engine only on an admission that metered its own bound, whichever
// surface asked (ADR-162 decision 5: "whichever surface enters"). A trusted
// run with no admission keeps the run-by-id ladder's unmetered resume
// unchanged; an admission, when present, must name exactly the run's own.
func CheckResumeAdmitted(run *store.Run, admitted *GateAdmission) error {
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

// CheckLaunchAdmitted is CheckResumeAdmitted's launch twin, the comparison
// Service.Launch crosses once the launch's trust is settled and before
// anything is compiled or published: an untrusted launch is published only on
// an admission that judged THAT trust and charged THAT author subject. The
// trust a launch runs under is not always the one its request states — a
// launch naming a fork run as its parent_run_id or delegation source runs
// under the fork's trust and meters its author (inheritSourceTrust) — so a
// surface that judged the request alone would admit an untrusted run as
// trusted work, past every arm of decision 5. A trusted launch with no
// admission is unchanged; an admission, when present, must name exactly the
// launch's own.
func CheckLaunchAdmitted(spec LaunchSpec) error {
	want := LaunchAdmissionOf(spec)
	if spec.Admitted == nil {
		if spec.Trust.Trusted() {
			return nil
		}
		return fmt.Errorf("%w: the launch runs under trust %q (author subject %q) and crossed no admission gate, so neither the org budget nor the fork author's bound was metered", ErrLaunchNotAdmitted, string(want.Trust), want.ForkAuthorSubject)
	}
	if *spec.Admitted != want {
		return fmt.Errorf("%w: the launch runs under trust %q and author subject %q, the admission judged trust %q and author subject %q — a run it is launched from moved under the gate, retry the launch", ErrLaunchNotAdmitted, string(want.Trust), want.ForkAuthorSubject, string(spec.Admitted.Trust), spec.Admitted.ForkAuthorSubject)
	}
	return nil
}
