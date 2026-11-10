package cloudpublisher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §6: a fork child replays its parent's launch-frozen credential
// frame. The child keeps the team_gateway stamp naming the pinned key
// (runview.Fork), and its first resume — the only way a fork runs — must
// walk under the same pin: without it the walk seals the team's other
// gateway key under a stamp that names the pinned one. Launch pinned, fork
// through runview.Service.Fork on the same store, resume the CHILD.
func TestFork_TheChildsResumeSealsThePinnedGatewayKey(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	dir := t.TempDir()
	rs, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	p.store = rs
	var published []*queue.RunMessage
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error { published = append(published, m); return nil }
	seedOtherGatewayKey(t, p)
	pinned := seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "")
	ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", pinActor), gwRepo)
	const src = "workflow wf:\n  start -> done\n"
	wf := &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes}
	if _, err := p.SubmitLaunch(ctx, "run-parent", runview.LaunchSpec{
		FilePath: "wf.bot", Source: src, BotID: gwBot, ProjectPath: gwRepo,
		KeyOverrides: gatewayPin(pinned.ID),
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	parent, err := rs.LoadRun(ctx, "run-parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.ContentBoundary == nil || parent.ContentBoundary.KeyID != pinned.ID {
		t.Fatalf("fixture: parent boundary = %+v, want team_gateway naming the pinned key", parent.ContentBoundary)
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
	published = nil
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: res.NewRunID, FilePath: "wf.bot", Source: src}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume of the fork child: %v (log:\n%s)", err, buf.String())
	}
	if len(published) != 1 {
		t.Fatalf("published %d messages, want the child's resume", len(published))
	}
	m := published[0]
	if m.ContentBoundary == nil || m.ContentBoundary.KeyID != pinned.ID {
		t.Fatalf("child resume boundary = %+v, want the frozen stamp naming the pinned key", m.ContentBoundary)
	}
	rec, err := p.runSecrets.Get(ctx, m.SecretsRef)
	if err != nil {
		t.Fatal(err)
	}
	dek, err := secrets.NewAESGCMSealer(m.BundleDEK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := secrets.OpenRunBundle(dek, "team1", "", res.NewRunID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.APIKeys[secrets.ProviderOpenAICompatible]; got != "sk-gw-pinned" {
		t.Fatalf("child resume sealed gateway key %q, want the pinned key — the fork dropped the pin its stamp names (log:\n%s)", got, buf.String())
	}
}
