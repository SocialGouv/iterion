package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// adminEraseUserResponse answers POST /api/admin/users/{id}/erase: the row
// as it stands, and what this call removed — zeros when the account was
// erased already.
type adminEraseUserResponse struct {
	User UserView `json:"user"`
	// AlreadyErased reports the account was erased before this call, which
	// swept what an interrupted erasure may have left and changed nothing
	// else.
	AlreadyErased               bool  `json:"already_erased"`
	SessionsDeleted             int64 `json:"sessions_deleted"`
	PersonalAccessTokensRevoked int64 `json:"personal_access_tokens_revoked"`
	SSOLinksRemoved             int64 `json:"sso_links_removed"`
	MembershipsRemoved          int64 `json:"memberships_removed"`
	OrgMembershipsRemoved       int64 `json:"org_memberships_removed"`
	InvitationsRemoved          int64 `json:"invitations_removed"`
	PushSubscriptionsRemoved    int64 `json:"push_subscriptions_removed"`
	// OAuthCredentialsRemoved counts the person's own LLM subscription
	// credentials (forfaits) deleted — never an org's or the platform's.
	OAuthCredentialsRemoved int64 `json:"oauth_credentials_removed"`
	// PoolPledgesRemoved counts the person's credential-pool contributions
	// withdrawn.
	PoolPledgesRemoved int64 `json:"pool_pledges_removed"`
	// APIKeysRemoved and SecretsRemoved count the person's user-scoped
	// API keys and generic secrets deleted, in every team; team-wide ones
	// stay with the team.
	APIKeysRemoved int64 `json:"api_keys_removed"`
	SecretsRemoved int64 `json:"secrets_removed"`
	// RunNotesReattributed counts the run notes whose author — the
	// address a note defaults to — now names the account's id.
	RunNotesReattributed int64 `json:"run_notes_reattributed"`
	// ConfigShareDeliveriesScrubbed counts the config-share audit rows of
	// the person's config-editor sessions stripped of IP and user agent.
	ConfigShareDeliveriesScrubbed int64    `json:"config_share_deliveries_scrubbed"`
	AuditEventsScrubbed           int64    `json:"audit_events_scrubbed"`
	PersonalOrgsMarkedForPurge    []string `json:"personal_orgs_marked_for_purge,omitempty"`
}

// handleAdminEraseUser erases an account by anonymization (GDPR art. 17),
// super-admin only — docs/adr: "identity data lifecycle". The identity half
// is auth.Service.EraseUser (row, links, memberships, invitations, sessions,
// personal org); this handler hands it, as the step that runs while the row
// still carries its address, what lives in the server's other stores
// (eraseAccountHoldings) — and writes the one audit line of the act, under
// the admin's identity. Every step is idempotent: erasing twice answers 200
// with already_erased, and an erasure a failure interrupted (answered 500
// with the step named) completes when the request is repeated; the line is
// written as soon as the account carries the erased status, marked
// incomplete when the request fails past it. The caller's own account is
// refused: an erasure is a request another person acts on.
func (s *Server) handleAdminEraseUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if caller, _ := auth.FromContext(r.Context()); caller.UserID == id {
		s.httpErrorCode(w, r, http.StatusUnprocessableEntity, "self_erasure",
			"refusing to erase your own account — another super-admin erases it")
		return
	}
	var out adminEraseUserResponse
	res, err := s.authSvc.EraseUser(r.Context(), id, auth.EraseOptions{
		PersonalOrgGrace: orgDeletionGrace,
		BeforeAnonymize: func(ctx context.Context, u identity.User) error {
			return s.eraseAccountHoldings(ctx, u, &out)
		},
	})
	out.User = s.toUserView(res.User)
	out.AlreadyErased = res.AlreadyErased
	out.SessionsDeleted = res.SessionsDeleted
	out.SSOLinksRemoved = res.SSOLinksRemoved
	out.MembershipsRemoved = res.MembershipsRemoved
	out.OrgMembershipsRemoved = res.OrgMembershipsRemoved
	out.InvitationsRemoved = res.InvitationsRemoved
	out.PersonalOrgsMarkedForPurge = res.PersonalOrgsMarkedGone
	// A last-owner refusal answered once the account was marked — an
	// ownership granted during the erasure, refused by the sweep's recount —
	// still leaves the account erased: its line is written, incomplete.
	if res.Marked {
		if aerr := s.auditErasure(r, id, out, err != nil, res.User.ID != ""); aerr != nil {
			// The line names the account to an erasure's replay after a
			// restoration: an erasure whose line is lost is reported, and
			// repeating the request (idempotent) writes it.
			err = errors.Join(err, fmt.Errorf("the user.erased audit line was not written: %w", aerr))
		}
	}
	switch {
	case errors.Is(err, auth.ErrLastOrgOwner):
		s.httpErrorCode(w, r, http.StatusConflict, "last_org_owner", "%s", err.Error())
		return
	case errors.Is(err, identity.ErrNotFound):
		httpError(w, http.StatusNotFound, "%s", err.Error())
		return
	case err != nil:
		s.identityFailure(w, r, fmt.Errorf("%w — the erasure is incomplete: repeat it", err))
		return
	}
	writeJSON(w, out)
}

