package cloudpublisher

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mustDEKSealer rebuilds the per-run bundle opener from the DEK the
// resolution produced (ADR-123): the fixtures open exactly what the
// runner would — no ring in the path.
func mustDEKSealer(t *testing.T, dek []byte) secrets.Sealer {
	t.Helper()
	s, err := secrets.NewAESGCMSealer(dek)
	if err != nil {
		t.Fatalf("dek sealer: %v", err)
	}
	return s
}

func TestResolveAndSealCredentials_GenericWorkflowSecrets(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	creds, err := p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
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
	// The stamp witness: the record names the ring key that sealed it —
	// without it, rotation cannot target the bundle and every open falls
	// back to the legacy cohort path.
	if rec.KeyID != secrets.DEKKeyID {
		t.Fatalf("record key id %q, want the dek scheme", rec.KeyID)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", "run-1", rec.KeyID, rec.SealedBundle)
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
	if _, err := p.SubmitLaunch(ctx, "run-webhook", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
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
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	if _, err := p.SubmitLaunch(ctx, "run-resume", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-resume", store.RunStatusFailedResumable, "needs retry"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if err := p.SubmitResume(context.Background(), runview.ResumeSpec{
		RunID:   "run-resume",
		Source:  spec.Source,
		Answers: map[string]any{"ok": true},
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
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
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, resume.BundleDEK), rec.TenantID, "", "run-resume", rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	if got := bundle.GenericSecrets["forge_token"]; got != "bot-bound-token" {
		t.Fatalf("resume generic secret forge_token = %q, want bot-bound-token (via bot binding, with no same-name team secret)", got)
	}
}

func TestResolveAndSealCredentials_RequiredSecretUnresolvedFails(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	_, err = p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
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
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	creds, err := p.resolveAndSealCredentials(ctx, "run-1", "", "team", "alice", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
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
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	if _, err := p.SubmitLaunch(ctx, "run-canary", spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil {
		t.Fatal("expected SubmitLaunch to fail for an unresolved required secret")
	} else if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	} else if !errors.Is(err, secrets.ErrRequiredSecretUnresolved) {
		t.Fatalf("the launch refusal is not secrets.ErrRequiredSecretUnresolved: %v", err)
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
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": key}, "default")
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
	if err := p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil {
		t.Fatal("expected SubmitResume to fail for an unresolved required secret")
	} else if !strings.Contains(err.Error(), "test_e2e_canary") {
		t.Fatalf("error should name the secret, got %q", err.Error())
	} else if !errors.Is(err, secrets.ErrRequiredSecretUnresolved) || !runview.ResumeNeedsOperator(err) {
		// The retry sweeper ends its automation on it (runview.ResumeNeedsOperator):
		// the refusal must stay typed through every wrapper.
		t.Fatalf("the resume refusal is not a typed secrets.ErrRequiredSecretUnresolved: %v", err)
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

// #659 pt 1: an operator investigating "whose credential funded that
// run?" must find the answer in the server logs — not in
// `/proc/<pid>/environ` inside a pod (measured, 2026-09-03). The seal
// path used to print per-tier SKIPPED lines but nothing on a GRANT, so
// the log was one-sided. resolveAndSealCredentials now emits ONE INFO
// line per run naming every credential that lands in the sealed bundle,
// with its TIER (byok / oauth-forfait / pool / platform), slot (provider
// or oauth kind) and fingerprint. Never plaintext.
//
// The oracle is the LOG output, not the return value — the return has
// always been informative enough, the gap was the operator's inability
// to correlate it to a run without ad-hoc grep pipelines.
func TestResolveAndSealCredentials_LogsGrantedCredentialSet(t *testing.T) {
	var buf bytes.Buffer
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": bytes.Repeat([]byte{9}, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	apiKeys := secrets.NewMemoryApiKeyStore()
	tenantCtx := store.WithTenant(context.Background(), "team-1")
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("sk-ant-real"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeys.Create(tenantCtx, secrets.ApiKey{
		ID: id, ScopeTeamID: "team-1", Provider: secrets.ProviderAnthropic,
		Name: "k1", Last4: "real", SealedSecret: sealed,
		Fingerprint: secrets.FingerprintSHA256("sk-ant-real"),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	p := &Publisher{
		apiKeys:    apiKeys,
		sealer:     sealer,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		logger:     iterlog.New(iterlog.LevelInfo, &buf),
	}
	if _, err := p.resolveAndSealCredentials(tenantCtx, "run-log-1", "", "team-1", "u1", "bot", "", &ir.Workflow{}, nil, nil, model.RouteView{Workflow: &ir.Workflow{}}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "GRANTED for run=run-log-1") {
		t.Fatalf("expected a GRANTED line for the run; got:\n%s", log)
	}
	if !strings.Contains(log, "byok(api_key:anthropic") {
		t.Fatalf("expected the tier/slot naming; got:\n%s", log)
	}
	if strings.Contains(log, "sk-ant-real") {
		t.Fatalf("secret plaintext leaked into the log line: %s", log)
	}
	if !strings.Contains(log, "fp="+secrets.FingerprintSHA256("sk-ant-real")) {
		t.Fatalf("expected the credential fingerprint in the log; got:\n%s", log)
	}
}

// flakyGetSecrets is a secret store whose Get fails while failing is set: a
// store that could not answer (a primary step-down).
type flakyGetSecrets struct {
	secrets.GenericSecretStore
	failing bool
}

var errSecretStoreBlip = errors.New("secrets: get generic secret: server selection error: context deadline exceeded")

func (s *flakyGetSecrets) Get(ctx context.Context, id string) (secrets.GenericSecret, error) {
	if s.failing {
		return secrets.GenericSecret{}, errSecretStoreBlip
	}
	return s.GenericSecretStore.Get(ctx, id)
}

// An automatic resume whose required secret's every candidate failed to read
// — the bound secret's Get on a store blip, or its sealed value on a replica
// whose key ring predates it — is refused, but not as a secret that resolves
// to nothing: the retry sweeper re-arms it (runview.ResumeNeedsOperator
// false), and the resume succeeds once the read does. A missing required
// secret beside it is still an operator's. Red when a read error is typed
// secrets.ErrRequiredSecretUnresolved.
func TestSubmitResume_UnreadableRequiredSecretIsRetried(t *testing.T) {
	newRing := func(t *testing.T, ids ...string) (secrets.Sealer, map[string][]byte) {
		t.Helper()
		keys := map[string][]byte{}
		for _, id := range ids {
			k := make([]byte, 32)
			if _, err := rand.Read(k); err != nil {
				t.Fatal(err)
			}
			keys[id] = k
		}
		s, err := secrets.NewKeyRingSealer(keys, ids[len(ids)-1])
		if err != nil {
			t.Fatal(err)
		}
		return s, keys
	}
	type world struct {
		st        store.RunStore
		gs        *flakyGetSecrets
		published int
		pub       func(secrets.Sealer) *Publisher
	}
	setup := func(t *testing.T, sealer secrets.Sealer) *world {
		t.Helper()
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		mem := secrets.NewMemoryGenericSecretStore()
		id := secrets.NewGenericSecretID()
		sealed, err := secrets.SealGenericSecret(sealer, id, []byte("bot-bound-token"))
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Create(context.Background(), secrets.GenericSecret{ID: id, ScopeTeamID: "team", Name: "org_forge_token", SealedSecret: sealed, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		bindings := secrets.NewMemoryBotSecretBindingStore()
		if err := bindings.Create(context.Background(), secrets.BotSecretBinding{ID: "b1", TenantID: "team", BotID: "review-pr", SecretID: id, SecretNameForWorkflow: "forge_token", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		w := &world{st: st, gs: &flakyGetSecrets{GenericSecretStore: mem}}
		runSecrets := secrets.NewMemoryRunSecretsStore()
		w.pub = func(s secrets.Sealer) *Publisher {
			return &Publisher{store: st, genericSecrets: w.gs, botBindings: bindings, runSecrets: runSecrets, sealer: s,
				publishRun: func(context.Context, *queue.RunMessage) error { w.published++; return nil }}
		}
		return w
	}
	ctx := store.WithIdentity(context.Background(), "team", "webhook:user")
	spec := runview.LaunchSpec{FilePath: "review.bot", Source: "workflow review:\n  start -> done\n", BotID: "review-pr"}
	wf := &ir.Workflow{Name: "review", Secrets: map[string]*ir.Secret{"forge_token": {}}}
	launch := func(t *testing.T, w *world, p *Publisher, runID string) {
		t.Helper()
		if _, err := p.SubmitLaunch(ctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if err := w.st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "usage window"); err != nil {
			t.Fatal(err)
		}
	}
	resume := func(p *Publisher, runID string, wf *ir.Workflow) error {
		return p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, Source: spec.Source, Automatic: true}, wf, &runview.CompiledSource{Hash: "hash"})
	}
	retried := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("the resume was not refused")
		}
		if errors.Is(err, secrets.ErrRequiredSecretUnresolved) || runview.ResumeNeedsOperator(err) {
			t.Fatalf("a read error typed as a secret that resolves to nothing — the sweeper would abandon the run: %v", err)
		}
		if !strings.Contains(err.Error(), "forge_token") {
			t.Fatalf("the refusal does not name the secret: %v", err)
		}
	}

	t.Run("store blip", func(t *testing.T) {
		sealer, _ := newRing(t, "k1")
		w := setup(t, sealer)
		p := w.pub(sealer)
		launch(t, w, p, "run-blip")
		w.gs.failing = true
		err := resume(p, "run-blip", wf)
		retried(t, err)
		if !errors.Is(err, errSecretStoreBlip) {
			t.Fatalf("the refusal does not carry the store's error: %v", err)
		}
		w.gs.failing = false
		if err := resume(p, "run-blip", wf); err != nil || w.published != 2 {
			t.Fatalf("resume after the blip: err=%v published=%d, want it published", err, w.published)
		}
	})
	t.Run("older key ring", func(t *testing.T) {
		current, keys := newRing(t, "k1", "k2")
		older, err := secrets.NewKeyRingSealer(map[string][]byte{"k1": keys["k1"]}, "k1")
		if err != nil {
			t.Fatal(err)
		}
		w := setup(t, current)
		launch(t, w, w.pub(current), "run-ring")
		retried(t, resume(w.pub(older), "run-ring", wf))
		if err := resume(w.pub(current), "run-ring", wf); err != nil {
			t.Fatalf("resume on a replica holding the key: %v", err)
		}
	})
	t.Run("a missing secret beside it", func(t *testing.T) {
		sealer, _ := newRing(t, "k1")
		w := setup(t, sealer)
		p := w.pub(sealer)
		launch(t, w, p, "run-mixed")
		w.gs.failing = true
		both := &ir.Workflow{Name: "review", Secrets: map[string]*ir.Secret{"forge_token": {}, "never_stored": {}}}
		if err := resume(p, "run-mixed", both); !errors.Is(err, secrets.ErrRequiredSecretUnresolved) || !runview.ResumeNeedsOperator(err) {
			t.Fatalf("a required secret stored nowhere: %v, want the operator's refusal", err)
		}
	})
}
