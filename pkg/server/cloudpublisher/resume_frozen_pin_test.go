// The cloud resume judges the mono_family pin its run FROZE at launch
// against the credentials it re-seals (#1221). The launch refusal #2258
// delivered covered the moment a run is created; nothing covered the moment
// it comes BACK, and a resume replays the frozen inputs — so a run whose
// family lost its credential since started and died at its first review
// node, which on the forge lane reads as a required check going dark on
// every pull request of the repo (measured 2026-09-22).
//
// The scenario is the ticket's own, played end to end on this lane: launch
// pinned `mono_family=gpt` with an OpenAI credential, remove the credential,
// resume. Mutant: the RefuseFrozenPin call removed from SubmitResume → the
// resume publishes and the run goes on to die at its first node → the
// refusal and status assertions red.

package cloudpublisher

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// monoFamilyBot declares the opt-in var the topology reads and carries one
// route per family, so the bundle seals the key of each family the tier
// holds — which is what the resume's family set is built from.
const monoFamilyBot = "vars:\n" +
	"  review_mode: string = \"mono\"\n" +
	"  mono_family: string = \"claude\"\n" +
	"\njudge reviewer_claude:\n" +
	"  model: \"anthropic/claude-opus-5\"\n" +
	"  backend: \"claw\"\n" +
	"\njudge reviewer_gpt:\n" +
	"  model: \"openai/gpt-5.5\"\n" +
	"  backend: \"claw\"\n" +
	"\nworkflow main:\n  reviewer_claude -> reviewer_gpt\n  reviewer_gpt -> done\n"

// resumePinPublisher is the sealing stack of allowlistPublisher with a real
// store and a capturing publish, so one fixture can both launch and resume.
func resumePinPublisher(t *testing.T, st store.RunStore, published **queue.RunMessage, seeds ...secrets.Provider) *Publisher {
	t.Helper()
	p, _ := allowlistPublisher(t, seeds...)
	p.store = st
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error {
		*published = m
		return nil
	}
	return p
}

func TestSubmitResume_RefusesAPinNoCredentialServesAnyMore(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team1", "owner1")
	const runID = "run-frozen-pin"

	// The launch, while an OpenAI credential serves the pinned family.
	var published *queue.RunMessage
	launcher := resumePinPublisher(t, st, &published, secrets.ProviderAnthropic, secrets.ProviderOpenAI)
	wf := compileTestSource(t, monoFamilyBot)
	cs := &runview.CompiledSource{Hash: "hash"}
	if _, err := launcher.SubmitLaunch(ctx, runID, runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   monoFamilyBot,
		Vars:     map[string]string{"review_mode": "mono", "mono_family": "gpt"},
	}, wf, cs); err != nil {
		t.Fatalf("SubmitLaunch with an OpenAI credential = %v, want the pin honoured", err)
	}
	launched, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after launch: %v", err)
	}
	if launched.Inputs["mono_family"] != "gpt" {
		t.Fatalf("the launch froze mono_family=%v, want gpt — the scenario needs the pin on the doc", launched.Inputs["mono_family"])
	}
	frozen := maps.Clone(launched.Inputs)
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("park the run: %v", err)
	}

	// The credential is gone. Same store, same frozen inputs, a tier that
	// now holds only the Anthropic key.
	published = nil
	resumer := resumePinPublisher(t, st, &published, secrets.ProviderAnthropic)
	err = resumer.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: monoFamilyBot}, wf, cs)
	if err == nil {
		t.Fatal("the resume of a run pinned to a family no credential serves any more was accepted — it starts and dies at its first review node, and the check goes dark")
	}
	for _, want := range []string{"refusing to resume", `"gpt"`, "claude", "launch a fresh run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	if published != nil {
		t.Error("the refused resume published a queue message — the runner would claim it")
	}

	// The run is back where it was, with its inputs byte-identical: the
	// judgement is read-only, and the rollback is the publisher's own.
	after, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after the refusal: %v", err)
	}
	if after.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("status after the refused resume = %s, want paused_waiting_human", after.Status)
	}
	// The refusal must be legible where an operator looks, not only in a
	// log line: the 7-day cap on the runs listing is what hid the 2026-09-22
	// outage, so the reason rides the run document.
	for _, want := range []string{`"gpt"`, "claude"} {
		if !strings.Contains(after.Error, want) {
			t.Errorf("the run document does not say why the resume was refused (%q missing): %q", want, after.Error)
		}
	}
	if !reflect.DeepEqual(after.Inputs, frozen) {
		t.Errorf("the refused resume rewrote the run's frozen inputs: %+v, want %+v", after.Inputs, frozen)
	}
}

// The same run resumes normally while a credential still serves its frozen
// family: the judgement refuses a dead configuration, it does not refuse
// resumes. Without this half, a mutant that refused every resume would pass
// the test above.
func TestSubmitResume_AcceptsAPinACredentialStillServes(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team1", "owner1")
	const runID = "run-served-pin"

	var published *queue.RunMessage
	p := resumePinPublisher(t, st, &published, secrets.ProviderAnthropic, secrets.ProviderOpenAI)
	wf := compileTestSource(t, monoFamilyBot)
	cs := &runview.CompiledSource{Hash: "hash"}
	if _, err := p.SubmitLaunch(ctx, runID, runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   monoFamilyBot,
		Vars:     map[string]string{"review_mode": "mono", "mono_family": "gpt"},
	}, wf, cs); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("park the run: %v", err)
	}
	published = nil
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: monoFamilyBot}, wf, cs); err != nil {
		t.Fatalf("SubmitResume with the credential still in place = %v, want the resume to proceed", err)
	}
	if published == nil {
		t.Fatal("the accepted resume published nothing")
	}
}