// auditErasure writes the `user.erased` platform line: the counts of what
// the call removed, and — for a request that failed once the account
// carried the erased status — incomplete, with whether the row had lost its
// address yet. The line is what names the account to an erasure's replay
// after a restoration, so it is written for an incomplete act too, and
// synchronously: its store's error is the caller's to report. The insert
// is detached from the request, under the insert bound: a client gone once
// the account carries the erased status takes nothing of the line with it.
func (s *Server) auditErasure(r *http.Request, id string, out adminEraseUserResponse, incomplete, anonymized bool) error {
	meta := map[string]any{
		"already_erased":                   out.AlreadyErased,
		"sessions_deleted":                 out.SessionsDeleted,
		"personal_access_tokens_revoked":   out.PersonalAccessTokensRevoked,
		"sso_links_removed":                out.SSOLinksRemoved,
		"memberships_removed":              out.MembershipsRemoved,
		"org_memberships_removed":          out.OrgMembershipsRemoved,
		"invitations_removed":              out.InvitationsRemoved,
		"push_subscriptions_removed":       out.PushSubscriptionsRemoved,
		"oauth_credentials_removed":        out.OAuthCredentialsRemoved,
		"pool_pledges_removed":             out.PoolPledgesRemoved,
		"api_keys_removed":                 out.APIKeysRemoved,
		"secrets_removed":                  out.SecretsRemoved,
		"run_notes_reattributed":           out.RunNotesReattributed,
		"config_share_deliveries_scrubbed": out.ConfigShareDeliveriesScrubbed,
		"audit_events_scrubbed":            out.AuditEventsScrubbed,
		"personal_orgs_marked_for_purge":   out.PersonalOrgsMarkedForPurge,
	}
	if incomplete {
		meta["incomplete"] = true
		meta["anonymized"] = anonymized
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.auditInsertBound())
	defer cancel()
	return s.auditInsertNow(ctx, s.auditRequestEvent(r, audit.ScopePlatform, "", "user.erased", "user", id, meta))
}

