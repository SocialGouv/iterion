package server

import (
	"context"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
)

// Addr returns the actual bound address (host:port) once ListenAndServe has
// successfully created its listener. It blocks until the listener is ready or
// the context is cancelled. Used by the desktop host when Port=0 was passed
// and the OS picks the port.
func (s *Server) Addr() string {
	<-s.addrReady
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		// Even on error, signal Addr() so callers don't block forever.
		close(s.addrReady)
		return err
	}
	s.listener = ln
	// Reflect the OS-chosen port back into the config so logging and the
	// origin allowlist see the real port (Port=0 mode).
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.cfg.Port = tcpAddr.Port
	}
	close(s.addrReady)
	// A process that started below the durable rollout high-water mark is
	// deliberately kept alive so /healthz and /readyz explain why the pod is
	// parked. It must not, however, start any of the background coordinators
	// below: several of them win one-shot CAS claims before launching a run,
	// and the queue fence would then reject that launch after the claim was
	// irreversibly consumed. Serve diagnostics only; readiness keeps this pod
	// out of the Service and the queue layer remains the publication backstop.
	if s.cfg.Superseded {
		s.logger.Error("server: superseded epoch — background workers disabled; serving diagnostics only")
		return s.server.Serve(ln)
	}
	// Sweep abandoned upload staging dirs in the background. Without
	// this, attachments uploaded for runs that never launched (operator
	// closed the modal, browser crashed mid-upload, etc.) accumulate
	// under <store>/uploads/ until the disk fills. The reaper itself
	// is best-effort — it walks the staging root, deletes dirs older
	// than uploadStagingTTL, and stops when s.shutdown closes.
	if s.runs != nil {
		errtrack.Go("server.stagedUploadReaper", s.runStagedUploadReaper)
	}
	// Studio's built-in pipeline launcher: start ready tickets (dragged into
	// Todo) when a concurrency slot frees. Only when no external dispatcher
	// owns the board (which would otherwise race to claim the same tickets).
	if s.pipelineAdmissionEnabled() {
		errtrack.Go("server.pipelineAdmissionLoop", s.runPipelineAdmissionLoop)
	}
	// Teach the run service's concurrency gate about slots held open by
	// pipelines that died and need a human. Wired regardless of the admission
	// loop: the FIFO drain and every non-board launch path go through the same
	// gate, and they run even when an external dispatcher owns the board.
	s.wirePipelineReservations(s.runs)
	// MVP3b: fan native-board issue-state transitions out to runs that
	// subscribed (Run.WatchedIssueIDs). No-op when no native tracker is
	// wired or the events tail can't start.
	if s.runs != nil && s.cfg.NativeTrackerStore != nil {
		s.watchCoord = startWatchCoordinator(s.runs, s.cfg.NativeTrackerStore, s.logger)
	}
	// Event-driven trigger spine: tail board transitions → match stored
	// subscriptions → promote matching cards (the dispatcher then claims
	// them). No-op when no TriggerStore is wired. The dispatcher Manager
	// doubles as the nudger so a promoted card dispatches now, not at poll.
	if s.cfg.NativeTrackerStore != nil && s.cfg.TriggerStore != nil {
		var nudger trigger.Nudger
		if s.cfg.Dispatcher != nil {
			nudger = s.cfg.Dispatcher
		}
		var launcher trigger.Launcher
		if s.runs != nil {
			launcher = newServiceLauncher(s.runs, s.logger, s.resolveRunRetryPolicy, s.resolveBotSource)
		}
		s.triggerCoord = StartTriggerCoordinator(s.cfg.NativeTrackerStore, s.cfg.TriggerStore, nudger, launcher, s.scheduleGate(), s.cfg.EventsBus, s.logger)
	}
	// Cloud counterpart: the multi-tenant Mongo board spine (board_events
	// poll-tail → NATS bus → evaluator with the atomic consume effect).
	// Distinct from the local branch above — cloud wires no
	// NativeTrackerStore; the board dispatcher below stays the promote
	// path's launch authority and 5s safety net.
	if s.cfg.NativeTrackerStore == nil && s.cfg.CloudBoardCoordinator != nil && s.cfg.TriggerStore != nil && s.runs != nil {
		s.cloudTriggerCoord = StartCloudTriggerCoordinator(
			s.cfg.CloudBoardCoordinator, s.cfg.TriggerStore,
			newServiceLauncher(s.runs, s.logger, s.resolveRunRetryPolicy, s.resolveBotSource),
			s.cfg.EventsBus, s.logger)
	}
	// Wire the run-completion source onto the process's single event spine
	// (the injected EventsBus, which the trigger coordinator also rides
	// when active; its own InProcBus otherwise) so every consumer —
	// trigger evaluator, usernotify — sees the same run-outcome stream.
	if s.runs != nil {
		if s.cfg.EventsBus != nil {
			s.runs.SetEventPublisher(s.cfg.EventsBus)
		} else if s.triggerCoord != nil {
			s.runs.SetEventPublisher(s.triggerCoord.Bus())
		}
	}
	s.startUserNotify()
	s.startOperatorAlerts()
	s.startGateReconciler()
	s.startGateAutofix()
	s.startOutcomeRouter()
	// Sweep abandoned OIDC PendingAuth entries — a user who clicks
	// "Sign in with Google" then closes the tab never returns to
	// trigger the lazy eviction inside Take, so without this the
	// in-memory store grows unbounded under brute-force attempts
	// or distracted users.
	if mss, ok := s.oidcStates.(*oidc.MemoryStateStore); ok {
		errtrack.Go("server.oidcStateSweeper", func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			mss.StartSweeper(ctx, 0) // 0 = use store TTL as interval
		})
	}
	// Forge OAuth token refresh: keep oauth_app connection tokens (and their
	// managed forge_token secrets) fresh so bot runs never read an expired
	// credential. PAT connections are skipped by the worker. No-op when the
	// forge orchestrator isn't wired (local mode).
	if s.forgeOrchestrator != nil {
		worker := &forge.RefreshWorker{
			Connections:    s.forgeConnections,
			Secrets:        s.genericSecrets,
			Sealer:         s.sealer,
			RefresherFor:   s.forgeRefresherFor,
			SecurityMinter: s.forgeSecurityTokenMinter,
			Lead:           5 * time.Minute,
		}
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if _, err := worker.RunOnce(ctx); err != nil && s.logger != nil {
						s.logger.Warn("forge token refresh: %v", err)
					}
				}
			}
		}()
	}
	// OAuth-forfait token refresh: proactively rotate Claude Code (and
	// Codex) subscription access tokens before they expire so neither an
	// interactive run nor an automated (webhook/dispatcher/cron) run ever
	// reads a stale credential. Covers personal AND org-scoped records.
	// No-op without a store/sealer or any configured client id.
	if s.oauthStore != nil && s.sealer != nil && (s.cfg.AnthropicOAuthClientID != "" || s.cfg.CodexOAuthClientID != "") {
		worker := &secrets.OAuthRefreshWorker{
			Store:             s.oauthStore,
			Sealer:            s.sealer,
			HTTP:              s.httpClient,
			AnthropicClientID: s.cfg.AnthropicOAuthClientID,
			CodexClientID:     s.cfg.CodexOAuthClientID,
			Lead:              30 * time.Minute,
		}
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := worker.RunOnce(ctx); err != nil && s.logger != nil {
						s.logger.Warn("oauth-forfait refresh: %v", err)
					} else if n > 0 && s.logger != nil {
						s.logger.Info("oauth-forfait refresh: rotated %d token(s)", n)
					}
				}
			}
		}()
	}
	// Forge → board issue sync (cloud only): periodically mirror every
	// sync-enabled repo's forge issues onto its team board. Off unless a
	// cloud board + the integration store are wired. See board_forge.go.
	if s.cfg.CloudBoardFor != nil && s.forgeIntegrations != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runBoardSyncWorker(ctx, 5*time.Minute)
		}()
	}
	// Orphan-run sweeper (cloud only): flips queued/running rows whose
	// runner died without a terminal write to failed_resumable. Needs
	// both the Mongo store (stale scan capability) and the queue (KV
	// lease check) — silently absent otherwise (local mode).
	if lister, ok := s.cfg.Store.(staleRunLister); ok && s.queue != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runQueueSweeper(ctx, lister, s.queue)
		}()
	}
	// Abandoned-lease sweeper: gives a lending contributor back the
	// concurrency slot of a run whose pod died without reporting.
	if s.credPool != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runCredPoolSweeper(ctx)
		}()
	}
	// Retry sweeper (cloud only): resumes runs whose provider quota window
	// has reopened. Needs only the store — unlike the orphan sweeper it
	// asks no question of the queue, because the retry instant was decided
	// when the run failed. Multi-replica-safe via the store CAS.
	if lister, ok := s.cfg.Store.(retryDueLister); ok && s.runs != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runRetrySweeper(ctx, lister)
		}()
	} else if s.cfg.ScheduledBots != nil {
		// Cloud mode without the capability: a type assertion that quietly
		// fails here (a store wrapped in a decorator, say) means every
		// quota-blocked run stays parked forever with nothing to bring it
		// back — and no other signal would say so. The local store not
		// implementing it is expected and stays silent.
		s.warnf("retry sweeper NOT started: the store does not implement ListRunsDueForRetry — " +
			"runs parked on a provider quota window will never resume on their own")
	}
	// Merge-gate sweeper (cloud only): the reconciliation net under the
	// reconciler's lossy outcome event. Same shape and cadence as the retry
	// sweeper next door, and needed for the same reason — a required check
	// nobody answers blocks a pull request indefinitely, and a dropped event
	// leaves no trace saying so.
	if lister, ok := s.cfg.Store.(gateSweepLister); ok && s.forgePublishTokens != nil && s.forgeConnections != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.runGateSweeper(ctx, lister)
		}()
	} else if s.cfg.ScheduledBots != nil && s.forgePublishTokens != nil {
		// Cloud mode with gating wired but no sweep: the event path is then the
		// SOLE trigger, and its misses are exactly the invisible ones.
		s.warnf("merge-gate sweeper NOT started: the store does not implement ListNotifiableRuns — " +
			"a review whose outcome event is dropped will leave its required check absent forever")
	}
	// Cloud scheduler: fire due cron-scheduled bots. Multi-replica-safe via the
	// store CAS (no leader election). Absent in local mode (ScheduledBots nil).
	if s.cfg.ScheduledBots != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			(&cloudsched.Ticker{
				Store:    s.cfg.ScheduledBots,
				Launch:   s.launchScheduledBot,
				Logger:   s.logger,
				Gate:     s.cloudScheduleGate,
				Audit:    s.cloudScheduleAudit,
				Interval: schedulerTickInterval(),
			}).Run(ctx)
		}()
	}
	// Org purge sweeper: nightly hard-purge of soft-deleted orgs past their
	// grace. Idempotent across replicas (no leader election needed).
	if s.cfg.OrgPurgeSweeper != nil {
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			s.cfg.OrgPurgeSweeper.Run(ctx)
		}()
	}
	// Cloud board dispatcher: claim + run eligible cards across all tenants.
	// Multi-replica-safe via the per-card Claim CAS (no leader election).
	if s.cfg.CloudBoardCoordinator != nil {
		marker := "board-dispatcher:" + uuid.NewString()
		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-s.shutdown
				cancel()
			}()
			d := newBoardDispatcher(s.cfg.CloudBoardCoordinator, s.processBoardCard, marker, 4, s.logger)
			// Parked-card sweep wiring: read a run's status tenant-scoped,
			// and clear the denormalized ⏸ badge when the sweep moves a card.
			d.statusFor = s.boardRunStatus
			d.clearBadge = func(tenant, id string) { s.setCardAwaitingInput(tenant, id, false) }
			// Fork-adoption sweep wiring: full run record + the issue's runs
			// (tenant-scoped) to resolve a finished fork, and the CloudBoardFor
			// seam to adopt it onto the stranded card.
			d.runFor = s.boardRun
			d.issueRuns = s.boardIssueRuns
			d.adoptRun = s.adoptCardRun
			d.run(ctx)
		}()
	}
	// Truthful URL in the log: if the operator chose a non-loopback bind we
	// print the actual address so they know the studio is exposed beyond the
	// local machine. Previously we always printed http://localhost:<port>
	// regardless of the bind interface.
	displayHost := s.cfg.Bind
	if displayHost == "127.0.0.1" || displayHost == "::1" || displayHost == "" {
		displayHost = "localhost"
	}
	s.logger.Info("Editor server listening on http://%s:%d", displayHost, s.cfg.Port)
	return s.server.Serve(ln)
}

