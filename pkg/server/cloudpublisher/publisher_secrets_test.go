package cloudpublisher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResolveAndSealCredentials_GenericWorkflowSecrets(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	genericStore := secrets.NewMemoryGenericSecretStore()
	secretID := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, secretID, []byte("apiVersion: v1"))
	if err != nil {
		t.Fatalf("SealGenericSecret: %v", err)
	}
	if err := genericStore.Create(context.Background(), secrets.GenericSecret{
		ID:           secretID,
		ScopeTeamID:  "team",
		ScopeUserID:  "alice",
		Name:         "kubeconfig",
		SealedSecret: sealed,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	runSecrets := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{
		genericSecrets: genericStore,
		runSecrets:     runSecrets,
		sealer:         sealer,
	}
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"kubeconfig": {As: "file"},
	}}
	ctx := store.WithTenant(context.Background(), "team")
	creds, err := p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", wf, nil, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		t.Fatal("expected secrets ref")
	}
	rec, err := runSecrets.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "run-1", rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	if bundle.GenericSecrets["kubeconfig"] != "apiVersion: v1" {
		t.Fatalf("GenericSecrets not sealed into bundle: %+v", bundle.GenericSecrets)
	}
}

func TestSubmitLaunchPersistsWebhookRepoAndBotMetadata(t *testing.T) {
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
	ctx := store.WithIdentity(context.Background(), "team", "webhook:user")
	wf := &ir.Workflow{Name: "review"}
	spec := runview.LaunchSpec{
		FilePath: "review.bot",
		Source:   "workflow review:\n  start -> done\n",
		RepoURL:  "https://git.example/acme/app.git",
		RepoRef:  "refs/merge-requests/7/head",
		BotID:    "review-pr",
	}
	if _, err := p.SubmitLaunch(ctx, "run-webhook", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-webhook")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.RepoURL != spec.RepoURL || r.RepoSHA != spec.RepoRef || r.BotID != spec.BotID {
		t.Fatalf("persisted metadata = repo_url=%q repo_sha=%q bot_id=%q, want %q %q %q", r.RepoURL, r.RepoSHA, r.BotID, spec.RepoURL, spec.RepoRef, spec.BotID)
	}
	if len(published) != 1 {
		t.Fatalf("published %d messages, want 1", len(published))
	}
	if published[0].RepoURL != spec.RepoURL || published[0].RepoSHA != spec.RepoRef {
		t.Fatalf("published metadata = repo_url=%q repo_sha=%q, want %q %q", published[0].RepoURL, published[0].RepoSHA, spec.RepoURL, spec.RepoRef)
	}
}

func TestSubmitResumeReusesWebhookRepoAndBotSecretBinding(t *testing.T) {
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
		ID:           secretID,
		ScopeTeamID:  "team",
		Name:         "org_forge_token",
		SealedSecret: sealed,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("generic Create: %v", err)
	}
	bindingStore := secrets.NewMemoryBotSecretBindingStore()
	if err := bindingStore.Create(context.Background(), secrets.BotSecretBinding{
		ID:                    "binding-1",
		TenantID:              "team",
		BotID:                 "review-pr",
		SecretID:              secretID,
		SecretNameForWorkflow: "forge_token",
		CreatedAt:             time.Now().UTC(),
	}); err != nil {
		t.Fatalf("binding Create: %v", err)
	}
	runSecrets := secrets.NewMemoryRunSecretsStore()
	var published []*queue.RunMessage
	p := &Publisher{
		store:          st,
		genericSecrets: genericStore,
		botBindings:    bindingStore,
		runSecrets:     runSecrets,
		sealer:         sealer,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team", "webhook:user")
	wf := &ir.Workflow{
		Name: "review",
		Secrets: map[string]*ir.Secret{
			"forge_token": {},
		},
	}
	spec := runview.LaunchSpec{
		FilePath: "review.bot",
		Source:   "workflow review:\n  start -> done\n",
		RepoURL:  "https://git.example/acme/app.git",
		RepoRef:  "refs/merge-requests/7/head",
		BotID:    "review-pr",
	}
	if _, err := p.SubmitLaunch(ctx, "run-resume", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-resume", store.RunStatusFailedResumable, "needs retry"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if err := p.SubmitResume(context.Background(), runview.ResumeSpec{
		RunID:   "run-resume",
		Source:  spec.Source,
		Answers: map[string]any{"ok": true},
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d messages, want 2", len(published))
	}
	resume := published[1]
	if resume.Resume == nil {
		t.Fatal("resume message missing Resume spec")
	}
	if resume.RepoURL != spec.RepoURL || resume.RepoSHA != spec.RepoRef {
		t.Fatalf("resume metadata = repo_url=%q repo_sha=%q, want %q %q", resume.RepoURL, resume.RepoSHA, spec.RepoURL, spec.RepoRef)
	}
	if resume.SecretsRef == "" {
		t.Fatal("resume did not seal bot-bound secret")
	}
	rec, err := runSecrets.Get(ctx, resume.SecretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "run-resume", rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	if got := bundle.GenericSecrets["forge_token"]; got != "bot-bound-token" {
		t.Fatalf("resume generic secret forge_token = %q, want bot-bound-token (via bot binding, with no same-name team secret)", got)
	}
}

func TestResolveAndSealCredentials_RequiredSecretUnresolvedFails(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// Store has no matching secret — the team credential was deleted.
	p := &Publisher{
		genericSecrets: secrets.NewMemoryGenericSecretStore(),
		runSecrets:     secrets.NewMemoryRunSecretsStore(),
		sealer:         sealer,
	}
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"test_e2e_canary": {As: "file"}, // non-optional, no inline value
	}}
	ctx := store.WithTenant(context.Background(), "team")
	_, err = p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", wf, nil, nil)
	if err == nil {
		t.Fatal("expected launch to fail for an unresolved required secret")
	}
	if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "resolves to nothing") {
		t.Fatalf("error should explain the required-secret contract, got %q", err.Error())
	}
}

