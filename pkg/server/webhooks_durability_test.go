package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

func waitWebhookWorkers(t *testing.T, s *Server) {
	t.Helper()
	s.bgMu.Lock()
	workers := append([]backgroundWorker(nil), s.bgWorkers...)
	s.bgMu.Unlock()
	for _, w := range workers {
		select {
		case <-w.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("worker %s did not finish", w.name)
		}
	}
}

// W1: mutate acceptWebhookTargets to execute on r.Context() instead of
// launchBookingContext (remove all three detach sites in the HTTP path). The forge closes its
// HTTP/2 connection after 100ms, while launch is still in progress: the mutant
// records launch_error and produces zero runs. Removing acknowledgeWebhookLaunch
// from the route also reddens the ACK assertion (HTTP/2 has no END_STREAM).
func TestWebhookDurability_ForgeDisconnectAfter100ms(t *testing.T) {
	for _, lane := range []string{"pull_request", "comment", "review_thread", "issues"} {
		t.Run(lane, func(t *testing.T) {
			s := newWebhookTestServer(t)
			s.registerGitHubWebhookRoute()
			s.cfg.Bots.Paths = []string{botsDirAbs(t)}
			s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
				return gateAuthorized, "allowed", nil
			}
			s.webhookPRForgePRResolver = func(_ context.Context, _ webhooks.Config, _ webhooks.Provider, p prforge.ParsedNote, _ webhooks.CommandRoute) (forge.PullRef, error) {
				return forge.PullRef{State: "open", HeadRepoFullName: p.ProjectPath, SourceBranch: "feature/x", TargetBranch: "main"}, nil
			}
			cfg, secret := ghConfig(t, s)
			payload, event := ghOpenPR, prforge.EventHeaderPullRequest
			switch lane {
			case "comment":
				payload, event = ghIssueCommentFeaturly, prforge.EventHeaderIssueComment
				cfg.BotIDs = []string{"feature-dev"}
				cfg.CommandMap = map[string][]webhooks.CommandRoute{"featurly": {{BotID: "feature-dev", Scope: "any", ArgsVar: "feature_prompt"}}}
			case "review_thread":
				payload, event = ghReviewCommentReply("alice", "explain the finding"), prforge.EventHeaderReviewComment
				cfg.BotIDs = []string{"revi-converse"}
				s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
					return true, "thread", "allowed", nil
				}
			case "issues":
				cfg, secret = issueConfig(t, s)
				payload, event = ghLabeledIssue, prforge.EventHeaderIssues
			}
			if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			closed := make(chan struct{})
			var runs atomic.Int32
			s.webhookLaunchBot = func(ctx context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
				<-closed
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if _, ok := ctx.Deadline(); !ok {
					return "", errors.New("launch is unbounded")
				}
				id, _ := auth.FromContext(ctx)
				if id.TeamID != cfg.TenantID {
					return "", errors.New("lost tenant identity")
				}
				runs.Add(1)
				return "run-after-disconnect", nil
			}
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			go (&http2.Server{}).ServeConn(server, &http2.ServeConnOpts{Handler: s.mux})
			cc, err := (&http2.Transport{}).NewClientConn(client)
			if err != nil {
				t.Fatal(err)
			}
			defer cc.Close()
			req := ghReq(context.Background(), payload, event, secret)
			req.URL.Scheme, req.URL.Host, req.RequestURI = "http", "forge.test", ""
			// This fake forge does not redeliver. Its connection has its own
			// clock, independent of the server's detached launch deadline.
			timer := time.AfterFunc(100*time.Millisecond, func() { _ = cc.Close(); close(closed) })
			defer timer.Stop()
			resp, err := cc.RoundTrip(req)
			if err == nil {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil {
					var ack map[string]string
					if err := json.Unmarshal(body, &ack); err != nil {
						t.Fatalf("ACK: %s (%v)", body, err)
					}
					if resp.StatusCode != 202 || ack["status"] != webhooks.StatusAccepted || ack["delivery_id"] == "" {
						t.Fatalf("ACK: %d %s", resp.StatusCode, body)
					}
				}
			}
			<-closed
			waitWebhookWorkers(t, s)
			rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 10)
			if err != nil || len(rows) != 1 || rows[0].Status != webhooks.StatusLaunched || rows[0].RunID != "run-after-disconnect" || runs.Load() != 1 {
				t.Fatalf("runs=%d deliveries=%+v err=%v", runs.Load(), rows, err)
			}
		})
	}
}