// startUserNotify builds the usernotify dispatcher (web-push sink), attaches
// it to the event spine, and starts the reconciliation sweep. No-op when the
// feature is off. The dispatcher subscribes on the shared EventsBus (cloud
// NATSBus — queue-group delivery dedups across replicas) or, locally, on the
// trigger coordinator's in-proc bus.
func (s *Server) startUserNotify() {
	if !s.webPushEnabled() || s.runs == nil {
		return
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return
	}
	s.pushSink = webpush.NewSink(s.cfg.PushSubscriptions, webpush.SinkOptions{
		VAPIDPublicKey:  s.cfg.WebPushVAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.WebPushVAPIDPrivateKey,
		Subscriber:      s.cfg.WebPushSubscriber,
	}, s.logger)
	s.userNotify = usernotify.NewDispatcher(rs, s.cfg.NotificationPrefs, s.cfg.NotificationSent, s.cfg.PublicURL, s.logger, s.pushSink)

	bus := s.cfg.EventsBus
	if bus == nil && s.triggerCoord != nil {
		bus = s.triggerCoord.Bus()
	}
	if bus != nil {
		cancel, err := s.userNotify.Attach(bus)
		if err != nil {
			s.logger.Warn("server: usernotify bus subscribe failed (sweep-only delivery): %v", err)
		} else {
			s.userNotifyCancel = cancel
		}
	}

	if s.cfg.NotifiableRuns != nil {
		sweeper := usernotify.NewSweeper(s.userNotify, s.cfg.NotifiableRuns, s.logger)
		sweepCtx, cancelSweep := context.WithCancel(context.Background())
		go func() {
			<-s.shutdown
			cancelSweep()
		}()
		go sweeper.Start(sweepCtx)
	}
	s.logger.Info("server: user notifications enabled (web push)")
}

