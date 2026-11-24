package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_AnUntrustedRunResumesOnlyOnItsOwnAdmission: the run service is
// the one choke point every resume surface crosses — the gated ones (the HTTP
// resume, the WS answer, the retry sweeper) and the ones that cross no gate
// (answer-human, an async answer's auto-resume, an assistant mission). A fork
// run must re-enter the engine only on an admission that names its own trust
// and the fork author subject its launch metered (ADR-162 decision 5); a
// trusted run keeps the run-by-id ladder's resume unchanged.
//
// The publisher is a double, so SubmitResume's own comparison never runs
// here: every refusal row is this layer's alone.
//
// Mutations: mute CheckResumeAdmitted in Service.Resume → every "refused" row
// resumes (red on its own assertion); mute it in PreflightResume → the
// preflight rows red; make the nil-admission arm admit any trust → the
// "no admission" rows red.
func TestResume_AnUntrustedRunResumesOnlyOnItsOwnAdmission(t *testing.T) {
	const author = "fork-author:t1:github:4242"
	setup := func(t *testing.T, trust store.RunTrust, forkAuthor string) (*Service, *trustCapturingPublisher, string) {
		t.Helper()
		dir := t.TempDir()
		botPath := filepath.Join(dir, "operator_resume.bot")
		if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pub := &trustCapturingPublisher{}
		svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
		if err != nil {
			t.Fatal(err)
		}
		_, hash, err := CompileWorkflowWithHash(botPath, "", AnalysisTrust)
		if err != nil {
			t.Fatal(err)
		}
		seedPausedOperatorRun(t, svc, "run-admission", hash)
		r, err := svc.store.LoadRun(context.Background(), "run-admission")
		if err != nil {
			t.Fatal(err)
		}
		r.Trust = trust
		r.ForkAuthorSubject = forkAuthor
		if err := svc.store.SaveRun(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		return svc, pub, botPath
	}
	refused := func(t *testing.T, svc *Service, pub *trustCapturingPublisher, err error) {
		t.Helper()
		if !errors.Is(err, ErrResumeNotAdmitted) {
			t.Fatalf("Resume = %v, want ErrResumeNotAdmitted", err)
		}
		if pub.resumed {
			t.Fatal("the refused resume reached the publisher — the run re-entered the engine past the bound it launched under")
		}
		if RunMayHaveStarted(err) {
			t.Fatalf("the refusal reads as a run that may have started (%v) — every metered surface would keep the unit it must hand back", err)
		}
		r, lerr := svc.store.LoadRun(context.Background(), "run-admission")
		if lerr != nil {
			t.Fatal(lerr)
		}
		if r.Status != store.RunStatusPausedOperator {
			t.Fatalf("status = %s after a refused resume, want it untouched (%s)", r.Status, store.RunStatusPausedOperator)
		}
	}

	for _, row := range []struct {
		name     string
		admitted *GateAdmission
	}{
		{"a fork run with no admission is refused", nil},
		{"a fork run on a trusted admission is refused", &GateAdmission{}},
		{"a fork run on another author's admission is refused", &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: "fork-author:t1:github:9999"}},
		{"a fork run on an admission that metered no author is refused", &GateAdmission{Trust: store.RunTrustFork}},
	} {
		t.Run(row.name, func(t *testing.T) {
			svc, pub, botPath := setup(t, store.RunTrustFork, author)
			_, err := svc.Resume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath, Admitted: row.admitted})
			refused(t, svc, pub, err)
		})
	}

	t.Run("a fork run on its own admission resumes", func(t *testing.T) {
		svc, pub, botPath := setup(t, store.RunTrustFork, author)
		if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath,
			Admitted: &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: author}}); err != nil {
			t.Fatalf("Resume = %v, want the admitted fork run resumed", err)
		}
		if !pub.resumed {
			t.Fatal("the admitted resume never reached the publisher — the refusal rows above would prove nothing")
		}
	})

	t.Run("a trusted run with no admission resumes unchanged", func(t *testing.T) {
		svc, pub, botPath := setup(t, store.RunTrustDefault, "")
		if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath}); err != nil {
			t.Fatalf("Resume = %v, want the ladder's unmetered resume of a trusted run untouched", err)
		}
		if !pub.resumed {
			t.Fatal("the trusted resume never reached the publisher")
		}
	})

	t.Run("a trusted run on a fork admission is refused", func(t *testing.T) {
		svc, pub, botPath := setup(t, store.RunTrustDefault, "")
		_, err := svc.Resume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath,
			Admitted: &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: author}})
		refused(t, svc, pub, err)
	})

	t.Run("the preflight refuses what Resume refuses", func(t *testing.T) {
		svc, _, botPath := setup(t, store.RunTrustFork, author)
		if err := svc.PreflightResume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath}); !errors.Is(err, ErrResumeNotAdmitted) {
			t.Fatalf("PreflightResume = %v, want ErrResumeNotAdmitted — the upload a refused resume carries would be consumed first", err)
		}
		if err := svc.PreflightResume(context.Background(), ResumeSpec{RunID: "run-admission", FilePath: botPath,
			Admitted: &GateAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: author}}); err != nil {
			t.Fatalf("PreflightResume = %v on the run's own admission, want nil", err)
		}
	})
}
