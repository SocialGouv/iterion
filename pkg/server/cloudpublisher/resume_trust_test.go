package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The neutering has to hold on attempt 2..N, not just on the launch. A resume
// rebuilds the credential bundle from the PRIOR RUN DOCUMENT — and it is
// reached UNATTENDED, by the retry sweeper, after a provider usage window.
// The credential that matters is not the LLM key: the runner writes the
// resolved forge_token into the clone as a git credential store, on a tree
// the pull-request author wrote.
//
// The fixture is the structural twin of TestSubmitResumeReusesWebhookRepoAndBotSecretBinding:
// same bot-secret binding, same shape — so the ONLY difference is the trust
// on the launch, and the forbidden alternative (the token resolving, as it
// does there) is what this test forbids.
func TestSubmitResume_CarriesTrustAndWithholdsSecretsOnTheSecondAttempt(t *testing.T) {
	run := func(t *testing.T, trust store.RunTrust) (*queue.RunMessage, secrets.RunBundle, *secrets.MemoryRunSecretsStore, secrets.Sealer) {
		t.Helper()
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
		if err != nil {
			t.Fatalf("sealer: %v", err)
		}
		genericStore := secrets.NewMemoryGenericSecretStore()
		secretID := secrets.NewGenericSecretID()
		sealed, err := secrets.SealGenericSecret(sealer, secretID, []byte("bot-bound-token"))
		if err != nil {
			t.Fatalf("SealGenericSecret: %v", err)
		}
		if err := genericStore.Create(context.Background(), secrets.GenericSecret{
			ID: secretID, ScopeTeamID: "team", Name: "org_forge_token",
			SealedSecret: sealed, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("generic Create: %v", err)
		}
		bindingStore := secrets.NewMemoryBotSecretBindingStore()
		if err := bindingStore.Create(context.Background(), secrets.BotSecretBinding{
			ID: "binding-1", TenantID: "team", BotID: "review-pr", SecretID: secretID,
			SecretNameForWorkflow: "forge_token", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("binding Create: %v", err)
		}
		runSecrets := secrets.NewMemoryRunSecretsStore()
		var published []*queue.RunMessage
		p := &Publisher{
			store: st, genericSecrets: genericStore, botBindings: bindingStore,
			runSecrets: runSecrets, sealer: sealer,
			publishRun: func(_ context.Context, msg *queue.RunMessage) error {
				published = append(published, msg)
				return nil
			},
		}
		ctx := store.WithIdentity(context.Background(), "team", "webhook:user")
		// Optional, so the fork arm withholds rather than refusing — the
		// required case has its own test.
		wf := &ir.Workflow{Name: "review", Secrets: map[string]*ir.Secret{"forge_token": {Optional: true}}}
		spec := runview.LaunchSpec{
			FilePath: "review.bot",
			Source:   "workflow review:\n  start -> done\n",
			RepoURL:  "https://github.com/acme/app.git",
			RepoRef:  "refs/pull/7/head",
			BotID:    "review-pr",
			Trust:    trust,
		}
		if _, err := p.SubmitLaunch(ctx, "run-resume", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitLaunch: %v", err)
		}
		if err := st.UpdateRunStatus(ctx, "run-resume", store.RunStatusFailedResumable, "needs retry"); err != nil {
			t.Fatalf("UpdateRunStatus: %v", err)
		}
		if err := p.SubmitResume(context.Background(), runview.ResumeSpec{
			RunID: "run-resume", Source: spec.Source, Answers: map[string]any{"ok": true},
		}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("SubmitResume: %v", err)
		}
		if len(published) != 2 {
			t.Fatalf("published %d messages, want 2 (launch + resume)", len(published))
		}
		resume := published[1]
		var bundle secrets.RunBundle
		if resume.SecretsRef != "" {
			rec, err := runSecrets.Get(ctx, resume.SecretsRef)
			if err != nil {
				t.Fatalf("RunSecrets.Get: %v", err)
			}
			if bundle, err = secrets.OpenRunBundle(sealer, "run-resume", rec.SealedBundle); err != nil {
				t.Fatalf("OpenRunBundle: %v", err)
			}
		}
		return resume, bundle, runSecrets, sealer
	}

	// The witness: with the same fixture and a trusted launch, the resume
	// DOES re-resolve the bot-bound token. Without this arm, an empty bundle
	// below would prove nothing about trust.
	t.Run("a trusted resume re-resolves the bot-bound token", func(t *testing.T) {
		resume, bundle, _, _ := run(t, store.RunTrustDefault)
		if !resume.Trust.Trusted() {
			t.Fatalf("resume.Trust = %q, want the trusted default", resume.Trust)
		}
		if got := bundle.GenericSecrets["forge_token"]; got != "bot-bound-token" {
			t.Fatalf("trusted resume forge_token = %q, want bot-bound-token — the fixture must be able to resolve it, or the fork arm proves nothing", got)
		}
	})

	t.Run("a fork-trust resume carries the marker and gets no token", func(t *testing.T) {
		resume, bundle, _, _ := run(t, store.RunTrustFork)
		if resume.Trust != store.RunTrustFork {
			t.Fatalf("resume.Trust = %q, want %q — the resume message is rebuilt from the prior document, and a resume that loses the marker resolves credentials as a trusted run", resume.Trust, store.RunTrustFork)
		}
		if got, ok := bundle.GenericSecrets["forge_token"]; ok {
			t.Fatalf("resume bundle carried forge_token = %q onto an untrusted workspace — the runner would write it into the fork author's clone", got)
		}
	})
}