// eraseAccountHoldings ends what the account u holds in the server's stores
// beside the identity and session ones, counting into out — the
// BeforeAnonymize half of an erasure, run while u still carries its
// address. Every step is idempotent. In order:
//   - every personal access token revoked (rows kept, hash only);
//   - every credential-pool pledge withdrawn, then every LLM subscription
//     credential the person holds under their own owner key deleted — the
//     forfait a resumed run of theirs would otherwise still be sealed with,
//     and the refresh worker still renew;
//   - every user-scoped API key and generic secret deleted, in every team;
//   - every push subscription deleted;
//   - the run notes whose author is the address reattributed to the id;
//   - the config-share deliveries of the person's config-editor sessions
//     stripped of IP and user agent;
//   - the audit rows naming the account scrubbed (audit.Store.ScrubActor),
//     the refusals by address found by their digest under every signing
//     key (auth.Service.SubjectDigests, ADR-157) —
//     before the line of the act is written, which keeps the admin's own IP
//     and user agent.
//
// What stays keyed by the id is a pseudonym: notification and model
// preferences (removed with their organization's purge), memory spaces.
func (s *Server) eraseAccountHoldings(ctx context.Context, u identity.User, out *adminEraseUserResponse) error {
	var err error
	if out.PersonalAccessTokensRevoked, err = s.revokeAccountPATs(ctx, u.ID); err != nil {
		return fmt.Errorf("revoke its personal access tokens: %w", err)
	}
	if s.credPoolPledges != nil {
		pledges, err := s.credPoolPledges.ListByUser(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("list its credential-pool pledges: %w", err)
		}
		for _, p := range pledges {
			if err := s.credPoolPledges.Delete(ctx, p.ID); err != nil && !errors.Is(err, credpool.ErrNotFound) {
				return fmt.Errorf("withdraw its credential-pool pledge %s: %w", p.ID, err)
			}
			out.PoolPledgesRemoved++
		}
	}
	if s.oauthStore != nil {
		recs, err := s.oauthStore.ListByUser(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("list its subscription credentials: %w", err)
		}
		for _, rec := range recs {
			if rec.UserID != u.ID {
				return fmt.Errorf("the subscription credentials listed for %s include %s, owned by %s: refusing to delete what is not the account's", u.ID, rec.ID, rec.UserID)
			}
			if err := s.oauthStore.Delete(ctx, rec.ID); err != nil && !errors.Is(err, secrets.ErrOAuthNotFound) {
				return fmt.Errorf("delete its %s subscription credential %s: %w", rec.Kind, rec.ID, err)
			}
			out.OAuthCredentialsRemoved++
		}
	}
	if s.apiKeys != nil {
		if out.APIKeysRemoved, err = s.apiKeys.DeleteByScopeUser(ctx, u.ID); err != nil {
			return fmt.Errorf("delete its personal API keys: %w", err)
		}
	}
	if s.genericSecrets != nil {
		if out.SecretsRemoved, err = s.genericSecrets.DeleteByScopeUser(ctx, u.ID); err != nil {
			return fmt.Errorf("delete its personal secrets: %w", err)
		}
	}
	if s.cfg.PushSubscriptions != nil {
		if out.PushSubscriptionsRemoved, err = s.cfg.PushSubscriptions.DeleteByUser(ctx, u.ID); err != nil {
			return fmt.Errorf("delete its push subscriptions: %w", err)
		}
	}
	if s.runs != nil && !identity.IsErasedEmail(u.Email) {
		if out.RunNotesReattributed, err = s.runs.ReattributeRunNotesCtx(ctx, u.Email, u.ID); err != nil {
			return fmt.Errorf("reattribute its run notes: %w", err)
		}
	}
	if s.configShares != nil {
		if out.ConfigShareDeliveriesScrubbed, err = s.configShares.ScrubActor(ctx, configshare.ActorUser(u.ID)); err != nil {
			return fmt.Errorf("scrub its config-share deliveries: %w", err)
		}
	}
	if s.auditStore != nil {
		digests := s.authSvc.SubjectDigests(u.Email)
		scrubbed := time.Now()
		if out.AuditEventsScrubbed, err = s.auditStore.ScrubActor(ctx, u.ID, u.Email, digests); err != nil {
			return fmt.Errorf("scrub its audit rows: %w", err)
		}
		// Audit lines are inserted in the background, on every replica: one
		// naming the person queued before the scrub may land after it, keyed
		// by an invitation id and an address the row is about to lose. Each
		// lands within the insert bound of being queued, or never — scrub
		// again once that bound has passed, while the address is still known.
		if err = s.waitOutAuditInserts(ctx, scrubbed); err != nil {
			return fmt.Errorf("scrub its audit rows: %w", err)
		}
		n, err := s.auditStore.ScrubActor(ctx, u.ID, u.Email, digests)
		if err != nil {
			return fmt.Errorf("scrub its audit rows again past the insert bound: %w", err)
		}
		out.AuditEventsScrubbed += n
	}
	return nil
}

// grantRefusal is the answer every placement of an EXISTING account gives
// one no grant may land on: a disabled account is re-enabled first, an
// erased one is granted nothing ever again. Empty when the account may be
// granted. what names the grant ("an org", "a team").
func grantRefusal(u identity.User, what string) string {
	switch u.Status {
	case identity.UserStatusErased:
		return fmt.Sprintf("user %s is erased — an erased account is granted nothing", u.ID)
	case identity.UserStatusDisabled:
		return fmt.Sprintf("user %s is disabled — re-enable the account before granting it %s", u.Email, what)
	}
	return ""
}
