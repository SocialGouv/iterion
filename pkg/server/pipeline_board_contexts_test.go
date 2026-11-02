package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The board "launch now" lane must launch AS the team the board was
// selected from: the run document's tenant, the credential resolution AND
// the operator-bound context documents (ADR-147) all key on the store
// tenant stamp of the launch ctx. The lane used to launch on a bare
// context.Background() — no stamp — so a context binding on the bot was
// skipped with neither a delivery nor a degraded flag: the exact silent
// loss the kind exists to fix.
const tierContextBot = `attachments:
  handbook: file

schema probe_out:
  ok: string

tool noop:
  command: ` + "`printf '{\"ok\":\"TEAMFORK\"}'`" + `
  output: probe_out

workflow tier_probe:
  worktree: none
  entry: noop
  noop -> done
`

// handoffPublisher plays the cloud publisher honestly for this witness:
// the run row exists, then the launch's promote chain runs
// (PromoteForHandoff), exactly as cloudpublisher.SubmitLaunch does — so a
// composed bound-context promote either delivers into the store here or
// the test says it never ran.
type handoffPublisher struct {
	tierPublisher
	t          *testing.T
	rs         store.RunStore
	tenantSeen string
	ownerSeen  string
	runID      string
}

func (p *handoffPublisher) SubmitLaunch(ctx context.Context, runID string, spec runview.LaunchSpec, wf *ir.Workflow, cs *runview.CompiledSource) (int, error) {
	p.tenantSeen, _ = store.TenantFromContext(ctx)
	p.ownerSeen, _ = store.OwnerFromContext(ctx)
	p.runID = runID
	if _, err := p.rs.CreateRun(ctx, runID, wf.Name, nil); err != nil {
		return 0, err
	}
	if err := runview.PromoteForHandoff(ctx, runID, spec.AttachmentPromote); err != nil {
		return 0, err
	}
	return p.tierPublisher.SubmitLaunch(ctx, runID, spec, wf, cs)
}

func newContextLaneServer(t *testing.T, pub *handoffPublisher, bindings *secrets.MemoryBotSecretBindingStore, checkoutFiles map[string]string) (*Server, *store.FilesystemRunStore) {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(tierBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "probe",
		Files: map[string]string{botsource.MainBotFile: tierContextBot},
	}); err != nil {
		t.Fatalf("seed the team's fork: %v", err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub.rs = rs
	pub.t = t
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub),
		runview.WithBotContextResolver(&runview.BotContextResolver{
			Bindings: bindings,
			Fetch: func(_ context.Context, b secrets.BotSecretBinding) (string, error) {
				dir := t.TempDir()
				for rel, content := range checkoutFiles {
					p := filepath.Join(dir, filepath.FromSlash(rel))
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return dir, nil
			},
		}))
	return s, rs
}

func TestLaunchTicketNow_BoundContextsRideTheLaunch(t *testing.T) {
	bindings := secrets.NewMemoryBotSecretBindingStore()
	tenantCtx := store.WithTenant(context.Background(), "t1")
	if err := bindings.Create(tenantCtx, secrets.BotSecretBinding{
		ID: "b1", TenantID: "t1", BotID: "probe",
		Kind:                  secrets.BindingKindContext,
		SecretNameForWorkflow: "handbook",
		Context:               &secrets.ContextSource{GitURL: "https://example.invalid/o/c.git", Ref: "v1.0.0", Path: "ctx/handbook.md"},
	}); err != nil {
		t.Fatal(err)
	}
	pub := &handoffPublisher{}
	s, rs := newContextLaneServer(t, pub, bindings, map[string]string{"ctx/handbook.md": "# the pricing handbook\n"})
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	card, err := board.Create(native.Issue{Title: "launch me", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatal(err)
	}

	// The launch comes from an operator's click: the request ctx carries
	// their identity. The stamp the lane adds must carry the TEAM (the
	// tenant gates every team-keyed materialisation) but a MACHINE actor —
	// the run is the board lane's work, and stamping the human would make
	// their personal credentials spendable by a run they did not fund
	// (SpendableBy measures the owner; every other card lane stamps its own
	// machine actor: board-dispatcher, trigger-spine, cloud-scheduler).
	clickCtx := auth.WithIdentity(boundedCtx(t), auth.Identity{TeamID: "t1", UserID: "operator-alice", Kind: auth.KindUser})
	if _, err := s.launchTicketNow(clickCtx, "t1", s.runs, board, card); err != nil {
		t.Fatalf("launchTicketNow = %v, want nil", err)
	}
	// The mechanism: the launch ctx carries the store tenant stamp of the
	// team the board was selected from.
	if pub.tenantSeen != "t1" {
		t.Errorf("the launch ctx carries tenant %q, want t1 — an unstamped launch silently skips every team-keyed materialisation", pub.tenantSeen)
	}
	if pub.ownerSeen != pipelineBoardLaunchActor {
		t.Errorf("the launch ctx carries owner %q, want the lane's machine actor %q — the clicking operator's personal credentials must not become spendable by the run", pub.ownerSeen, pipelineBoardLaunchActor)
	}
	// The behavior: the bound context is an attachment of the run.
	held, err := rs.ListAttachments(context.Background(), pub.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Name != "handbook" {
		t.Fatalf("attachments = %+v, want the bound context delivered as handbook", held)
	}
	rc, _, err := rs.OpenAttachment(context.Background(), pub.runID, "handbook")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body := make([]byte, 64)
	n, _ := rc.Read(body)
	if string(body[:n]) != "# the pricing handbook\n" {
		t.Errorf("the delivered document reads %q", body[:n])
	}
}