func TestResolveAndSealCredentials_OptionalSecretUnresolvedSkips(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	p := &Publisher{
		genericSecrets: secrets.NewMemoryGenericSecretStore(),
		runSecrets:     secrets.NewMemoryRunSecretsStore(),
		sealer:         sealer,
	}
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{
		"test_e2e_canary": {As: "file", Optional: true},
	}}
	ctx := store.WithTenant(context.Background(), "team")
	creds, err := p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", wf, nil, nil)
	if err != nil {
		t.Fatalf("optional unresolved secret must not fail launch: %v", err)
	}
	// No credentials of any kind → empty ref (runner falls back to env).
	if creds.secretsRef != "" {
		t.Fatalf("expected empty ref for an all-unresolved optional secret, got %q", creds.secretsRef)
	}
}

func TestSubmitLaunch_RequiredSecretUnresolved_NoRunRecord(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var published []*queue.RunMessage
	p := &Publisher{
		store:          st,
		genericSecrets: secrets.NewMemoryGenericSecretStore(),
		runSecrets:     secrets.NewMemoryRunSecretsStore(),
		sealer:         sealer,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	wf := &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{
		"test_e2e_canary": {As: "file"},
	}}
	spec := runview.LaunchSpec{
		FilePath: "canary.bot",
		Source:   "workflow canary:\n  start -> done\n",
		BotID:    "canary",
	}
	if _, err := p.SubmitLaunch(ctx, "run-canary", spec, wf, "hash"); err == nil {
		t.Fatal("expected SubmitLaunch to fail for an unresolved required secret")
	} else if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	}
	// No run record persisted, nothing published.
	if _, err := st.LoadRun(ctx, "run-canary"); err == nil {
		t.Fatal("no run record should exist after a required-secret launch failure")
	}
	if len(published) != 0 {
		t.Fatalf("no message should be published, got %d", len(published))
	}
}

// TestSubmitResume_RequiredSecretUnresolved_KeepsResumableStatus pins the
// resume mirror of the launch gate: when the required secret vanished
// between the failure and the resume, the atomic queue claim must be rolled
// back to the prior resumable status. Nothing was published, so leaving the
// run stranded in `queued` would make it impossible to resume from the studio.
func TestSubmitResume_RequiredSecretUnresolved_KeepsResumableStatus(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var published []*queue.RunMessage
	p := &Publisher{
		store:          st,
		genericSecrets: secrets.NewMemoryGenericSecretStore(),
		runSecrets:     secrets.NewMemoryRunSecretsStore(),
		sealer:         sealer,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	// A failed-resumable run on record, as a real resume would find it.
	if err := st.SaveRun(ctx, &store.Run{ID: "run-resume", TenantID: "team", OwnerID: "alice", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	wf := &ir.Workflow{Name: "canary", Secrets: map[string]*ir.Secret{
		"test_e2e_canary": {As: "file"},
	}}
	spec := runview.ResumeSpec{
		RunID:    "run-resume",
		FilePath: "canary.bot",
		Source:   "workflow canary:\n  start -> done\n",
	}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err == nil {
		t.Fatal("expected SubmitResume to fail for an unresolved required secret")
	} else if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	}
	run, err := st.LoadRun(ctx, "run-resume")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("run status = %s, want failed_resumable (must stay resumable, not stranded queued)", run.Status)
	}
	if len(published) != 0 {
		t.Fatalf("no message should be published, got %d", len(published))
	}
}