func failedDurableWebhook(t *testing.T, s *Server) (webhooks.Config, webhooks.Delivery) {
	t.Helper()
	cfg, secret := ghConfig(t, s)
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "", errors.New("temporary launch failure")
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, secret))
	var ack map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	d, err := s.webhookDeliveries.Get(context.Background(), ack["delivery_id"])
	if err != nil || d.Status != webhooks.StatusLaunchError || d.NextAttemptAt == nil || d.RetryTarget == nil {
		t.Fatalf("seed: %+v %v", d, err)
	}
	return cfg, d
}

// W2: remove the sweep's prepare/launch call => zero runs. Replace
// ClaimFailedRetry by unconditional Update => the concurrent stale offers can
// both launch. Ignoring the replay RunID/status guard duplicates the last offer.
func TestWebhookDurability_SweepRetriesExactlyOnce(t *testing.T) {
	s := newWebhookTestServer(t)
	_, d := failedDurableWebhook(t, s)
	var runs atomic.Int32
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		runs.Add(1)
		return "one-run", nil
	}
	s.sweepWebhookRetries(context.Background(), d.NextAttemptAt.Add(-time.Nanosecond))
	if runs.Load() != 0 {
		t.Fatal("backoff ignored")
	}
	s.webhookDeliveries = newBarrierDeliveryStore(s.webhookDeliveries, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt) })
	}
	wg.Wait()
	s.sweepWebhookRetries(context.Background(), d.NextAttemptAt.Add(time.Hour))
	if _, launch := s.prepareWebhookRetry(context.Background(), d); launch != nil {
		launch(context.Background())
	}
	got, err := s.webhookDeliveries.Get(context.Background(), d.ID)
	if err != nil || runs.Load() != 1 || got.RunID != "one-run" || got.Attempts != 2 {
		t.Fatalf("runs=%d delivery=%+v err=%v", runs.Load(), got, err)
	}
}

// W3: remove the route's launch call => zero runs and a stuck accepted row.
// Replace the route's retryability refusal with an unconditional launch =>
// the second request creates another run. Dropping requireSuperAdmin admits the
// ordinary member. The accepted response names the same durable delivery.
func TestWebhookDurability_RetryRoute(t *testing.T) {
	s := newWebhookTestServer(t)
	s.registerWebhookRoutes()
	var err error
	s.signer, err = auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(make([]byte, 32)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, d := failedDurableWebhook(t, s)
	var runs atomic.Int32
	s.webhookLaunchBot = func(ctx context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		id, _ := auth.FromContext(ctx)
		if id.IsSuperAdmin || id.TeamID != d.TenantID {
			return "", errors.New("retry executes as operator")
		}
		runs.Add(1)
		return "one-run", nil
	}
	call := func(admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/webhooks/deliveries/"+d.ID+"/retry", nil)
		token, _, err := s.signer.IssueAccess(auth.Identity{UserID: "admin", IsSuperAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		waitWebhookWorkers(t, s)
		return rec
	}
	if rec := call(false); rec.Code != http.StatusForbidden {
		t.Fatalf("member: %d %s", rec.Code, rec.Body)
	}
	if rec := call(true); rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	rec := call(true)
	var refusal map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	if rec.Code != http.StatusConflict || refusal["error"] != "delivery_not_retryable" || refusal["status"] != webhooks.StatusLaunched || runs.Load() != 1 {
		t.Fatalf("repeat: runs=%d %d %s", runs.Load(), rec.Code, rec.Body)
	}
}

// W4: remove the worker registration => Shutdown returns while launch is
// blocked. Link the launch context to shutdown => ctx.Err becomes canceled.
func TestWebhookDurability_ShutdownJoinsWithoutCancelingLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.bgJoinBudget = time.Second
	cfg, secret := ghConfig(t, s)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s.webhookLaunchBot = func(ctx context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		close(started)
		<-release
		finished <- ctx.Err()
		return "drained-run", ctx.Err()
	}
	req := ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, secret)
	rec := httptest.NewRecorder()
	s.acknowledgeWebhookLaunch(http.HandlerFunc(s.handleGitHubWebhook)).ServeHTTP(rec, req)
	<-started
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stopped <- s.Shutdown(ctx)
	}()
	select {
	case <-s.shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown not signaled")
	}
	select {
	case err := <-stopped:
		t.Fatalf("shutdown skipped the in-flight launch: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatalf("shutdown canceled launch: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestWebhookDurability_PersistedRunIsNeverRelaunched(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprint(named), func(t *testing.T) {
			s := newWebhookTestServer(t)
			cfg, secret := ghConfig(t, s)
			if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				calls++
				if named {
					return "", &runview.RunPersistedError{RunID: "already-created", Err: errors.New("write failed after creation")}
				}
				return "", &runview.QueueUnavailableError{Cause: errors.New("publish may have landed")}
			}
			for range 2 {
				s.handleGitHubWebhook(httptest.NewRecorder(), ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, secret))
			}
			s.sweepWebhookRetries(context.Background(), time.Now().Add(time.Hour))
			if calls != 1 {
				t.Fatalf("created %d runs for an ambiguous launch", calls)
			}
		})
	}
}

