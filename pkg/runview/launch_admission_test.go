package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// TestLaunch_AnUntrustedLaunchIsPublishedOnlyOnItsOwnAdmission: Service.Launch
// is the one choke point every launch surface crosses, so it holds an
// untrusted launch to the admission its surface's gate stamped
// (CheckLaunchAdmitted) — AFTER the launch's trust is settled, because a
// launch naming a fork run as its parent runs under the fork's trust and
// meters its author whatever its request said. The shape the review found: a
// gate that judged the request alone stamps a TRUSTED admission, and the run
// service published the run untrusted past every arm of decision 5. Each
// guard has its own rows: no admission at all (the parent row and the
// launch-of-its-own row), an admission naming another trust or author (the
// two mismatch rows).
//
// Mutations: drop the CheckLaunchAdmitted call from Launch → every refused row
// publishes; its nil arm returning nil → the two no-admission rows publish;
// its comparison disabled → the two mismatch rows publish.
func TestLaunch_AnUntrustedLaunchIsPublishedOnlyOnItsOwnAdmission(t *testing.T) {
	const author = "forkauthor|t1|github|4242"
	own := &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: author}
	for _, row := range []struct {
		name     string
		parent   bool
		trust    store.RunTrust
		subject  string
		admitted *GateAdmission
		refused  bool
	}{
		{name: "a fork parent, no admission", parent: true, refused: true},
		{name: "a fork parent, judged as trusted work", parent: true, admitted: &GateAdmission{}, refused: true},
		{name: "a fork parent, judged on another author", parent: true,
			admitted: &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: "forkauthor|t1|github|1"}, refused: true},
		{name: "a fork parent, judged on its own trust and author", parent: true, admitted: own},
		{name: "an untrusted launch of its own, no admission", trust: store.RunTrustFork, subject: author, refused: true},
		{name: "an untrusted launch of its own, admitted", trust: store.RunTrustFork, subject: author, admitted: own},
		{name: "a trusted launch with no admission (the control)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			botPath := filepath.Join(dir, "source_trust.bot")
			if err := os.WriteFile(botPath, []byte(sourceTrustBot), 0o644); err != nil {
				t.Fatal(err)
			}
			pub := &subjectCapturingPublisher{}
			svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
			if err != nil {
				t.Fatal(err)
			}
			spec := LaunchSpec{FilePath: botPath, Trust: row.trust, ForkAuthorSubject: row.subject, Admitted: row.admitted}
			if row.parent {
				r := storetest.SeedParentRun(t, svc.store, "fork-parent")
				r.Trust, r.ForkAuthorSubject = store.RunTrustFork, author
				if err := svc.store.SaveRun(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				spec.ParentRunID = "fork-parent"
			}
			_, err = svc.Launch(context.Background(), spec)
			if row.refused {
				if !errors.Is(err, ErrLaunchNotAdmitted) {
					t.Fatalf("Launch = %v, want %v", err, ErrLaunchNotAdmitted)
				}
				if pub.launched {
					t.Fatalf("an untrusted launch was published on admission %+v", row.admitted)
				}
				return
			}
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if !pub.launched {
				t.Fatal("nothing was published — this row proves nothing")
			}
			want := LaunchAdmissionOf(spec)
			if row.parent {
				want = *own
			}
			if pub.launchTrust != want.Trust || pub.subject != want.ForkAuthorSubject {
				t.Fatalf("published trust %q, author %q; want %q, %q", pub.launchTrust, pub.subject, want.Trust, want.ForkAuthorSubject)
			}
		})
	}
}
