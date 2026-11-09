package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-162 decision 5's carrier chain, document and wire halves: the fork
// author's metering subject that the launch's admission composed rides the
// run document (what a resume, a usage-window retry and orgusage read) and
// the RunMessage (what the runner's second charge reads where spend is
// measured). Both carriers are stamped by SubmitLaunch from the LaunchSpec;
// a resume replays them from the document, never re-derived.
//
// Mutations: drop ForkAuthorSubject at either stamping site (the run
// document, the launch wire) or from the resume replay → each reddens this
// witness on its own assertion while every gate test stays green — the
// dollars half of the bound goes dark with CI green, which is exactly the
// hole this file closes.
func TestSubmitLaunchAndResume_TheForkAuthorSubjectRidesDocumentAndWire(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published []*queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "t1", "user")
	wf := &ir.Workflow{Name: "fork_review"}
	subject := string(orgusage.ForkAuthorSubject("t1", "github", "4242"))
	spec := runview.LaunchSpec{
		FilePath: "fork_review.bot",
		Source:   "workflow fork_review:\n  entry: done\n",
		BotID:    "review-pr",
		// What the lane's admission proved and composed (decision 5).
		Trust:             store.RunTrustFork,
		RepoSHAExpected:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ForkAuthorSubject: subject,
	}
	if _, err := p.SubmitLaunch(ctx, "run-fork", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d messages, want 1", len(published))
	}
	// The document: what orgusage reads and what a resume replays from.
	got, err := st.LoadRun(ctx, "run-fork")
	if err != nil {
		t.Fatal(err)
	}
	if got.ForkAuthorSubject != subject {
		t.Errorf("run document ForkAuthorSubject = %q, want %q — orgusage records zero fork-author spend and the $20 cap of decision 5 never trips", got.ForkAuthorSubject, subject)
	}
	if got.Trust != store.RunTrustFork {
		t.Errorf("run document Trust = %q, want %q", got.Trust, store.RunTrustFork)
	}
	if got.RepoSHAExpected != spec.RepoSHAExpected {
		t.Errorf("run document RepoSHAExpected = %q, want the admitted commit %q", got.RepoSHAExpected, spec.RepoSHAExpected)
	}
	// The wire: the runner's recordOrgSpend reads the subject here, where
	// spend is measured — not from the document a failed run may never load.
	launch := published[0]
	if launch.ForkAuthorSubject != subject {
		t.Errorf("launch RunMessage ForkAuthorSubject = %q, want %q — the runner's second charge would meter nobody", launch.ForkAuthorSubject, subject)
	}
	if launch.Trust != store.RunTrustFork {
		t.Errorf("launch RunMessage Trust = %q, want %q", launch.Trust, store.RunTrustFork)
	}
	if launch.RepoSHAExpected != spec.RepoSHAExpected {
		t.Errorf("launch RunMessage RepoSHAExpected = %q, want the admitted commit", launch.RepoSHAExpected)
	}

	// The resume replays the subject from the document (the usage-window
	// retry, the operator's resume): the same string by construction, never
	// re-derived identity halves.
	if err := st.UpdateRunStatus(ctx, "run-fork", store.RunStatusFailedResumable, "needs retry"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-fork", Source: spec.Source,
		// What a gated surface admitted on this document (decision 5).
		Admitted: &runview.ResumeAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: subject},
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d messages, want 2 (launch + resume)", len(published))
	}
	resume := published[1]
	if resume.ForkAuthorSubject != subject {
		t.Errorf("resume RunMessage ForkAuthorSubject = %q, want %q — a retried fork run must re-enter the author's bound, not run unmetered on its second attempt", resume.ForkAuthorSubject, subject)
	}
	if resume.Trust != store.RunTrustFork {
		t.Errorf("resume RunMessage Trust = %q, want %q", resume.Trust, store.RunTrustFork)
	}
}

// TestSubmitResume_AnUntrustedPriorRepublishesOnlyOnItsOwnAdmission: the
// message SubmitResume publishes replays the PRIOR document's Trust and
// ForkAuthorSubject, so the admission is judged against that document — not
// against the run service's earlier read, which a write between the two can
// have moved. A resume no gate judged (answer-human, an async answer's
// auto-resume, an assistant mission), or one judged on another trust or
// another author, publishes nothing and claims nothing.
//
// Mutation: mute runview.CheckResumeAdmitted in SubmitResume → every refused
// row publishes a resume of the fork run (red on its own assertion).
func TestSubmitResume_AnUntrustedPriorRepublishesOnlyOnItsOwnAdmission(t *testing.T) {
	subject := string(orgusage.ForkAuthorSubject("t1", "github", "4242"))
	setup := func(t *testing.T) (*Publisher, *store.FilesystemRunStore, *[]*queue.RunMessage, context.Context) {
		t.Helper()
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		published := &[]*queue.RunMessage{}
		p := &Publisher{
			store: st,
			publishRun: func(_ context.Context, msg *queue.RunMessage) error {
				*published = append(*published, msg)
				return nil
			},
		}
		ctx := store.WithIdentity(context.Background(), "t1", "user")
		if _, err := p.SubmitLaunch(ctx, "run-fork", runview.LaunchSpec{
			FilePath: "fork_review.bot", Source: "workflow fork_review:\n  entry: done\n", BotID: "review-pr",
			Trust: store.RunTrustFork, RepoSHAExpected: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ForkAuthorSubject: subject,
		}, &ir.Workflow{Name: "fork_review"}, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitLaunch: %v", err)
		}
		if err := st.UpdateRunStatus(ctx, "run-fork", store.RunStatusFailedResumable, "needs retry"); err != nil {
			t.Fatalf("UpdateRunStatus: %v", err)
		}
		return p, st, published, ctx
	}
	for _, row := range []struct {
		name     string
		admitted *runview.ResumeAdmission
	}{
		{"no admission", nil},
		{"a trusted admission", &runview.ResumeAdmission{}},
		{"another author's admission", &runview.ResumeAdmission{Trust: store.RunTrustFork, ForkAuthorSubject: string(orgusage.ForkAuthorSubject("t1", "github", "9999"))}},
	} {
		t.Run(row.name+" is refused", func(t *testing.T) {
			p, st, published, ctx := setup(t)
			err := p.SubmitResume(ctx, runview.ResumeSpec{
				RunID: "run-fork", Source: "workflow fork_review:\n  entry: done\n", Admitted: row.admitted,
			}, &ir.Workflow{Name: "fork_review"}, &runview.CompiledSource{Hash: "hash"})
			if !errors.Is(err, runview.ErrResumeNotAdmitted) {
				t.Fatalf("SubmitResume = %v, want runview.ErrResumeNotAdmitted", err)
			}
			if n := len(*published); n != 1 {
				t.Fatalf("published %d messages, want only the launch — the refused resume re-entered a fork run past its bound", n)
			}
			got, lerr := st.LoadRun(ctx, "run-fork")
			if lerr != nil {
				t.Fatal(lerr)
			}
			if got.Status != store.RunStatusFailedResumable {
				t.Fatalf("status = %s after a refused resume, want it unclaimed (%s)", got.Status, store.RunStatusFailedResumable)
			}
		})
	}
}
