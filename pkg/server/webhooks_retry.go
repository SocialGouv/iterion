package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

const (
	webhookRetryInterval = 20 * time.Second
	webhookRetryBatch    = 50
	webhookRetryBase     = 30 * time.Second
	webhookRetryMax      = 10 * time.Minute
)

func webhookMaxAttempts() int {
	// The belt, not the raw env: the name is credential-shaped
	// (envtrust.CredentialShapedName), so a project `.env` cannot plant it
	// and a plant that slipped past an older loader answers nothing here —
	// the operator's own export is the whole answer, as the bot-var gate
	// already treats it.
	if n, err := strconv.Atoi(strings.TrimSpace(envOperator("ITERION_WEBHOOK_MAX_ATTEMPTS"))); err == nil && n > 0 {
		return n
	}
	return 8
}

// setWebhookRetry schedules only failures proven to have created no run.
// A finite attempt budget applies to manual redrives and forge replays too.
func setWebhookRetry(d *webhooks.Delivery, now time.Time) {
	d.NextAttemptAt = nil
	if !retryableWebhookDelivery(*d) {
		return
	}
	if d.Attempts >= webhookMaxAttempts() {
		d.Status = webhooks.StatusLaunchExhausted
		return
	}
	wait := webhookRetryBase
	for i := 1; i < d.Attempts && wait < webhookRetryMax; i++ {
		wait = min(wait*2, webhookRetryMax)
	}
	next := now.Add(wait)
	d.NextAttemptAt = &next
}

func cloneWebhookVars(vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	return out
}

// acknowledgeWebhookLaunch lets net/http finish the response (including the
// HTTP/2 END_STREAM) while a registered worker finishes the claimed launch.
// The worker owns its response buffer; it never touches the network writer
// after this handler returns. Ordinary pre-claim responses wait for completion.
func (s *Server) acknowledgeWebhookLaunch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-s.shutdown:
			httpError(w, http.StatusServiceUnavailable, "server_draining")
			return
		default:
		}
		ready := make(chan webhookHTTPResponse, 1)
		if !s.tryGoSafe("webhookDelivery", func() {
			reply := &webhookResponseBuffer{header: make(http.Header), ready: ready}
			completed := false
			defer func() {
				if !completed && !reply.sent {
					reply.code = 0
					reply.body.Reset()
					s.httpErrorFor(reply, r, http.StatusInternalServerError, "webhook_handler_failed")
				}
				reply.Flush()
			}()
			ctx, cancel := launchBookingContext(r.Context())
			defer cancel()
			request := r.WithContext(ctx)
			next.ServeHTTP(errtrack.HTTPErrorWriter(reply, request, s.logger), request)
			completed = true
		}) {
			httpError(w, http.StatusServiceUnavailable, "server_draining")
			return
		}
		select {
		case response := <-ready:
			for k, values := range response.header {
				w.Header()[k] = values
			}
			w.WriteHeader(response.code)
			_, _ = w.Write(response.body)
		case <-r.Context().Done():
		}
	})
}

type webhookHTTPResponse struct {
	header http.Header
	code   int
	body   []byte
}

type webhookResponseBuffer struct {
	header http.Header
	code   int
	body   bytes.Buffer
	ready  chan<- webhookHTTPResponse
	sent   bool
}

func (w *webhookResponseBuffer) Header() http.Header { return w.header }
func (w *webhookResponseBuffer) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *webhookResponseBuffer) Write(body []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.body.Write(body)
}
func (w *webhookResponseBuffer) Flush() {
	if w.sent {
		return
	}
	if w.code == 0 {
		w.code = http.StatusOK
	}
	w.sent = true
	w.ready <- webhookHTTPResponse{header: w.header.Clone(), code: w.code, body: bytes.Clone(w.body.Bytes())}
}