// Removing acknowledgeWebhookLaunch leaves HTTP/2 without END_STREAM while
// the launch is blocked, even though the headers and JSON have been flushed.
func TestWebhookDurability_HTTP2AcknowledgesBeforeLaunchCompletes(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, _ := ghConfig(t, s)
	release := make(chan struct{})
	defer func() { close(release); waitWebhookWorkers(t, s) }()
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		<-release
		return "run", nil
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	handler := s.acknowledgeWebhookLaunch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.acceptWebhookTargets(ghCtx(cfg), w, r, cfg, webhookEventMeta{}, []forgeLaunchTarget{{BotID: "review-pr", IdemKey: "ack-before-launch", Vars: map[string]string{}}}, "hash", "ip")
	}))
	go (&http2.Server{}).ServeConn(server, &http2.ServeConnOpts{Handler: handler})
	cc, err := (&http2.Transport{}).NewClientConn(client)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://forge.test/webhook", nil)
	resp, err := cc.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("ACK waits for launch: %v", err)
	}
	var ack map[string]string
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatal(err)
	}
	d, err := s.webhookDeliveries.Get(ctx, ack["delivery_id"])
	if resp.StatusCode != 202 || err != nil || d.Status != webhooks.StatusAccepted || d.RunID != "" {
		t.Fatalf("ACK before durable claim: %d %s row=%+v err=%v", resp.StatusCode, body, d, err)
	}
}

