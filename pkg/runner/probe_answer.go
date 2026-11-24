package runner

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner's half of a credential window's probe (pkg/server's
// sweepCredentialProbes): the attempt that follows a pull answers whether the
// wall the pull set aside fell, by recording the instant a call on one of the
// wall's credentials was served (store.RunRetryState.ProbeAnsweredAt). The
// sweeper releases the window's other waits on that record and on nothing
// else: a run's status and the age of its pull say nothing about the wall — a
// canary queued for an hour, served on another credential or held behind a
// cap runs past the marker's TTL without having asked it anything.

// probeWatch is what an attempt that follows a pull watches for: a served
// call placed on one of the wall's credentials.
type probeWatch struct {
	// wall is the wall's credentials (store.RunRetryState.WallCredentials).
	wall map[string]bool
	// answer records the answer at the instant given. Called once, off the
	// emitter's lock.
	answer func(at time.Time)
	// done is nil until the answer fires, closed once it is written.
	done chan struct{}
}

// watchProbeAnswer arms the emitter to answer the probe this attempt follows:
// the first served call it places on one of wall's credentials calls answer.
// Called before the attempt runs; a nil or empty wall arms nothing.
func (m *metricsEmitter) watchProbeAnswer(wall []string, answer func(at time.Time)) {
	if len(wall) == 0 || answer == nil {
		return
	}
	w := &probeWatch{wall: make(map[string]bool, len(wall)), answer: answer}
	for _, fp := range wall {
		if fp != "" {
			w.wall[fp] = true
		}
	}
	m.mu.Lock()
	m.probe = w
	m.mu.Unlock()
}

// noteProbeAnswerLocked answers the probe when a served call — one that spent
// tokens or money — was placed on one of the wall's credentials the way a
// chain verdict is (routeCredentialLocked): its own credential keys (ADR-161
// decision 8), else the credential plan's row; never a call that rode the
// environment. A call the runner cannot place answers nothing. Called under
// m.mu, by the observation paths that are served calls: a claw step, a
// finished delegation's own route.
func (m *metricsEmitter) noteProbeAnswerLocked(nodeID string, route routeKey, provider string, cost float64, tokens int64) {
	w := m.probe
	if w == nil || w.done != nil || (cost <= 0 && tokens <= 0) {
		return
	}
	served, placed := m.routeCredentialLocked(nodeID, route, provider)
	if !placed || !w.wall[served] {
		return
	}
	done := make(chan struct{})
	w.done = done
	at := time.Now().UTC()
	go func() {
		defer close(done)
		w.answer(at)
	}()
}

// waitProbeAnswer returns once the answer this attempt recorded, if any, has
// been written.
func (m *metricsEmitter) waitProbeAnswer() {
	var done chan struct{}
	m.mu.Lock()
	if m.probe != nil {
		done = m.probe.done
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

// armProbeAnswer arms the attempt's emitter when the run it executes follows
// a pull of its wait whose wall is still ahead (RunRetryState
// .AnnouncedProbeFrom) — a credential window's canary, or a run released on
// its answer — and the store records answers. The write goes through
// store.RunProbeAnswerStore, detached from the run's context: an answer the
// attempt got is the window's even when the attempt is cancelled next.
func (r *Runner) armProbeAnswer(msg *queue.RunMessage, usage *metricsEmitter, run *store.Run, logger *iterlog.Logger) {
	if usage == nil || run == nil {
		return
	}
	rs := run.RetryState
	if rs.AnnouncedProbeFrom(time.Now().UTC()) == nil || len(rs.WallCredentials) == 0 {
		return
	}
	answerer := store.AsRunProbeAnswerStore(r.cfg.Store)
	if answerer == nil {
		return
	}
	usage.watchProbeAnswer(rs.WallCredentials, func(at time.Time) {
		ctx, cancel := context.WithTimeout(store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID), usageRetryStoreTimeout)
		defer cancel()
		answered, err := answerer.AnswerRunProbe(ctx, msg.RunID, at)
		switch {
		case err != nil:
			logger.Warn("runner: run %s: a call on its wall's credentials was served at %s, and the probe's answer could not be recorded (%v) — its credential window releases nobody on it and is probed again",
				msg.RunID, at.Format(time.RFC3339), err)
		case answered:
			logger.Info("runner: run %s: a call on its wall's credentials was served at %s — the probe of its credential window got through",
				msg.RunID, at.Format(time.RFC3339))
		}
	})
}