// acceptWebhookTargets claims before acknowledging, then executes on the
// existing bounded launch context. acknowledgeWebhookLaunch owns the worker
// and the shutdown join for HTTP callers.
func (s *Server) acceptWebhookTargets(ctx context.Context, w http.ResponseWriter, r *http.Request, cfg webhooks.Config, meta webhookEventMeta, targets []forgeLaunchTarget, hash, ip string) {
	select {
	case <-s.shutdown:
		httpError(w, http.StatusServiceUnavailable, "server_draining")
		return
	default:
	}
	if s.webhookDeliveries == nil {
		httpError(w, http.StatusServiceUnavailable, "webhooks unavailable")
		return
	}
	ctx, cancel := launchBookingContext(ctx)
	defer cancel()
	results := make([]webhookLaunchResult, 0, len(targets))
	var launches []func(context.Context) webhookLaunchResult
	var firstID string
	for _, t := range targets {
		t.durableRetry = true
		result, launch := s.prepareWebhookTarget(ctx, r, cfg, meta, t, hash, ip)
		results = append(results, result)
		if launch != nil {
			launches = append(launches, launch)
			if firstID == "" {
				firstID = result.DeliveryID
			}
		}
	}
	if len(launches) == 0 {
		if len(results) == 1 {
			s.writeSingleLaunchResult(w, r, results[0])
			return
		}
		status, code := webhooks.StatusDuplicate, http.StatusOK
		for _, res := range results {
			if res.denial != nil {
				s.writeLaunchDenial(w, r, res.denial)
				return
			}
			if res.Status == webhooks.StatusFiltered && status == webhooks.StatusDuplicate {
				status = res.Status
			}
			if res.Status == webhooks.StatusLaunchError {
				status, code = res.Status, http.StatusBadGateway
			}
		}
		writeJSONStatus(w, code, map[string]any{"status": status, "launches": results})
		return
	}
	response := map[string]any{"status": webhooks.StatusAccepted, "delivery_id": firstID}
	if len(targets) > 1 {
		response["launches"] = results
	}
	writeWebhookAccepted(w, response)
	for _, launch := range launches {
		launchCtx, cancelLaunch := launchBookingContext(ctx)
		result := launch(launchCtx)
		cancelLaunch()
		if result.Status == webhooks.StatusLaunched {
			s.scheduleForgeBoardProjection(meta.ProjectPath)
		}
	}
}

type webhookAcceptedResponse struct {
	Status     string `json:"status"`
	DeliveryID string `json:"delivery_id"`
}

func writeWebhookAccepted(w http.ResponseWriter, response any) {
	body, _ := json.Marshal(response)
	w.Header().Set("Content-Type", "application/json")
	// Flush hands the framed response to acknowledgeWebhookLaunch; that
	// network handler returns so HTTP/2 also sees the end of the stream.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
}

func (s *Server) runWebhookRetrySweeper(ctx context.Context) {
	t := time.NewTicker(webhookRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweepWebhookRetries(ctx, now.UTC())
		}
	}
}

