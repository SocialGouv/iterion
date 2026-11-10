package cloudpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// pinnedForkParent launches a gateway run pinned to its own key and parks it
// for a fork at its gateway node, returning the publisher, its store dir, the
// pinned key and the captured publications.
func pinnedForkParent(t *testing.T) (*Publisher, *bytes.Buffer, string, secrets.ApiKey, *[]*queue.RunMessage) {
	t.Helper()
	p, buf := gatewayKeyPublisher(t)
	dir := t.TempDir()
	rs, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	p.store = rs
	published := &[]*queue.RunMessage{}
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error { *published = append(*published, m); return nil }
	seedOtherGatewayKey(t, p)
	pinned := seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "")
	ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", pinActor), gwRepo)
	wf := &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes}
	if _, err := p.SubmitLaunch(ctx, "run-parent", runview.LaunchSpec{
		FilePath: "wf.bot", Source: forkSrc, BotID: gwBot, ProjectPath: gwRepo,
		KeyOverrides: gatewayPin(pinned.ID),
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	parent, err := rs.LoadRun(ctx, "run-parent")
	if err != nil {
		t.Fatal(err)
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
	return p, buf, dir, pinned, published
}

const forkSrc = "workflow wf:\n  start -> done\n"

// forkKeyPinCheck is the server's check (pkg/server wires the same reading of
// its key store): the pinned row exists in the team's store.
func forkKeyPinCheck(p *Publisher) runview.ForkKeyPinCheck {
	return func(ctx context.Context, tenantID, keyID string) (bool, error) {
		_, err := p.apiKeys.Get(store.WithTenant(ctx, tenantID), keyID)
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			return false, nil
		}
		return err == nil, err
	}
}

// X-4 of the round-2 review: a fork is the recovery path for a run that can
// no longer continue, and the credential sweep's rule holds for the frame it
// copies — a key pin whose row no longer exists is the explicit clear. A fork
// of a run whose pinned gateway key was deleted drops the pin, names it in
// the fork's notes, and the child resumes on the team's current gateway key;
// a live pin travels (the control), and so does a pin the key store cannot
// answer for (its child's resume names it if its key is gone).
func TestFork_DropsAKeyPinWhoseKeyNoLongerExists(t *testing.T) {
	for _, row := range []struct {
		name    string
		deleted bool
		unread  bool
	}{
		{"the pinned key deleted before the fork", true, false},
		{"the pinned key alive (control)", false, false},
		{"the key store cannot answer", false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			deleted := row.deleted
			p, buf, dir, pinned, published := pinnedForkParent(t)
			ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", pinActor), gwRepo)
			if deleted {
				if err := p.apiKeys.Delete(store.WithTenant(context.Background(), "team1"), pinned.ID); err != nil {
					t.Fatalf("delete pinned key: %v", err)
				}
			}
			check := forkKeyPinCheck(p)
			if row.unread {
				check = func(context.Context, string, string) (bool, error) {
					return false, errors.New("key store: connection reset")
				}
			}
			svc, err := runview.NewService(dir, runview.WithLogger(iterlog.Nop()), runview.WithForkKeyPinCheck(check))
			if err != nil {
				t.Fatal(err)
			}
			res, err := svc.Fork(ctx, runview.ForkSpec{RunID: "run-parent", NodeID: "implement", TurnIndex: -1})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			child, err := p.store.LoadRun(ctx, res.NewRunID)
			if err != nil {
				t.Fatal(err)
			}
			if got := child.KeyOverrides[string(secrets.ProviderOpenAICompatible)]; (got == pinned.ID) == deleted {
				t.Fatalf("child gateway pin = %q, want it %s", got, map[bool]string{true: "dropped: its key no longer exists", false: "carried"}[deleted])
			}
			named := slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, pinned.ID) && strings.Contains(n, "no longer exists") })
			if named != deleted {
				t.Fatalf("fork notes %v name the dropped pin = %v, want %v", res.Notes, named, deleted)
			}
			*published = nil
			if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: res.NewRunID, FilePath: "wf.bot", Source: forkSrc}, &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes}, &runview.CompiledSource{Hash: "hash"}); err != nil {
				t.Fatalf("SubmitResume of the fork child: %v (log:\n%s)", err, buf.String())
			}
			if len(*published) != 1 {
				t.Fatalf("published %d messages, want the child's resume", len(*published))
			}
		})
	}
}

// X-4, the message: a pin whose key is deleted AFTER the fork strands the
// child on its inherited pin, and the refusal says so — the child never
// launched, so "relaunch the run" would throw the fork away: it names the
// parent and a second fork, which drops the dead pin.
func TestFork_AChildsPinRefusalNamesItsParent(t *testing.T) {
	p, buf, dir, pinned, _ := pinnedForkParent(t)
	ctx := secrets.WithLaunchRepo(store.WithIdentity(context.Background(), "team1", pinActor), gwRepo)
	svc, err := runview.NewService(dir, runview.WithLogger(iterlog.Nop()), runview.WithForkKeyPinCheck(forkKeyPinCheck(p)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Fork(ctx, runview.ForkSpec{RunID: "run-parent", NodeID: "implement", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := p.apiKeys.Delete(store.WithTenant(context.Background(), "team1"), pinned.ID); err != nil {
		t.Fatalf("delete pinned key: %v", err)
	}
	err = p.SubmitResume(ctx, runview.ResumeSpec{RunID: res.NewRunID, FilePath: "wf.bot", Source: forkSrc}, &ir.Workflow{Name: "wf", Nodes: gatewayOnlyWorkflow().Nodes}, &runview.CompiledSource{Hash: "hash"})
	var refusal *GatewayPinRefusedError
	if !errors.As(err, &refusal) || refusal.Cause != GatewayPinMissing {
		t.Fatalf("SubmitResume = %v, want the pin refused as missing (log:\n%s)", err, buf.String())
	}
	if msg := err.Error(); !strings.Contains(msg, "fork of run run-parent") || !strings.Contains(msg, "fork run run-parent again") || strings.Contains(msg, "relaunch") {
		t.Fatalf("a fork child's refusal %q must name its parent and a second fork, never a relaunch that discards the fork", msg)
	}
}