// Regression witnesses from the adversarial round: removing the provenance
// refusal, snapshot attempt check, or gate-owner exclusion reddens each one.
func seedRetryReview(t *testing.T, mismatch bool) (*Server, webhooks.Delivery) {
	s := newWebhookTestServer(t)
	cfg := webhooks.Config{ID: "review-wh", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"review-pr"}, ForkLane: mismatch}
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	d := webhooks.Delivery{ID: "review-delivery", WebhookID: cfg.ID, TenantID: cfg.TenantID, Provider: cfg.Provider, Status: webhooks.StatusLaunchError, IdempotencyKey: "review-idem", BotID: "review-pr", Attempts: 1, NextAttemptAt: &old, RetryTarget: &webhooks.DeferredTarget{BotID: "review-pr", IdemKey: "review-idem", Vars: map[string]string{}}}
	if err := s.webhookDeliveries.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return s, d
}
func TestWebhookDurability_RetryMismatchMustTerminate(t *testing.T) {
	t.Setenv("ITERION_WEBHOOK_MAX_ATTEMPTS", "2")
	s, d := seedRetryReview(t, true)
	for i := 0; i < 10; i++ {
		s.sweepWebhookRetries(context.Background(), time.Now())
	}
	got, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	rows, _ := s.webhookDeliveries.ListByWebhook(context.Background(), d.TenantID, d.WebhookID, 100)
	t.Logf("after 10 sweeps: original status=%s attempts=%d next=%v; audit rows=%d", got.Status, got.Attempts, got.NextAttemptAt, len(rows))
	if got.Status != webhooks.StatusLaunchExhausted {
		t.Fatalf("retry is still due, expected terminal status")
	}
}
func TestWebhookDurability_StaleSnapshotMustNotSpendBackoff(t *testing.T) {
	s, d := seedRetryReview(t, false)
	count := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		count++
		return "", errors.New("temporary failure")
	}
	for i := 0; i < 3; i++ {
		out, launch := s.prepareWebhookRetry(context.Background(), d)
		if launch == nil {
			t.Logf("prepare %d: %+v", i, out)
			continue
		}
		launch(context.Background())
	}
	got, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	t.Logf("one stale due snapshot processed three times: launch calls=%d attempts=%d next=%v", count, got.Attempts, got.NextAttemptAt)
	if count != 1 {
		t.Fatalf("stale due rows bypass backoff: launches=%d, want 1", count)
	}
}

func TestWebhookDurability_GenericSweepMustRespectGateExhaustion(t *testing.T) {
	s, d := seedRetryReview(t, false)
	d.EventKind = autofixEventKind
	d.Attempts = maxUnattendedLaunchAttempts
	if err := s.webhookDeliveries.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	verdict, _ := unattendedLaunchRetry(d, true, time.Now())
	if verdict != launchRetryExhausted {
		t.Fatalf("fixture not exhausted: %v", verdict)
	}
	count := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		count++
		return "gate-run-after-exhaustion", nil
	}
	s.sweepWebhookRetries(context.Background(), time.Now())
	got, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	t.Logf("gate lane verdict=exhausted; generic sweep launches=%d delivery=%s attempts=%d run=%s", count, got.Status, got.Attempts, got.RunID)
	if count != 0 {
		t.Fatalf("generic retry bypasses unattended gate exhaustion")
	}
}

// Removing webhookLaunchConcurrency admits two siblings at a concurrency cap of one.
func TestWebhookDurability_FanoutPreservesConcurrency(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := seedGate(t, s, gateSpec{id: "review-t1", maxConcurrentRuns: 1})
	s.cfg.Store = fakeActiveStore{active: 0}
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	launches := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		s.cfg.Store = fakeActiveStore{active: launches}
		return fmt.Sprintf("run-%d", launches), nil
	}
	cfg := webhooks.Config{ID: "review-wh", TenantID: "review-t1", Provider: webhooks.ProviderGitHub}
	targets := []forgeLaunchTarget{
		{BotID: "review-a", IdemKey: "review-a", Vars: map[string]string{}},
		{BotID: "review-b", IdemKey: "review-b", Vars: map[string]string{}},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://local/webhook", nil).WithContext(ctx)
	s.acceptWebhookTargets(ctx, rec, req, cfg, webhookEventMeta{}, targets, "hash", "ip")
	if launches != 1 {
		t.Fatalf("fanout produced %d runs with MaxConcurrentRuns=1: %s", launches, rec.Body.String())
	}
}

// Removing the attempt ceiling or the backoff cap makes a toxic delivery loop
// forever or schedule arbitrarily far ahead instead of naming its exhaustion.
func TestWebhookDurability_RetryBudgetAndBackoff(t *testing.T) {
	t.Setenv("ITERION_WEBHOOK_MAX_ATTEMPTS", "10")
	s := newWebhookTestServer(t)
	_, d := failedDurableWebhook(t, s)
	for attempts := 1; attempts < 10; attempts++ {
		if d.NextAttemptAt == nil {
			t.Fatalf("attempt %d has no retry: %+v", attempts, d)
		}
		delay := d.NextAttemptAt.Sub(*d.FailedAt)
		if delay > webhookRetryMax || delay < webhookRetryBase {
			t.Fatalf("unbounded backoff: %s", delay)
		}
		s.sweepWebhookRetries(context.Background(), *d.NextAttemptAt)
		d, _ = s.webhookDeliveries.Get(context.Background(), d.ID)
	}
	if d.Status != webhooks.StatusLaunchExhausted || d.Attempts != 10 || d.NextAttemptAt != nil {
		t.Fatalf("budget not terminal: %+v", d)
	}
	s.sweepWebhookRetries(context.Background(), time.Now().Add(24*time.Hour))
	got, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	if got.Attempts != 10 {
		t.Fatal("exhausted delivery retried")
	}
}

