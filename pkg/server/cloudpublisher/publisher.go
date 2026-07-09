// Package cloudpublisher wires runview.LaunchPublisher on top of
// NATS + Mongo so the cloud-mode `iterion server` can hand work off
// to the runner pool instead of executing in-process.
//
// The package lives under pkg/server/ rather than pkg/runview/ to
// keep the runview package free of NATS / Mongo imports — runview
// remains the local-mode entry point even when a cloud build is in
// the binary, and a build-time cycle would prevent that.
//
// Plan §F (T-31, T-32, T-33).
package cloudpublisher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"os"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Config bundles the dependencies of the publisher.
type Config struct {
	NATS  *natsq.Conn
	Store store.RunStore
	// MongoColl is the Mongo collection the publisher counts
	// queued runs against (for queue_position computation). The
	// caller passes it directly so the publisher doesn't have to
	// re-resolve it from the store interface.
	MongoColl *mongo.Collection
	Logger    *iterlog.Logger
	// Metrics, when non-nil, increments iterion_runs_created_total
	// after every successful Launch / Resume publish.
	Metrics *metrics.Registry

	// ApiKeys is the BYOK store. When non-nil, the publisher
	// resolves per-tenant credentials at launch time and seals
	// them into a per-run RunSecrets record. The runner unseals
	// and injects them into the engine ctx.
	ApiKeys secrets.ApiKeyStore
	// GenericSecrets stores workflow/user secrets addressable by name
	// from the DSL `secrets:` block.
	GenericSecrets secrets.GenericSecretStore
	// BotBindings, when non-nil, is consulted during generic-secret
	// resolution so an org-bound secret resolves for the launching bot
	// (user > binding > team priority).
	BotBindings secrets.BotSecretBindingStore
	// RunSecrets persists the sealed bundle keyed by SecretsRef.
	RunSecrets secrets.RunSecretsStore
	// Sealer is the AES-GCM master-key sealer (shared with the
	// REST handlers).
	Sealer secrets.Sealer
	// OAuthForfait is the per-user OAuth credential store. When
	// non-nil and a run's owner has connected an OAuth subscription,
	// the publisher embeds the verbatim credentials.json / auth.json
	// into the run bundle so the runner can materialise it for the
	// CLI subprocess.
	OAuthForfait secrets.OAuthStore
}

// Publisher is a runview.LaunchPublisher backed by NATS + Mongo.
type Publisher struct {
	nats           *natsq.Conn
	publishRun     func(context.Context, *queue.RunMessage) error
	cancelRun      func(string) error
	store          store.RunStore
	runs           *mongo.Collection
	logger         *iterlog.Logger
	metrics        *metrics.Registry
	apiKeys        secrets.ApiKeyStore
	genericSecrets secrets.GenericSecretStore
	botBindings    secrets.BotSecretBindingStore
	runSecrets     secrets.RunSecretsStore
	sealer         secrets.Sealer
	oauthForfait   secrets.OAuthStore

	// detached tracks fire-and-forget goroutines (e.g. MarkUsed
	// observability writes) so Drain can wait for them on shutdown
	// rather than letting orphan Mongo writes pile up against a
	// pod that's already past the SIGTERM mark.
	detached sync.WaitGroup
}

