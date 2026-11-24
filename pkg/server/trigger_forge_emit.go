package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// forgeTriggerPayload is the event payload mirroring one inbound webhook's
// launch onto the trigger spine: the launched run's id, the bot, the provider,
// the repo, and the launch vars MINUS the ones the server minted for that run.
//
// The grant the server mints at launch (store.ServerMintedLaunchVars: the
// forge publish bearer and the endpoints it is presented to) is for the run
// and its own record alone. The spine fans the event out to every subscription
// that matches it — in-process locally, NATS in cloud — so copying the bearer
// here would hand a live credential to readers the launch never named.
func forgeTriggerPayload(runID, botID, provider, repoURL string, vars map[string]string) map[string]any {
	payload := map[string]any{
		trigger.PayloadLaunchedRunID: runID,
		"bot_id":                     botID,
		"provider":                   provider,
		"repo_url":                   repoURL,
	}
	if carried := store.DropServerMintedVars(vars); len(carried) > 0 {
		vm := make(map[string]any, len(carried))
		for k, v := range carried {
			vm[k] = v
		}
		payload[trigger.PayloadVars] = vm
	}
	return payload
}

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
	payload := forgeTriggerPayload(runID, botID, string(cfg.Provider), repoURL, vars)
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