// Removing RetryObsolete lets saved failures revive closed or superseded work.
func TestWebhookDurability_RetryMustNotReviveClosedPR(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, d := failedDurableWebhook(t, s)
	secret := "review-secret"
	cfg.HMACSecretSealed, _ = webhooks.SealHMACSecret(s.sealer, cfg.ID, secret)
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	runs := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		runs++
		return "revived-run", nil
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, ghReq(ghCtx(cfg), ghClosedPR, prforge.EventHeaderPullRequest, secret))
	if rec.Code != 200 {
		t.Fatalf("close=%d %s", rec.Code, rec.Body)
	}
	s.sweepWebhookRetries(context.Background(), d.NextAttemptAt.Add(time.Second))
	row, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	if runs != 0 {
		t.Fatalf("closed PR revived: runs=%d row=%+v", runs, row)
	}
}
func TestWebhookDurability_RetryMustNotCancelNewerHead(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, d := failedDurableWebhook(t, s)
	cfg.Overlap = "supersede"
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Version++
	secret := "review-secret"
	cfg.HMACSecretSealed, _ = webhooks.SealHMACSecret(s.sealer, cfg.ID, secret)
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var cancelled []string
	s.webhookCancelRun = func(id string) error { cancelled = append(cancelled, id); return nil }
	runs := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		runs++
		if runs == 1 {
			return "newer-head-run", nil
		}
		return "old-head-retry", nil
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, ghReq(ghCtx(cfg), strings.ReplaceAll(ghOpenPR, "abc123", "newerSHA"), prforge.EventHeaderPullRequest, secret))
	if runs != 1 {
		t.Fatalf("new head did not launch: code=%d %s", rec.Code, rec.Body)
	}
	s.sweepWebhookRetries(context.Background(), d.NextAttemptAt.Add(time.Second))
	row, _ := s.webhookDeliveries.Get(context.Background(), d.ID)
	if len(cancelled) > 0 {
		t.Fatalf("old head retry cancelled new review: cancelled=%v old row=%+v", cancelled, row)
	}
}

// Requiring RunID in RetryObsolete lets the old head race an already claimed head.
func TestWebhookDurability_RetryRespectsNewerClaim(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, d := failedDurableWebhook(t, s)
	cfg.Overlap = "supersede"
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var cancelled []string
	s.webhookCancelRun = func(id string) error { cancelled = append(cancelled, id); return nil }
	var launched []string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		launched = append(launched, vars["head_sha"])
		return "run-" + vars["head_sha"], nil
	}
	meta := webhookEventMeta{Kind: d.EventKind, Action: d.EventAction, ProjectPath: d.ProjectPath, SubjectID: d.SubjectID, SubjectSHA: "newerSHA"}
	out, launch := s.prepareWebhookTarget(ghCtx(cfg), nil, cfg, meta, forgeLaunchTarget{durableRetry: true, BotID: d.BotID, IdemKey: "newer-head-claimed", Vars: map[string]string{"head_sha": "newerSHA"}}, "hash", "ip")
	if launch == nil || out.Status != webhooks.StatusAccepted {
		t.Fatalf("new claim: %+v", out)
	}
	s.sweepWebhookRetries(context.Background(), d.NextAttemptAt.Add(time.Second))
	launch(ghCtx(cfg))
	if len(launched) != 1 || launched[0] != "newerSHA" {
		t.Fatalf("older retry races admitted current head: launched=%v cancelled=%v", launched, cancelled)
	}
}