// New builds a Publisher.
func New(cfg Config) (*Publisher, error) {
	if cfg.NATS == nil {
		return nil, fmt.Errorf("cloudpublisher: NATS connection is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("cloudpublisher: Store is required")
	}
	if cfg.MongoColl == nil {
		return nil, fmt.Errorf("cloudpublisher: MongoColl is required for queue_position computation")
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &Publisher{
		nats: cfg.NATS,
		publishRun: func(ctx context.Context, msg *queue.RunMessage) error {
			_, err := cfg.NATS.PublishRun(ctx, msg)
			return err
		},
		cancelRun:      cfg.NATS.CancelRun,
		store:          cfg.Store,
		runs:           cfg.MongoColl,
		logger:         cfg.Logger,
		metrics:        cfg.Metrics,
		apiKeys:        cfg.ApiKeys,
		genericSecrets: cfg.GenericSecrets,
		botBindings:    cfg.BotBindings,
		runSecrets:     cfg.RunSecrets,
		sealer:         cfg.Sealer,
		oauthForfait:   cfg.OAuthForfait,
	}, nil
}

// allKnownProviders is the static set the publisher tries to resolve
// for every run. Providers without a configured key are simply
// omitted from the bundle; the runner falls back to env or surfaces
// "no credentials" at the LLM call site.
var allKnownProviders = []secrets.Provider{
	secrets.ProviderAnthropic,
	secrets.ProviderOpenAI,
	secrets.ProviderBedrock,
	secrets.ProviderVertex,
	secrets.ProviderAzure,
	secrets.ProviderOpenRouter,
	secrets.ProviderXAI,
}

func genericSecretNamesForWorkflow(wf *ir.Workflow) []string {
	if wf == nil || len(wf.Secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(wf.Secrets))
	for name, s := range wf.Secrets {
		if strings.TrimSpace(s.Value) != "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// resolveAndSealCredentials looks up every provider key visible to
// (tenantID, ownerID), pairs it with any OAuth-forfait the owner has
// connected, seals the resulting bundle, and persists it under a
// fresh secrets ref. Returns the ref or an empty string when no
// credentials are available — the runner then falls back to env.
func (p *Publisher) resolveAndSealCredentials(ctx context.Context, runID, tenantID, ownerID, botID string, wf *ir.Workflow, keyOverrides, secretOverrides map[string]string) (string, error) {
	if p.runSecrets == nil || p.sealer == nil {
		return "", nil
	}
	// Defence in depth: every caller (SubmitLaunch, SubmitResume)
	// already derives tenantID from either auth.FromContext or the
	// prior run document, but a future caller that forgets to thread
	// the identity must not silently produce a bundle keyed under
	// the empty tenant — that would let any team's runner unseal it.
	if tenantID == "" {
		if runID != "" && p.logger != nil {
			p.logger.Warn("cloudpublisher: refusing to seal credentials for run %s without a tenant_id", runID)
		}
		return "", nil
	}
	bundle := secrets.RunBundle{
		APIKeys:            map[secrets.Provider]string{},
		GenericSecrets:     map[string]string{},
		GenericSecretHosts: map[string][]string{},
		OAuthCredentials:   map[string][]byte{},
	}

	// 1. BYOK API keys.
	if p.apiKeys != nil {
		// Per-webhook key overrides (provider name → api_key id) take
		// precedence over the org/user default inside secrets.Resolve.
		var overrides map[secrets.Provider]string
		if len(keyOverrides) > 0 {
			overrides = make(map[secrets.Provider]string, len(keyOverrides))
			for prov, keyID := range keyOverrides {
				overrides[secrets.Provider(prov)] = keyID
			}
		}
		resolved, err := secrets.Resolve(ctx, p.apiKeys, tenantID, ownerID, allKnownProviders, overrides, p.sealer)
		if err != nil {
			return "", fmt.Errorf("cloudpublisher: resolve creds: %w", err)
		}
		now := time.Now().UTC()
		usedIDs := make([]string, 0, len(resolved))
		for prov, r := range resolved {
			if len(r.Plaintext) == 0 {
				continue
			}
			bundle.APIKeys[prov] = string(r.Plaintext)
			usedIDs = append(usedIDs, r.KeyID)
		}
		// Bumping last_used_at is best-effort observability, not on
		// the launch's critical path. Fire it detached with a short
		// timeout so a slow Mongo write doesn't block the NATS
		// publish.
		if len(usedIDs) > 0 {
			ids, t, tenant := usedIDs, now, tenantID
			p.goSafeDetached("apikey-markused", func() {
				// MarkUsed is tenant-filtered; carry the run's tenant onto the
				// detached ctx (matching the generic-secrets path below) or the
				// update silently matches nothing and last_used_at never moves.
				bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), tenant), 5*time.Second)
				defer cancel()
				for _, id := range ids {
					_ = p.apiKeys.MarkUsed(bg, id, t)
				}
			})
		}
	}

	// 2. Workflow/user generic secrets. A declared secret with an empty
	// value means "resolve a stored secret of the same name" for this run.
	if p.genericSecrets != nil && wf != nil && len(wf.Secrets) > 0 {
		names := genericSecretNamesForWorkflow(wf)
		resolved, err := secrets.ResolveGenericWithBindings(ctx, p.genericSecrets, p.botBindings, tenantID, ownerID, botID, names, secretOverrides, p.sealer)
		if err != nil {
			return "", fmt.Errorf("cloudpublisher: resolve workflow secrets: %w", err)
		}
		now := time.Now().UTC()
		usedIDs := make([]string, 0, len(resolved))
		for name, r := range resolved {
			if len(r.Plaintext) == 0 {
				continue
			}
			bundle.GenericSecrets[name] = string(r.Plaintext)
			// A binding-sourced resolution may carry an egress allowlist
			// that NARROWS where this credential can go. Thread it to the
			// runner, which intersects it with the workflow's declared
			// hosts in the secret guard. Empty = no binding restriction.
			if len(r.AllowedHosts) > 0 {
				bundle.GenericSecretHosts[name] = r.AllowedHosts
			}
			usedIDs = append(usedIDs, r.SecretID)
		}
		if len(usedIDs) > 0 {
			ids, t, tenant := usedIDs, now, tenantID
			p.goSafeDetached("generic-secret-markused", func() {
				bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), tenant), 5*time.Second)
				defer cancel()
				for _, id := range ids {
					_ = p.genericSecrets.MarkUsed(bg, id, t)
				}
			})
		}
	}

	// 3. OAuth-forfait blobs. Resolution is user-primary with an org
	//    fallback: the run owner's personal forfait wins per kind, and
	//    for any kind the owner hasn't connected we fall back to the
	//    team/org credential (stored under OrgOwnerKey(tenantID)). The
	//    org fallback is what covers automated runs (webhook/dispatcher/
	//    cron) whose owner is a synthetic identity with no personal
	//    forfait. The runner falls back to env when neither an API key
	//    nor an OAuth bundle is present.
	if p.oauthForfait != nil {
		addOAuth := func(ownerKey, label string) {
			if ownerKey == "" {
				return
			}
			records, err := p.oauthForfait.ListByUser(ctx, ownerKey)
			if err != nil {
				p.logger.Warn("cloudpublisher: oauth list for %s: %v", ownerKey, err)
				return
			}
			for _, rec := range records {
				// User record wins; don't let the org fallback overwrite it.
				if _, exists := bundle.OAuthCredentials[string(rec.Kind)]; exists {
					continue
				}
				payload, err := secrets.OpenOAuthPayload(p.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
				if err != nil {
					p.logger.Warn("cloudpublisher: unseal oauth %s/%s: %v", rec.UserID, rec.Kind, err)
					continue
				}
				bundle.OAuthCredentials[string(rec.Kind)] = payload
				p.logger.Info("cloudpublisher: oauth-forfait(%s) used run=%s owner=%s kind=%s", label, runID, ownerKey, rec.Kind)
			}
		}
		addOAuth(ownerID, "user")
		addOAuth(secrets.OrgOwnerKey(tenantID), "org")
	}

	if len(bundle.APIKeys) == 0 && len(bundle.GenericSecrets) == 0 && len(bundle.OAuthCredentials) == 0 {
		return "", nil
	}

	sealed, err := secrets.SealRunBundle(p.sealer, runID, bundle)
	if err != nil {
		return "", fmt.Errorf("cloudpublisher: seal bundle: %w", err)
	}
	ref := secrets.NewSecretsRef()
	now := time.Now().UTC()
	rec := secrets.RunSecretsRecord{
		ID:           ref,
		TenantID:     tenantID,
		RunID:        runID,
		SealedBundle: sealed,
		CreatedAt:    now,
		ExpiresAt:    now.Add(secrets.DefaultRunSecretsTTL),
	}
	if err := p.runSecrets.Put(ctx, rec); err != nil {
		return "", fmt.Errorf("cloudpublisher: persist run secrets: %w", err)
	}
	return ref, nil
}

