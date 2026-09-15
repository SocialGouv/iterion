package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/modelcatalog"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// probeCloudRunPresence lists the launch-tier credentials a cloud run for
// this identity would receive: tenant BYOK, user/org OAuth-forfait, then
// platform. Presence only — it never opens a sealed blob, never returns a
// value, and never consults the server process environment.
//
// The mutualised pool is not probed: a grant is opportunistic and proving
// it would mean acquiring one. Unproven providers stay reachability
// "unknown" rather than a false unreachable.
func (s *Server) probeCloudRunPresence(ctx context.Context, id auth.Identity) modelcatalog.CloudPresence {
	out := modelcatalog.CloudPresence{
		ProviderSources: map[string]string{},
	}
	if id.TeamID == "" {
		return out
	}
	now := time.Now().UTC()
	s.collectBYOKPresence(ctx, id.TeamID, id.UserID, now, out.ProviderSources)
	s.collectOAuthPresence(ctx, id.TeamID, id.UserID, &out)
	s.collectPlatformPresence(ctx, now, &out)
	return out
}

func (s *Server) collectBYOKPresence(ctx context.Context, teamID, userID string, now time.Time, into map[string]string) {
	if s.apiKeys == nil {
		return
	}
	tctx := store.WithTenant(ctx, teamID)
	keys, err := s.apiKeys.ListByTeam(tctx, teamID, userID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("models: list tenant BYOK: %v", err)
		}
		return
	}
	for _, k := range keys {
		noteProviderPresence(into, k, now)
	}
}

func (s *Server) collectOAuthPresence(ctx context.Context, teamID, userID string, out *modelcatalog.CloudPresence) {
	if s.oauthStore == nil {
		return
	}
	noteOAuth := func(ownerKey string) {
		if ownerKey == "" {
			return
		}
		recs, err := s.oauthStore.ListByUser(ctx, ownerKey)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("models: list oauth %s: %v", ownerKey, err)
			}
			return
		}
		for _, rec := range recs {
			if len(rec.SealedPayload) == 0 {
				continue
			}
			switch rec.Kind {
			case secrets.OAuthKindClaudeCode:
				out.ClaudeCodeOAuth = true
			case secrets.OAuthKindCodex:
				out.CodexOAuth = true
			}
		}
	}
	noteOAuth(userID)
	noteOAuth(secrets.OrgOwnerKey(teamID))
}

func (s *Server) collectPlatformPresence(ctx context.Context, now time.Time, out *modelcatalog.CloudPresence) {
	if s.apiKeys != nil {
		pctx := store.WithTenant(ctx, secrets.PlatformTenantID)
		keys, err := s.apiKeys.ListByTeam(pctx, secrets.PlatformTenantID, "")
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("models: list platform BYOK: %v", err)
			}
		} else {
			for _, k := range keys {
				name := modelcatalog.DetectProviderName(k.Provider)
				if _, already := out.ProviderSources[name]; already {
					continue
				}
				noteProviderPresence(out.ProviderSources, k, now)
			}
		}
	}
	if s.oauthStore == nil {
		return
	}
	recs, err := s.oauthStore.ListByUser(ctx, secrets.PlatformOwnerKey)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("models: list platform oauth: %v", err)
		}
		return
	}
	for _, rec := range recs {
		if len(rec.SealedPayload) == 0 {
			continue
		}
		switch rec.Kind {
		case secrets.OAuthKindClaudeCode:
			out.ClaudeCodeOAuth = true
		case secrets.OAuthKindCodex:
			out.CodexOAuth = true
		}
	}
}

func noteProviderPresence(into map[string]string, k secrets.ApiKey, now time.Time) {
	if len(k.SealedSecret) == 0 {
		return
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return
	}
	if !k.Provider.Valid() {
		return
	}
	name := modelcatalog.DetectProviderName(k.Provider)
	if name == "" {
		return
	}
	if _, exists := into[name]; exists {
		return
	}
	into[name] = modelcatalog.ProviderSourceLabel(k.Provider)
}
