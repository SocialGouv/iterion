package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// emitForgeTriggerEvent publishes a SourceForge trigger.Event onto the trigger
// bus after an inbound forge webhook has launched a run. This puts git-forge
// events on the same spine as board/run/schedule sources — uniform
// observability and a foundation for forge→run chaining — WITHOUT changing the
// launch authority: the event carries PayloadLaunchedRunID, so the evaluator
// treats it as observational and never re-launches (no double-launch). The
// forge cutover (the spine becomes the launcher, the inline path retired
// behind a parity flag) is the follow-on that stops setting that marker.
//
// No-op when the trigger spine isn't wired (no native tracker / no
// TriggerStore) — forge webhooks keep working exactly as before.
func (s *Server) emitForgeTriggerEvent(ctx context.Context, cfg webhooks.Config, meta webhookEventMeta, botID string, vars map[string]string, repoURL, repoRef, runID string) {
	if s.triggerCoord == nil {
		return
	}
	bus := s.triggerCoord.Bus()
	if bus == nil {
		return
	}
	payload := map[string]any{
		trigger.PayloadLaunchedRunID: runID,
		"bot_id":                     botID,
		"provider":                   string(cfg.Provider),
		"repo_url":                   repoURL,
	}
	if len(vars) > 0 {
		vm := make(map[string]any, len(vars))
		for k, v := range vars {
			vm[k] = v
		}
		payload[trigger.PayloadVars] = vm
	}
	ev := trigger.Event{
		ID:       "forge:" + string(cfg.Provider) + ":" + cfg.TenantID + ":" + meta.SubjectID + ":" + runID,
		Source:   trigger.SourceForge,
		Kind:     meta.Kind,
		Action:   meta.Action,
		TenantID: cfg.TenantID,
		Repo:     meta.ProjectPath,
		Actor:    meta.SenderHandle,
		Subject: trigger.Subject{
			Type: meta.Kind,
			ID:   meta.SubjectID,
			URL:  meta.SubjectURL,
			SHA:  meta.SubjectSHA,
			Ref:  repoRef,
		},
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	_ = bus.Publish(ctx, ev)
}