// SubmitLaunch persists the run as queued in Mongo, then publishes
// the RunMessage to JetStream. The runner pool drains the queue and
// transitions queued → running on pickup.
//
// Tenant and owner identifiers are pulled from ctx (stamped by the
// server's auth middleware) and propagate to both the persisted Run
// document and the NATS message so the runner can verify isolation.
func (p *Publisher) SubmitLaunch(ctx context.Context, runID string, spec runview.LaunchSpec, wf *ir.Workflow, hash string) (int, error) {
	// 1. Persist the run with status=queued + workflow_hash + file_path
	//    so List endpoints see it instantly and Resume can reload the
	//    workflow. Single SaveRun (upsert) avoids the CreateRun → LoadRun
	//    → SaveRun round-trip the previous shape required.
	now := time.Now().UTC()
	tenantID, _ := store.TenantFromContext(ctx)
	ownerID, _ := store.OwnerFromContext(ctx)
	r := &store.Run{
		FormatVersion:   store.RunFormatVersion,
		ID:              runID,
		WorkflowName:    wf.Name,
		WorkflowHash:    hash,
		FilePath:        spec.FilePath,
		Status:          store.RunStatusQueued,
		Inputs:          varsAsAny(spec.Vars),
		CreatedAt:       now,
		UpdatedAt:       now,
		QueuedAt:        &now,
		TenantID:        tenantID,
		OwnerID:         ownerID,
		RepoURL:         spec.RepoURL,
		RepoSHA:         spec.RepoRef,
		ProjectPath:     spec.ProjectPath,
		BotID:           spec.BotID,
		KeyOverrides:    spec.KeyOverrides,
		SecretOverrides: spec.SecretOverrides,
		// Cap. 3 sharding fields — propagate to the persisted Run so
		// studio surfaces can render the parent/child relationship,
		// and onto the published RunMessage below so the runner pod
		// that claims this work knows its place in the shard set.
		ParentRunID:        spec.ParentRunID,
		ShardIndex:         spec.ShardIndex,
		ShardCount:         spec.ShardCount,
		ShardLabel:         spec.ShardLabel,
		CallbackURL:        spec.CallbackURL,
		CallbackToken:      spec.CallbackToken,
		CallbackAnswerNode: spec.CallbackAnswerNode,
	}
	if err := p.store.SaveRun(ctx, r); err != nil {
		return 0, fmt.Errorf("cloudpublisher: save run: %w", err)
	}

	// 1b. Resolve BYOK credentials and seal them under a fresh
	//     secrets_ref. Empty ref means "no team-scoped credentials
	//     configured" — the runner falls back to env.
	secretsRef, err := p.resolveAndSealCredentials(ctx, runID, tenantID, ownerID, spec.BotID, wf, spec.KeyOverrides, spec.SecretOverrides)
	if err != nil {
		return 0, err
	}

	// 2. Build the RunMessage. We marshal the AST inline; T-42 will
	//    add the IRRef fallback for oversized workflows. The
	//    runner side re-parses + re-compiles, so the wire payload
	//    is the AST File, not the compiled IR.
	body, err := marshalIRFromSpec(spec.FilePath, spec.Source)
	if err != nil {
		return 0, err
	}
	msg := &queue.RunMessage{
		V:              queue.SchemaVersion,
		RunID:          runID,
		WorkflowName:   wf.Name,
		WorkflowHash:   hash,
		IRCompiled:     body,
		Vars:           varsAsAny(spec.Vars),
		SecretsRef:     secretsRef,
		BackendConfig:  queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		TenantID:       tenantID,
		OwnerID:        ownerID,
		// Cap. 3 sharding: when this run is a child shard, the runner
		// pod that picks it up sees its place in the set so the studio
		// can group siblings and so a future event-based aggregator
		// can correlate completions.
		ParentRunID:        spec.ParentRunID,
		ShardIndex:         spec.ShardIndex,
		ShardCount:         spec.ShardCount,
		ShardLabel:         spec.ShardLabel,
		CallbackURL:        spec.CallbackURL,
		CallbackToken:      spec.CallbackToken,
		CallbackAnswerNode: spec.CallbackAnswerNode,
		// Repo to clone before sandboxing (webhook-launched runs have no
		// operator checkout). RepoRef carries a branch or sha. ProjectPath
		// is NOT on the wire — the runner clones from RepoURL and the
		// persisted run doc is the authoritative carrier of the slug.
		RepoURL: spec.RepoURL,
		RepoSHA: spec.RepoRef,
		BotID:   spec.BotID,
	}
	if err := p.publish(ctx, msg); err != nil {
		// Best-effort: roll the run doc back to failed so the studio
		// surfaces the queue failure rather than a stuck "queued"
		// row that never moves.
		_ = p.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, fmt.Sprintf("queue publish: %v", err))
		return 0, fmt.Errorf("cloudpublisher: publish: %w", err)
	}
	if p.metrics != nil {
		p.metrics.RunsCreatedTotal.WithLabelValues(string(store.RunStatusQueued)).Inc()
	}

	// 3. Compute queue position: count of runs with status=queued
	//    and created_at <= ours.
	pos, err := p.queuePosition(ctx, runID)
	if err != nil {
		// Non-fatal: the studio falls back to "Waiting on the queue"
		// generic copy when queue_position is zero.
		p.logger.Warn("cloudpublisher: queue position lookup: %v", err)
	}
	return pos, nil
}

