package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// One account's file, for the super-admin console.
//
// It exists because the platform could not answer the first question an
// operator asks about an account that signs in and sees nothing: where does
// it come from, and what was it actually granted? Every fact below already
// lived in the store and none of it was reachable through the API — so the
// answer was a Mongo read, or a guess.
//
// The sharpest case it serves: a GitHub login admitted but outside the SSO
// org allow-list is provisioned as a submitter (pkg/auth/oidc_service.go) —
// active, no org, no team, NO PASSWORD. On this page that reads as a
// signature (an SSO link, no password, an empty roster) instead of looking
// like a broken deployment.
//
// The route itself is registered in auth_routes.go beside its three
// /api/admin/users siblings.

type adminUserOrgView struct {
	OrgID    string `json:"org_id"`
	OrgName  string `json:"org_name"`
	OrgSlug  string `json:"org_slug"`
	Role     string `json:"role"`
	Personal bool   `json:"personal,omitempty"`
	JoinedAt string `json:"joined_at,omitempty"`
}

type adminUserTeamView struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
	TeamSlug string `json:"team_slug"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
	Role     string `json:"role"`
	Status   string `json:"status,omitempty"`
	Personal bool   `json:"personal,omitempty"`
	JoinedAt string `json:"joined_at,omitempty"`
	// OrphanGrant marks a team grant whose org membership is missing. The
	// invariant is that every team grant mirrors up to one; a console that
	// silently repaired the display would hide exactly the drift an
	// operator opened this page to find.
	OrphanGrant bool `json:"orphan_grant,omitempty"`
}

type adminUserSSOLinkView struct {
	Provider string `json:"provider"`
	// Subject is the IdP's own user id. It is the only field that tells
	// two accounts of the same provider apart, which is what "is this the
	// same person?" reduces to during an access investigation.
	Subject   string `json:"subject"`
	Email     string `json:"email,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type adminUserDetailView struct {
	User UserView `json:"user"`
	// HasPassword reports whether a password login is possible at all —
	// derived from the stored hash, which never leaves the server. Combined
	// with SSOLinks it names how this account gets in, and its absence on
	// both sides is a dead account rather than a locked-out one.
	HasPassword bool                   `json:"has_password"`
	Orgs        []adminUserOrgView     `json:"orgs"`
	Teams       []adminUserTeamView    `json:"teams"`
	SSOLinks    []adminUserSSOLinkView `json:"sso_links"`
}

func (s *Server) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	st := s.authStore()
	if st == nil {
		httpError(w, http.StatusNotFound, "user console not enabled on this server")
		return
	}
	userID := r.PathValue("id")
	u, err := st.GetUser(r.Context(), userID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	view, err := s.buildAdminUserDetail(r.Context(), u)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, view)
}

