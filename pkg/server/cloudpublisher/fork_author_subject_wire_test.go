package cloudpublisher

import (
	"context"
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
