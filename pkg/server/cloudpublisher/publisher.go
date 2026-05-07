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
}

// Publisher is a runview.LaunchPublisher backed by NATS + Mongo.
type Publisher struct {
	nats    *natsq.Conn
	store   store.RunStore
	runs    *mongo.Collection
	logger  *iterlog.Logger
	metrics *metrics.Registry
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
		nats:    cfg.NATS,
		store:   cfg.Store,
		runs:    cfg.MongoColl,
		logger:  cfg.Logger,
		metrics: cfg.Metrics,
	}, nil
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
		FormatVersion: store.RunFormatVersion,
		ID:            runID,
		WorkflowName:  wf.Name,
		WorkflowHash:  hash,
		FilePath:      spec.FilePath,
		Status:        store.RunStatusQueued,
		Inputs:        varsAsAny(spec.Vars),
		CreatedAt:     now,
		UpdatedAt:     now,
		QueuedAt:      &now,
		TenantID:      tenantID,
		OwnerID:       ownerID,
	}
	if err := p.store.SaveRun(ctx, r); err != nil {
		return 0, fmt.Errorf("cloudpublisher: save run: %w", err)
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
		BackendConfig:  queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		TenantID:       tenantID,
		OwnerID:        ownerID,
	}
	if _, err := p.nats.PublishRun(ctx, msg); err != nil {
		// Best-effort: roll the run doc back to failed so the editor
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
		// Non-fatal: the editor falls back to "Waiting on the queue"
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
	r, err := p.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("cloudpublisher: load run %s: %w", runID, err)
	}
	switch r.Status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
		return nil // already terminal
	}
	if err := p.store.UpdateRunStatus(ctx, runID, store.RunStatusCancelled, "cancelled by user"); err != nil {
		return fmt.Errorf("cloudpublisher: flip status: %w", err)
	}
	if err := p.nats.CancelRun(runID); err != nil {
		p.logger.Warn("cloudpublisher: nats cancel %s: %v", runID, err)
	}
	return nil
}

// SubmitResume republishes a RunMessage with ResumeSpec set. The
// runner picks it up and dispatches to engine.Resume which threads
// the answers in.
//
// On publish failure the run is reverted to failed_resumable so the
// editor surfaces an actionable error instead of leaving a "queued"
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
	// cooperative-cancel check + so the editor's QueueDepthBar reflects
	// the in-flight resume.
	if err := p.store.UpdateRunStatus(ctx, spec.RunID, store.RunStatusQueued, ""); err != nil {
		return fmt.Errorf("cloudpublisher: requeue %s: %w", spec.RunID, err)
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
		BackendConfig:  queue.BackendConfig{Default: queue.BackendClaw},
		PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
		// Carry the prior run's tenant onto the resume publication so
		// the runner re-acquires the lease in the right scope. We trust
		// the loaded prior doc rather than ctx: a super-admin resuming
		// from another team's UI must still target that team's tenant.
		TenantID: prior.TenantID,
		OwnerID:  prior.OwnerID,
	}
	if _, err := p.nats.PublishRun(ctx, msg); err != nil {
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

// queuePosition counts the runs with status=queued and created_at
// less than or equal to ours. The result is 1-based, matching the
// "1st in queue" copy the editor renders.
func (p *Publisher) queuePosition(ctx context.Context, runID string) (int, error) {
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
// the editor SPA uploads source verbatim and the server pod has no
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
