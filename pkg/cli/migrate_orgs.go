package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// MigrateOrgsResult summarizes a teams→orgs backfill (or its reversal).
type MigrateOrgsResult struct {
	ScannedTeams      int
	OrgsCreated       int
	TeamsLinked       int
	OrgMembersCreated int
	OrgsDeleted       int // reverse only
	// Mapping is teamID → orgID for the teams this run created/linked.
	Mapping map[string]string
	// Changes is a human-readable log of every mutation (or would-be
	// mutation under dry-run).
	Changes []string
}

// MigrateTeamsToOrgs is the idempotent teams→orgs backfill (ADR-048).
// For every team that has no parent org yet it creates a new Org (a new
// id, marked MigratedFromTeamID for idempotency + reversal), links the
// team to it, and mirrors the team's memberships up to org memberships
// (team owners/admins → org admins, everyone else → org member).
//
// Idempotent: a team already carrying an OrgID, or one that already has
// an org with MigratedFromTeamID == team.ID, is skipped — re-running is
// a no-op. The monthly run/cost/memory quota fields moved from Team to
// Org in this release; legacy custom caps are NOT carried over (they
// decode away from the new Team struct), so a migrated org starts on the
// platform defaults and an operator re-applies any custom cap via the
// admin console. Personal/Status are copied up.
//
// Pure store operations, so it runs against the Mongo store in
// production and the memory store in tests.
func MigrateTeamsToOrgs(ctx context.Context, st identity.Store, logger *iterlog.Logger, dryRun bool) (MigrateOrgsResult, error) {
	res := MigrateOrgsResult{Mapping: map[string]string{}}

	teams, err := listAllTeams(ctx, st)
	if err != nil {
		return res, fmt.Errorf("migrate orgs: list teams: %w", err)
	}
	res.ScannedTeams = len(teams)

	// Index existing migration-created orgs by their source team so a
	// re-run finds them instead of creating duplicates.
	migratedByTeam, err := migratedOrgsByTeam(ctx, st)
	if err != nil {
		return res, fmt.Errorf("migrate orgs: index orgs: %w", err)
	}

	for _, team := range teams {
		if team.OrgID != "" {
			continue // already linked
		}
		if existing, ok := migratedByTeam[team.ID]; ok {
			// Org exists from a prior run but the team link wasn't
			// persisted — repair the link and continue (idempotent).
			res.Mapping[team.ID] = existing.ID
			if err := linkTeam(ctx, st, team, existing.ID, dryRun); err != nil {
				return res, err
			}
			res.TeamsLinked++
			continue
		}

		org := identity.Org{
			ID:                 uuid.NewString(),
			Name:               team.Name,
			Slug:               team.Slug,
			CreatedBy:          team.CreatedBy,
			CreatedAt:          team.CreatedAt,
			UpdatedAt:          team.UpdatedAt,
			Personal:           team.Personal,
			MigratedFromTeamID: team.ID,
			Status:             team.Status,
			SuspendedAt:        team.SuspendedAt,
			SuspendedBy:        team.SuspendedBy,
			SuspendReason:      team.SuspendReason,
		}
		res.Changes = append(res.Changes, fmt.Sprintf("org create %q (from team %s) slug=%s", org.Name, team.ID, org.Slug))
		if !dryRun {
			created, cerr := createOrgUniqueSlug(ctx, st, org)
			if cerr != nil {
				return res, fmt.Errorf("migrate orgs: create org for team %s: %w", team.ID, cerr)
			}
			org = created
		}
		res.OrgsCreated++
		res.Mapping[team.ID] = org.ID

		if err := linkTeam(ctx, st, team, org.ID, dryRun); err != nil {
			return res, err
		}
		res.TeamsLinked++

		// Mirror team memberships → org memberships.
		members, merr := st.ListMembershipsByTeam(ctx, team.ID)
		if merr != nil {
			return res, fmt.Errorf("migrate orgs: list members of team %s: %w", team.ID, merr)
		}
		for _, m := range members {
			om := identity.OrgMembership{
				UserID:   m.UserID,
				OrgID:    org.ID,
				Role:     identity.OrgRoleForTeamRole(m.Role),
				Source:   m.Source,
				JoinedAt: m.JoinedAt,
			}
			res.Changes = append(res.Changes, fmt.Sprintf("org member %s -> org %s role=%s", m.UserID, org.ID, om.Role))
			if !dryRun {
				if err := st.UpsertOrgMembership(ctx, om); err != nil {
					return res, fmt.Errorf("migrate orgs: upsert org member %s: %w", m.UserID, err)
				}
			}
			res.OrgMembersCreated++
		}
		if logger != nil {
			logger.Info("migrate orgs: team %s -> org %s (%d members)", team.ID, org.ID, len(members))
		}
	}
	return res, nil
}

