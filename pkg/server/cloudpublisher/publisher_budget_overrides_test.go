package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The operator's launch-time budget ask must survive a resume: cloud
// resumes are often unattended (usage-window auto-retries), so nothing
// can re-state the override, and a dropped one silently reverts the run
// to the workflow's own cap. Mesure : un run posé avec max_duration 8h,
// parqué par le cap puis repris, est mort à 14407s/14400s pendant que
// son doc affichait 8h. Falsified both ways: the ask rides the launch
// wire AND lands on the run doc; an ask-less launch stays byte-identical
// (nil on both carriers); a resume replays the doc's ask onto its wire.

func TestSubmitLaunchPersistsTheBudgetAsk(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxDuration: "4h"}}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		Budget:   &ir.BudgetOverrides{MaxDuration: "8h", MaxCostUSD: 120},
	}
	if _, err := p.SubmitLaunch(ctx, "run-bo-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if published.Budget == nil || published.Budget.MaxDuration != "8h" || published.Budget.MaxCostUSD != 120 {
		t.Fatalf("launch wire budget = %+v, want the operator's ask verbatim", published.Budget)
	}
	r, err := st.LoadRun(ctx, "run-bo-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.BudgetOverrides == nil || r.BudgetOverrides.MaxDuration != "8h" || r.BudgetOverrides.MaxCostUSD != 120 {
		t.Fatalf("run doc budget ask = %+v, want the raw ask persisted as the resume's replay source", r.BudgetOverrides)
	}
}

func TestSubmitLaunchWithoutBudgetAskPersistsNone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxDuration: "4h"}}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-bo-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil || published.Budget != nil {
		t.Fatalf("ask-less launch published budget %+v, want nil (older consumers stay byte-identical)", published.Budget)
	}
	if r, _ := st.LoadRun(ctx, "run-bo-2"); r != nil && r.BudgetOverrides != nil {
		t.Fatalf("ask-less launch persisted %+v, want none", r.BudgetOverrides)
	}
}

// #652 part 2: `iterion remote runs resume --max-duration 4h` (and its
// peer overrides) must beat the launch ask persisted on the run doc.
// Otherwise the documented "raise the cap + resume" recovery is inert
// on cloud runs and the resumed run dies at exactly the same wall.
func TestSubmitResumeThisResumeBudgetBeatsDocReplay(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	const runID = "run-cli-resume-raise-cap"
	if err := st.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team-a", OwnerID: "u1",
		Status:          store.RunStatusFailedResumable,
		BudgetOverrides: &store.RunBudgetOverrides{MaxDuration: "2h30m"}, // the launch ask that killed it
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxDuration: "4h"}}
	// The resume ask raises to 4h — the recovery the operator would
	// have typed at the remote CLI.
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "wf.bot",
		Source: "workflow wf:\n  entry: done\n",
		Budget: &ir.BudgetOverrides{MaxDuration: "4h"},
	}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil || published.Budget == nil {
		t.Fatal("nothing published or budget dropped from the wire")
	}
	if published.Budget.MaxDuration != "4h" {
		t.Fatalf("resume wire duration = %q, want 4h — the this-resume override was silently ignored and the resumed run will die at 2h30m again (#652 part 2)", published.Budget.MaxDuration)
	}
}

func TestSubmitResumeReplaysTheLaunchBudgetAsk(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	const runID = "run-bo-resume"
	if err := st.SaveRun(ctx, &store.Run{
		ID:              runID,
		TenantID:        "team-a",
		OwnerID:         "u1",
		Status:          store.RunStatusFailedResumable,
		BudgetOverrides: &store.RunBudgetOverrides{MaxDuration: "8h", MaxCostUSD: 120},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	// The workflow's own budget is the 4h that killed the lived run: the
	// replay must beat it, not inherit it.
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxDuration: "4h"}}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if published.Budget == nil || published.Budget.MaxDuration != "8h" || published.Budget.MaxCostUSD != 120 {
		t.Fatalf("resume wire budget = %+v, want the launch ask replayed from the run doc (a nil here is the measured death at 14407s/14400s under a doc showing 8h)", published.Budget)
	}
}