func (s *Server) sweepWebhookRetries(ctx context.Context, now time.Time) {
	due, err := s.webhookDeliveries.ListRetryDue(ctx, now, webhookRetryBatch)
	if err != nil {
		s.warnf("webhook retry sweep: %v", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		_, launch := s.prepareWebhookRetry(ctx, d)
		if launch == nil {
			continue
		}
		launchCtx, cancel := launchBookingContext(ctx)
		result := launch(launchCtx)
		cancel()
		if result.Status == webhooks.StatusLaunched {
			s.scheduleForgeBoardProjection(d.ProjectPath)
		}
	}
}

// retryableWebhookDelivery excludes legacy audit rows with no saved target and
// failures whose launcher already produced a run. Never infer absence from an
// error alone: RunPersistedError and QueueUnavailableError carry that fact.
func retryableWebhookDelivery(d webhooks.Delivery) bool {
	return d.Status == webhooks.StatusLaunchError && d.RunID == "" && d.RetryTarget != nil &&
		d.EventKind != autofixEventKind && d.EventKind != gateRelaunchEventKind
}

func (s *Server) prepareWebhookRetry(ctx context.Context, d webhooks.Delivery) (webhookLaunchResult, func(context.Context) webhookLaunchResult) {
	out := webhookLaunchResult{Status: d.Status, DeliveryID: d.ID, RunID: d.RunID, BotID: d.BotID}
	if !retryableWebhookDelivery(d) {
		out.Error = "delivery_not_retryable"
		return out, nil
	}
	cfg, err := s.webhookConfigs.Get(ctx, d.WebhookID)
	if err != nil || !cfg.Enabled || cfg.TenantID != d.TenantID || cfg.Provider != d.Provider || !cfg.AllowsBot(d.BotID) {
		if err != nil && !errors.Is(err, webhooks.ErrNotFound) {
			return s.failWebhookRetry(ctx, d, fmt.Sprintf("read webhook config: %v", err), false), nil
		}
		return s.failWebhookRetry(ctx, d, "webhook_retry_disabled", true), nil
	}
	if (cfg.ForkLane && !d.RetryTarget.Trust.IsFork()) || (!cfg.ForkLane && !d.RetryTarget.Trust.Trusted()) ||
		(!d.RetryTarget.Trust.Trusted() && d.RetryTarget.ExpectedSHA == "") {
		return s.failWebhookRetry(ctx, d, "webhook_retry_provenance_refused", true), nil
	}
	if d.Attempts >= webhookMaxAttempts() {
		return s.failWebhookRetry(ctx, d, "webhook_retry_exhausted", true), nil
	}
	// The operator authorizes the retry, but the webhook's own tenant pays
	// and passes admission. A super-admin request must not bypass its quotas.
	actor := webhookActor(cfg.ID)
	ctx = auth.WithIdentity(ctx, auth.Identity{UserID: actor, TeamID: cfg.TenantID, Role: identity.RoleMember, Kind: auth.KindWebhook})
	ctx = store.WithIdentity(ctx, cfg.TenantID, actor)
	t := d.RetryTarget
	meta := webhookEventMeta{Kind: d.EventKind, Action: d.EventAction, ProjectPath: d.ProjectPath,
		SubjectID: d.SubjectID, ParentSubjectID: d.ParentSubjectID, SubjectSHA: d.SubjectSHA}
	out, launch := s.prepareWebhookTarget(ctx, syntheticRequestForBase(d.PublicBase), cfg, meta, forgeLaunchTarget{
		durableRetry: true, retryAttempt: &d.Attempts, BotID: d.BotID, IdemKey: d.IdempotencyKey, Vars: cloneWebhookVars(t.Vars), RepoURL: t.RepoURL,
		RepoRef: t.RepoRef, Trust: t.Trust, ExpectedSHA: t.ExpectedSHA,
	}, d.PayloadHash, d.SourceIP)
	if launch == nil {
		return out, nil
	}
	// Keep the tenant identity on execution as well as on admission.
	return out, func(execCtx context.Context) webhookLaunchResult {
		execCtx = auth.WithIdentity(execCtx, auth.Identity{UserID: actor, TeamID: cfg.TenantID, Role: identity.RoleMember, Kind: auth.KindWebhook})
		return launch(store.WithIdentity(execCtx, cfg.TenantID, actor))
	}
}

// Even a pre-launch refusal consumes a retry attempt and backs off. Otherwise
// a temporarily disabled or toxic config is retried on every tick forever.
func (s *Server) failWebhookRetry(ctx context.Context, d webhooks.Delivery, reason string, terminal bool) webhookLaunchResult {
	expected := d.Attempts
	d.Attempts++
	now := s.gateNow()
	d.FailedAt, d.Error = &now, reason
	setWebhookRetry(&d, now)
	if terminal {
		d.Status, d.NextAttemptAt = webhooks.StatusLaunchExhausted, nil
	}
	claimed, err := s.webhookDeliveries.ClaimFailedRetry(ctx, d, expected)
	out := webhookLaunchResult{BotID: d.BotID, DeliveryID: d.ID, Status: d.Status, Error: reason, httpStatus: http.StatusConflict}
	if err != nil {
		out.Error, out.httpStatus = err.Error(), http.StatusInternalServerError
	}
	if err == nil && !claimed {
		out.Status, out.Error = webhooks.StatusDuplicate, "delivery_not_retryable"
	}
	return out
}

func (s *Server) handleRetryWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	if s.webhookDeliveries == nil || s.webhookConfigs == nil {
		httpError(w, http.StatusServiceUnavailable, "webhooks unavailable")
		return
	}
	select {
	case <-s.shutdown:
		httpError(w, http.StatusServiceUnavailable, "server_draining")
		return
	default:
	}
	ctx, cancel := launchBookingContext(r.Context())
	defer cancel()
	d, err := s.webhookDeliveries.Get(ctx, r.PathValue("id"))
	if errors.Is(err, webhooks.ErrNotFound) {
		httpError(w, http.StatusNotFound, "delivery_not_found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if !retryableWebhookDelivery(d) {
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "delivery_not_retryable", "status": d.Status, "delivery_id": d.ID})
		return
	}
	out, launch := s.prepareWebhookRetry(ctx, d)
	if launch == nil {
		if out.httpStatus >= 500 {
			httpError(w, out.httpStatus, "%s", out.Error)
			return
		}
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "delivery_not_retryable", "status": out.Status, "delivery_id": d.ID})
		return
	}
	writeWebhookAccepted(w, webhookAcceptedResponse{Status: webhooks.StatusAccepted, DeliveryID: d.ID})
	if result := launch(ctx); result.Status == webhooks.StatusLaunched {
		s.scheduleForgeBoardProjection(d.ProjectPath)
	}
}

// webhookLaunchConcurrency confirms capacity after earlier fan-out siblings
// have launched. Admission has already established the tenant and metered it.
func (s *Server) webhookLaunchConcurrency(ctx context.Context, cfg webhooks.Config, botID, repo string) *launchDenial {
	if id, _ := auth.FromContext(ctx); id.IsSuperAdmin {
		return nil
	}
	st := s.authStore()
	if st == nil {
		return nil
	}
	team, err := st.GetTeam(ctx, cfg.TenantID)
	if err != nil {
		return nil
	} // Same degraded-read policy as gateLaunch.
	return s.gateConcurrency(ctx, team, s.budgetFloorPolicy(ctx), launchSubject{BotID: botID, Repo: repo})
}