// CancelRun flips the Mongo doc to cancelled. Two effects:
//   - the runner's cooperative-cancel check on next pickup acks the
//     JetStream delivery without executing;
//   - if a runner is currently holding the lease, the cancel subject
//     `iterion.cancel.<run_id>` unwinds engine.Run.
//
// Idempotent: running CancelRun on an already-terminal run is a no-op.
func (p *Publisher) CancelRun(ctx context.Context, runID string) error {
	// Cancel descends here with a NON-request context (runview.Service.Cancel
	// takes no ctx), so the mongo tenant filter has no tenant and its
	// tenant-scoped queries panic. The caller (handleCancelRun / the WS
	// handleCancel) has ALREADY gated access with a tenant-scoped LoadRunCtx,
	// so bypass the filter explicitly here rather than crashing — exactly the
	// seam store.WithoutTenantFilter exists for. Without this, cancelling any
	// cloud run 502s (panic: "tenant-scoped query without tenant in ctx").
	ctx = store.WithoutTenantFilter(ctx)
	r, err := p.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("cloudpublisher: load run %s: %w", runID, err)
	}
	switch r.Status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
		return nil // already terminal
	}
	// CAS on the cancellable statuses. A SubmitResume that races this
	// call can flip queued → running between our LoadRun and the
	// UpdateRunStatus below; without the conditional update we'd
	// silently overwrite the in-flight resume back to cancelled with
	// no visible warning. The expectedFrom set lists every status we
	// consider cancellable here.
	cancellable := []store.RunStatus{
		store.RunStatusQueued,
		store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusFailedResumable,
	}
	changed, err := p.store.UpdateRunStatusIf(ctx, runID, store.RunStatusCancelled, "cancelled by user", cancellable)
	if err != nil {
		return fmt.Errorf("cloudpublisher: flip status: %w", err)
	}
	if !changed {
		// Status raced from under us — re-read and decide if this is
		// an actual no-op (already terminal) or a transient state we
		// should surface for the operator to retry.
		r2, _ := p.store.LoadRun(ctx, runID)
		if r2 != nil {
			switch r2.Status {
			case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
				return nil
			}
			return fmt.Errorf("cloudpublisher: cancel raced (status now %s) — retry", r2.Status)
		}
		return nil
	}
	if err := p.cancel(runID); err != nil {
		p.logger.Warn("cloudpublisher: nats cancel %s: %v", runID, err)
	}
	return nil
}