// startOperatorAlerts builds the operator-alert dispatcher — the cloud twin
// of the in-process alert Manager, which is fed by file tails and in-process
// engine observers only and therefore never sees a runner pod's failures
// (the 2026-08-31 five silent parked digests). Bus-fed with a sweep net,
// episodes deduped through the shared sent-notifications claim store.
func (s *Server) startOperatorAlerts() {
	if s.cfg.AlertsWebhookURL == "" || s.runs == nil {
		return
	}
	if s.cfg.NotificationSent == nil {
		// Without the claim store every replica AND the sweep would re-send
		// each episode. Loud refusal beats a spamming alert channel.
		s.logger.Warn("server: operator alerts configured but no episode-claim store wired — disabled (wire NotificationSent)")
		return
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return
	}
	var sinks []alert.Sink
	if wh := alert.NewWebhookSink(s.cfg.AlertsWebhookURL, s.logger); wh != nil {
		sinks = append(sinks, wh)
	}
	if tk := alert.NewTrackerSink(); tk != nil {
		sinks = append(sinks, tk)
	}
	d := &alert.OpsDispatcher{
		Runs:    rs,
		Claims:  s.cfg.NotificationSent,
		Sinks:   sinks,
		BaseURL: s.cfg.PublicURL,
		Logger:  s.logger,
	}
	s.opsAlerts = d
	bus := s.cfg.EventsBus
	if bus == nil && s.triggerCoord != nil {
		bus = s.triggerCoord.Bus()
	}
	if bus != nil {
		cancel, err := bus.Subscribe(alert.OpsSubscriberName, trigger.Matcher{
			Sources: []trigger.Source{trigger.SourceRun},
			Kinds:   []string{trigger.KindRunFailed},
		}, d.Handle)
		if err != nil {
			s.logger.Warn("server: operator-alert bus subscribe failed (sweep-only delivery): %v", err)
		} else {
			s.opsAlertsCancel = cancel
		}
	}
	if s.cfg.NotifiableRuns != nil {
		sweepCtx, cancelSweep := context.WithCancel(context.Background())
		go func() {
			<-s.shutdown
			cancelSweep()
		}()
		go d.RunOpsSweep(sweepCtx, s.cfg.NotifiableRuns)
	}
	s.logger.Info("server: operator alerts enabled (parked/failed runs → webhook)")
}

