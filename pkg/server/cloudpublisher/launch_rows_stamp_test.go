package cloudpublisher

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The queued document carries the launch's two row lists: the folded list
// the run executes with and the raw launch rows (the runner reads those for
// the subbot children of the run's tree). A fork carries both, and the
// fork's resume — the only way a fork runs — replays the folded list the
// parent executed with.
func TestSubmitLaunchStampsTheRawLaunchRowsAndAForkReplaysTheFold(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	dir := t.TempDir()
	rs, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	p.store = rs
	var published []*queue.RunMessage
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error { published = append(published, m); return nil }
	pinned := seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "")
	ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", pinActor), gwRepo)
	wf := &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes}
	folded := []runview.ModelOverrideEntry{{Selector: "implement", Effort: "high"}}
	raw := []runview.ModelOverrideEntry{{Selector: "implement", Effort: "low"}}
	if _, err := p.SubmitLaunch(ctx, "run-parent", runview.LaunchSpec{
		FilePath: "wf.bot", Source: forkSrc, BotID: gwBot, ProjectPath: gwRepo,
		KeyOverrides:         gatewayPin(pinned.ID),
		ModelOverrides:       folded,
		LaunchModelOverrides: raw,
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	wantFolded := []store.RunModelOverride{{Selector: "implement", Effort: "high"}}
	wantRaw := []store.RunModelOverride{{Selector: "implement", Effort: "low"}}
	parent, err := rs.LoadRun(ctx, "run-parent")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parent.ModelOverrides, wantFolded) || !reflect.DeepEqual(parent.LaunchModelOverrides, wantRaw) {
		t.Fatalf("queued document rows = %+v, launch rows = %+v — want the fold %+v and the raw rows %+v", parent.ModelOverrides, parent.LaunchModelOverrides, wantFolded, wantRaw)
	}

	parent.Status = store.RunStatusCancelled
	parent.Checkpoint = &store.Checkpoint{NodeID: "implement", ArtifactsKnown: true}
	if err := rs.SaveRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: "run-parent", NodeID: "implement", Backend: "claw", FinishReason: "tool_use",
		MessagesRef: "implement/0/0.messages.json",
		Messages:    json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService(dir, runview.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Fork(ctx, runview.ForkSpec{RunID: "run-parent", NodeID: "implement", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := rs.LoadRun(ctx, res.NewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(child.ModelOverrides, wantFolded) || !reflect.DeepEqual(child.LaunchModelOverrides, wantRaw) {
		t.Fatalf("fork rows = %+v, launch rows = %+v — want the parent's fold %+v and raw rows %+v", child.ModelOverrides, child.LaunchModelOverrides, wantFolded, wantRaw)
	}
	published = nil
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: res.NewRunID, FilePath: "wf.bot", Source: forkSrc}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume of the fork: %v (log:\n%s)", err, buf.String())
	}
	if len(published) != 1 {
		t.Fatalf("published %d messages, want the fork's resume", len(published))
	}
	if want := []queue.ModelOverride{{Selector: "implement", Effort: "high"}}; !reflect.DeepEqual(published[0].ModelOverrides, want) {
		t.Fatalf("fork resume message rows = %+v, want the parent's fold %+v", published[0].ModelOverrides, want)
	}
}