// SubmitResume republishes a RunMessage with ResumeSpec set. The
// runner picks it up and dispatches to engine.Resume which threads
// the answers in.
//
// On publish failure the run is reverted to failed_resumable so the
// studio surfaces an actionable error instead of leaving a "queued"
// row that no runner will ever pick up. Mirrors the rollback pattern
// in SubmitLaunch.
func (p *Publisher) SubmitResume(ctx context.Context, spec runview.ResumeSpec, wf *ir.Workflow, hash string) error {
	body, err := marshalIRFromSpec(spec.FilePath, spec.Source)
	if err != nil {
		return err
	}
	// Capture the prior status so we can roll back to the right
	// resumable state if publish fails — the user could be resuming
	// from paused_waiting_human, failed_resumable, or cancelled.
	prior, loadErr := p.store.LoadRun(ctx, spec.RunID)
	if loadErr != nil {
		return fmt.Errorf("cloudpublisher: load prior run %s: %w", spec.RunID, loadErr)
	}
	priorStatus := prior.Status
	// Flip status to queued so the runner doesn't short-circuit on the
	// cooperative-cancel check + so the studio's QueueDepthBar reflects
	// the in-flight resume.
	if err := p.store.UpdateRunStatus(ctx, spec.RunID, store.RunStatusQueued, ""); err != nil {
		return fmt.Errorf("cloudpublisher: requeue %s: %w", spec.RunID, err)
	}
	// Re-resolve credentials for the resume publication. Keys may have
	// rotated between launch and resume; using the prior run's secrets ref
	// blindly would inject stale plaintext. Preserve the launching BotID so
	// bot-secret bindings remain durable across pause/failure/TTL republishes.
	secretsCtx := store.WithTenant(ctx, prior.TenantID)
	secretsCtx = store.WithOwner(secretsCtx, prior.OwnerID)
	secretsRef, secretsErr := p.resolveAndSealCredentials(secretsCtx, spec.RunID, prior.TenantID, prior.OwnerID, prior.BotID, wf, prior.KeyOverrides, prior.SecretOverrides)
	if secretsErr != nil {
		return secretsErr
	}
	msg := &queue.RunMessage{
		V:            queue.SchemaVersion,
		RunID:        spec.RunID,
		WorkflowName: wf.Name,
		WorkflowHash: hash,
		IRCompiled:   body,
		Resume: &queue.ResumeSpec{
			Answers: spec.Answers,
			Force:   spec.Force,
		},
		SecretsRef:     secretsRef,
		BackendConfig:  queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		// Carry the prior run's tenant onto the resume publication so
		// the runner re-acquires the lease in the right scope. We trust
		// the loaded prior doc rather than ctx: a super-admin resuming
		// from another team's UI must still target that team's tenant.
		TenantID: prior.TenantID,
		OwnerID:  prior.OwnerID,
		// Preserve webhook/cloud source metadata so a resumed runner can
		// reconstruct the same workspace as the original launch. ProjectPath
		// is carried by the persisted run doc, not the wire.
		RepoURL: prior.RepoURL,
		RepoSHA: prior.RepoSHA,
		BotID:   prior.BotID,
	}
	if err := p.publish(ctx, msg); err != nil {
		// Revert to the prior resumable status — typically
		// failed_resumable so the next user action is "Resume"
		// again. Falling back to failed_resumable is conservative
		// when the prior status wasn't itself resumable.
		rollback := priorStatus
		switch rollback {
		case store.RunStatusPausedWaitingHuman, store.RunStatusFailedResumable, store.RunStatusCancelled:
			// keep as-is
		default:
			rollback = store.RunStatusFailedResumable
		}
		if rbErr := p.store.UpdateRunStatus(ctx, spec.RunID, rollback, fmt.Sprintf("queue republish: %v", err)); rbErr != nil {
			p.logger.Error("cloudpublisher: rollback %s after publish failure: %v", spec.RunID, rbErr)
		}
		return fmt.Errorf("cloudpublisher: republish: %w", err)
	}
	if p.metrics != nil {
		p.metrics.RunsCreatedTotal.WithLabelValues("resumed").Inc()
	}
	return nil
}