// drainBudgetShare is the fraction of the caller's shutdown deadline the
// run-console drain may consume; the remainder is reserved for the HTTP
// shutdown so in-flight requests keep a real window.
const drainBudgetShare = 2.0 / 3.0

// drainBudget derives a context holding drainBudgetShare of ctx's
// remaining deadline. An undeadlined ctx passes through unchanged
// (nothing to divide).
func drainBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(float64(remaining)*drainBudgetShare))
}

// ShutdownDelay reports the configured lame-duck window so an entrypoint
// can size its Shutdown context to cover it plus the teardown itself.
func (s *Server) ShutdownDelay() time.Duration { return s.cfg.ShutdownDelay }

// beginDrain opens the lame-duck window: /readyz starts answering 503
// while the listener is still accepting, then we wait ShutdownDelay for
// the endpoints controller to pull this pod out of the Service. Without
// that pause the socket closes while traffic is still being routed here,
// which is a connection-refused — a 502 for a studio user, a dropped
// delivery for a forge webhook.
//
// The wait is bounded by ctx, so a caller whose deadline is already tight
// skips ahead rather than sitting out the full delay. Note that a SECOND
// signal does NOT shorten it: both entrypoints derive the shutdown context
// from context.Background(), so once the exit sequence starts it runs to
// completion or to its own deadline.
func (s *Server) beginDrain(ctx context.Context) {
	s.draining.Store(true)
	if s.cfg.ShutdownDelay <= 0 {
		return
	}
	s.logger.Info("server: draining — /readyz now 503, waiting %s for endpoint removal", s.cfg.ShutdownDelay)
	timer := time.NewTimer(s.cfg.ShutdownDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Shutdown gracefully shuts down the server.
//
// Order matters. The lame-duck flip comes first (see beginDrain), then the
// run console service drains in-process workflow goroutines, and the
// HTTP-level shutdown drains in-flight requests last. The workflow drain
// precedes it so any cancel events reach the on-disk store before the file
// watcher stops broadcasting and clients drop.
//
// Drain (rather than Stop) is intentional: it flips each in-flight
// run to failed_resumable and emits EventRunInterrupted so the next
// boot can offer one-click resume and clients can distinguish
// shutdown-induced termination from user-initiated cancel.
func (s *Server) Shutdown(ctx context.Context) error {
	s.beginDrain(ctx)
	if s.runs != nil {
		// Sub-budget: Drain and the HTTP shutdown below share the caller's
		// deadline, so a drain that eats all of it would leave
		// server.Shutdown an already-expired context — cutting in-flight
		// requests at the exact moment we're trying to let them finish.
		drainCtx, cancel := drainBudget(ctx)
		s.runs.Drain(drainCtx)
		cancel()
	}
	// Stop the dispatcher before the HTTP server tears down so its
	// shutdown() path can release in-flight claims to a clean state.
	// Without this the daemon's SIGTERM (watchexec restart, operator
	// Ctrl+C) would orphan claimed-by-self tickets on disk and the
	// next dispatcher start would skip them (ListCandidates filters
	// Claimed=true) until the operator manually edited the JSON.
	// See ticket 012cb3a2 / 7221c7be.
	if s.cfg.Dispatcher != nil {
		// Shutdown (not Stop) so a server SIGTERM / Ctrl-C / watchexec
		// rebuild preserves the operator's last-known intent in
		// runtime.json. Stop persists `desired=stopped` — that's
		// operator-driven only.
		s.cfg.Dispatcher.Shutdown()
	}
	if s.watchCoord != nil {
		s.watchCoord.Close()
	}
	if s.cloudTriggerCoord != nil {
		s.cloudTriggerCoord.Close()
	}
	if s.triggerCoord != nil {
		s.triggerCoord.Close()
	}
	if s.opsAlertsCancel != nil {
		s.opsAlertsCancel()
	}
	if s.userNotifyCancel != nil {
		s.userNotifyCancel()
	}
	if s.gateAutofixCancel != nil {
		s.gateAutofixCancel()
		s.gateAutofixCancel = nil
	}
	if s.outcomeRouterCancel != nil {
		s.outcomeRouterCancel()
		s.outcomeRouterCancel = nil
	}
	if s.gateReconcileCancel != nil {
		s.gateReconcileCancel()
	}
	if s.watcher != nil {
		s.watcher.Stop()
	}
	s.hub.Stop()
	// Close shutdown last so background goroutines (upload reaper)
	// observe it after the drain has settled and HTTP handlers have
	// stopped accepting new requests.
	select {
	case <-s.shutdown:
		// already closed (idempotent shutdown)
	default:
		close(s.shutdown)
	}
	return s.server.Shutdown(ctx)
}