// ReverseTeamsToOrgs undoes a backfill: for every org created by the
// migration (MigratedFromTeamID set) it clears the source team's OrgID,
// deletes the org's memberships, and deletes the org. Idempotent.
func ReverseTeamsToOrgs(ctx context.Context, st identity.Store, logger *iterlog.Logger, dryRun bool) (MigrateOrgsResult, error) {
	res := MigrateOrgsResult{Mapping: map[string]string{}}
	orgs, err := listAllOrgs(ctx, st)
	if err != nil {
		return res, fmt.Errorf("reverse orgs: list orgs: %w", err)
	}
	for _, org := range orgs {
		if org.MigratedFromTeamID == "" {
			continue
		}
		res.Mapping[org.MigratedFromTeamID] = org.ID
		// Unlink the source team.
		if team, terr := st.GetTeam(ctx, org.MigratedFromTeamID); terr == nil && team.OrgID == org.ID {
			team.OrgID = ""
			res.Changes = append(res.Changes, fmt.Sprintf("unlink team %s from org %s", team.ID, org.ID))
			if !dryRun {
				if err := st.UpdateTeam(ctx, team); err != nil {
					return res, fmt.Errorf("reverse orgs: unlink team %s: %w", team.ID, err)
				}
			}
		}
		// Delete org memberships.
		members, _ := st.ListOrgMembershipsByOrg(ctx, org.ID)
		for _, m := range members {
			if !dryRun {
				if err := st.DeleteOrgMembership(ctx, m.UserID, org.ID); err != nil {
					return res, fmt.Errorf("reverse orgs: delete org member %s: %w", m.UserID, err)
				}
			}
		}
		res.Changes = append(res.Changes, fmt.Sprintf("delete org %s (from team %s)", org.ID, org.MigratedFromTeamID))
		if !dryRun {
			if err := st.DeleteOrg(ctx, org.ID); err != nil {
				return res, fmt.Errorf("reverse orgs: delete org %s: %w", org.ID, err)
			}
		}
		res.OrgsDeleted++
		if logger != nil {
			logger.Info("reverse orgs: deleted org %s, unlinked team %s", org.ID, org.MigratedFromTeamID)
		}
	}
	return res, nil
}

// ---- helpers ----

func linkTeam(ctx context.Context, st identity.Store, team identity.Team, orgID string, dryRun bool) error {
	if dryRun {
		return nil
	}
	team.OrgID = orgID
	if err := st.UpdateTeam(ctx, team); err != nil {
		return fmt.Errorf("migrate orgs: link team %s -> org %s: %w", team.ID, orgID, err)
	}
	return nil
}

// createOrgUniqueSlug creates an org, retrying with a numeric suffix on
// a slug collision (a fresh signup may already hold the base slug).
func createOrgUniqueSlug(ctx context.Context, st identity.Store, org identity.Org) (identity.Org, error) {
	base := org.Slug
	if base == "" {
		base = "org"
	}
	for attempt := 0; attempt < 20; attempt++ {
		try := base
		if attempt > 0 {
			try = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		org.Slug = try
		created, err := st.CreateOrg(ctx, org)
		if errors.Is(err, identity.ErrOrgSlugAlreadyTaken) {
			continue
		}
		if err != nil {
			return identity.Org{}, err
		}
		return created, nil
	}
	return identity.Org{}, fmt.Errorf("could not allocate slug for org from %q", base)
}

// listAllPaged drains a paginated list endpoint into one slice.
func listAllPaged[T any](page func(identity.Page) ([]T, error)) ([]T, error) {
	const size = 500
	var out []T
	for offset := 0; ; offset += size {
		batch, err := page(identity.Page{Offset: offset, Limit: size})
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < size {
			return out, nil
		}
	}
}

func listAllTeams(ctx context.Context, st identity.Store) ([]identity.Team, error) {
	return listAllPaged(func(p identity.Page) ([]identity.Team, error) { return st.ListTeams(ctx, p) })
}

func listAllOrgs(ctx context.Context, st identity.Store) ([]identity.Org, error) {
	return listAllPaged(func(p identity.Page) ([]identity.Org, error) { return st.ListOrgs(ctx, p) })
}

func migratedOrgsByTeam(ctx context.Context, st identity.Store) (map[string]identity.Org, error) {
	orgs, err := listAllOrgs(ctx, st)
	if err != nil {
		return nil, err
	}
	out := make(map[string]identity.Org, len(orgs))
	for _, o := range orgs {
		if o.MigratedFromTeamID != "" {
			out[o.MigratedFromTeamID] = o
		}
	}
	return out, nil
}