// buildAdminUserDetail assembles the GRANTED memberships of one account.
//
// It deliberately does NOT go through buildOrgTree. That function answers
// "what can this user reach", and to do so it synthesizes an admin role on
// every team of an org the user administers. True for reachability, FALSE
// for what was granted — and an admin console that renders the second by
// reading the first tells an operator a team grant exists where none does.
// The two questions are different; so are their readers.
func (s *Server) buildAdminUserDetail(ctx context.Context, u identity.User) (adminUserDetailView, error) {
	st := s.authStore()

	orgMems, err := st.ListOrgMembershipsByUser(ctx, u.ID)
	if err != nil {
		return adminUserDetailView{}, err
	}
	teamMems, err := st.ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		return adminUserDetailView{}, err
	}

	teamIDs := make([]string, 0, len(teamMems))
	for _, m := range teamMems {
		teamIDs = append(teamIDs, m.TeamID)
	}
	teamsByID, err := st.GetTeamsByIDs(ctx, teamIDs)
	if err != nil {
		return adminUserDetailView{}, err
	}

	// The org ids to resolve are the union of the two sources: the orgs the
	// user is a member of, AND the parent orgs of the teams they hold a
	// grant in. Those sets are supposed to be nested; resolving only the
	// first would leave an orphan grant's org unnamed, which is the row an
	// operator most needs to read.
	orgIDs := make([]string, 0, len(orgMems)+len(teamsByID))
	memberOfOrg := make(map[string]bool, len(orgMems))
	for _, om := range orgMems {
		orgIDs = append(orgIDs, om.OrgID)
		memberOfOrg[om.OrgID] = true
	}
	for _, t := range teamsByID {
		orgIDs = append(orgIDs, t.OrgID)
	}
	orgsByID, err := st.GetOrgsByIDs(ctx, uniqueStrings(orgIDs))
	if err != nil {
		return adminUserDetailView{}, err
	}

	orgs := make([]adminUserOrgView, 0, len(orgMems))
	for _, om := range orgMems {
		v := adminUserOrgView{OrgID: om.OrgID, Role: string(om.Role)}
		if !om.JoinedAt.IsZero() {
			v.JoinedAt = om.JoinedAt.Format(time.RFC3339)
		}
		// A membership whose org row is gone still renders: the id is the
		// fact, and dropping the row would turn a dangling reference into
		// an account that merely "has no orgs".
		if o, ok := orgsByID[om.OrgID]; ok {
			v.OrgName, v.OrgSlug, v.Personal = o.Name, o.Slug, o.Personal
		}
		orgs = append(orgs, v)
	}

	teams := make([]adminUserTeamView, 0, len(teamMems))
	for _, m := range teamMems {
		v := adminUserTeamView{TeamID: m.TeamID, Role: string(m.Role)}
		if !m.JoinedAt.IsZero() {
			v.JoinedAt = m.JoinedAt.Format(time.RFC3339)
		}
		if t, ok := teamsByID[m.TeamID]; ok {
			v.TeamName, v.TeamSlug, v.Personal = t.Name, t.Slug, t.Personal
			v.Status = string(t.EffectiveStatus())
			v.OrgID = t.OrgID
			if o, ok := orgsByID[t.OrgID]; ok {
				v.OrgName = o.Name
			}
			// A personal team sits in a personal org whose membership row
			// follows the same rule as any other, so no exemption here.
			v.OrphanGrant = t.OrgID != "" && !memberOfOrg[t.OrgID]
		}
		teams = append(teams, v)
	}

	// Stable order, chosen rather than inherited: both stores sort these by
	// JoinedAt and leave ties to a map walk, so the console's row order
	// would otherwise change between two reads of the same account.
	sort.Slice(orgs, func(i, j int) bool {
		if orgs[i].OrgName != orgs[j].OrgName {
			return orgs[i].OrgName < orgs[j].OrgName
		}
		return orgs[i].OrgID < orgs[j].OrgID
	})
	sort.Slice(teams, func(i, j int) bool {
		if teams[i].OrgName != teams[j].OrgName {
			return teams[i].OrgName < teams[j].OrgName
		}
		if teams[i].TeamName != teams[j].TeamName {
			return teams[i].TeamName < teams[j].TeamName
		}
		return teams[i].TeamID < teams[j].TeamID
	})

	links := make([]adminUserSSOLinkView, 0)
	if s.authSvc != nil {
		raw, lerr := s.authSvc.ListSSOLinks(ctx, u.ID)
		if lerr != nil {
			return adminUserDetailView{}, lerr
		}
		for _, l := range raw {
			v := adminUserSSOLinkView{Provider: l.Provider, Subject: l.ProviderUserID, Email: l.Email}
			if !l.CreatedAt.IsZero() {
				v.CreatedAt = l.CreatedAt.Format(time.RFC3339)
			}
			links = append(links, v)
		}
		sort.Slice(links, func(i, j int) bool {
			if links[i].Provider != links[j].Provider {
				return links[i].Provider < links[j].Provider
			}
			return links[i].Subject < links[j].Subject
		})
	}

	return adminUserDetailView{
		User:        s.toUserView(u),
		HasPassword: u.PasswordHash != "",
		Orgs:        orgs,
		Teams:       teams,
		SSOLinks:    links,
	}, nil
}