func (p *Publisher) publish(ctx context.Context, msg *queue.RunMessage) error {
	if p.publishRun != nil {
		return p.publishRun(ctx, msg)
	}
	if p.nats == nil {
		return fmt.Errorf("cloudpublisher: NATS publisher is not configured")
	}
	_, err := p.nats.PublishRun(ctx, msg)
	return err
}

func (p *Publisher) cancel(runID string) error {
	if p.cancelRun != nil {
		return p.cancelRun(runID)
	}
	if p.nats == nil {
		return fmt.Errorf("cloudpublisher: NATS publisher is not configured")
	}
	return p.nats.CancelRun(runID)
}

// queuePosition counts the runs with status=queued and created_at
// less than or equal to ours. The result is 1-based, matching the
// "1st in queue" copy the studio renders.
func (p *Publisher) queuePosition(ctx context.Context, runID string) (int, error) {
	if p.runs == nil {
		return 0, nil
	}
	var doc struct {
		CreatedAt time.Time `bson:"created_at"`
	}
	if err := p.runs.FindOne(ctx, bson.M{"_id": runID}).Decode(&doc); err != nil {
		return 0, err
	}
	count, err := p.runs.CountDocuments(ctx, bson.M{
		"status":     store.RunStatusQueued,
		"created_at": bson.M{"$lte": doc.CreatedAt},
	})
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

// marshalIRFromSpec returns the AST.File bytes for the workflow.
// Resolution order: inline `source` (preferred in cloud mode where
// the studio SPA uploads source verbatim and the server pod has no
// shared filesystem) → `path` on local disk (fallback for tests and
// migration tooling). The runner re-parses + re-compiles, so the
// wire payload is the AST File, not the compiled IR.
func marshalIRFromSpec(path, source string) (json.RawMessage, error) {
	var src string
	parserPath := path
	switch {
	case source != "":
		src = source
		if parserPath == "" {
			parserPath = "<inline>"
		}
	case path != "":
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cloudpublisher: read %s: %w", path, err)
		}
		src = string(body)
	default:
		return nil, fmt.Errorf("cloudpublisher: launch spec has no source and no file_path; cannot serialise IR")
	}
	pr := parser.Parse(parserPath, src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("cloudpublisher: parse %s: %s", parserPath, d.Error())
		}
	}
	if pr.File == nil {
		return nil, fmt.Errorf("cloudpublisher: empty AST for %s", parserPath)
	}
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		return nil, fmt.Errorf("cloudpublisher: marshal IR: %w", err)
	}
	return body, nil
}

// Drain waits for any in-flight fire-and-forget goroutines (MarkUsed
// writes, etc.) to complete or the supplied ctx to fire — whichever
// comes first. Returns the ctx error on timeout so the server's
// graceful-shutdown path can log how many writes were lost.
//
// Safe to call multiple times; concurrent calls all observe the same
// WaitGroup.
func (p *Publisher) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.detached.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// goSafeDetached runs fn in a tracked, panic-recovering goroutine: it
// increments the detached WaitGroup so Drain still waits for it, and
// contains any panic in the best-effort body (a MarkUsed write into a
// driver) so it can't crash the process from a goroutine the caller
// can't recover. label identifies the task in the recovery log.
func (p *Publisher) goSafeDetached(label string, fn func()) {
	p.detached.Add(1)
	go func() {
		defer p.detached.Done()
		defer func() {
			if r := recover(); r != nil && p.logger != nil {
				p.logger.Error("cloudpublisher: detached task %q panicked: %v", label, r)
			}
		}()
		fn()
	}()
}

// varsAsAny upgrades a string-keyed map to interface{} so the wire
// payload can carry richer types if the launch spec ever evolves.
func varsAsAny(in map[string]string) map[string]interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
