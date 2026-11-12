package server

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The effective read model of a team's price feed (ADR-163): the table a
// run launched now for the team seals — the publisher's own fold
// (foldPriceFeed), at one instant, never a second one — and, per gateway
// id, every level and feed that states it. Member-readable: the prices
// and accepted efforts of the ids a team's runs dispatch are not secret,
// and the feeds' params, where credentials live, are not in it.

// priceFeedRunnerEnvFallback is the view's word on the runner's own
// environment tables (OPENAI_COMPATIBLE_MODELS,
// OPENAI_COMPATIBLE_REASONING_EFFORTS): they answer, on the runner that
// dispatches, the ids and fields the table leaves open, and the server
// never reads them.
const priceFeedRunnerEnvFallback = "not visible from the server"

// priceFeedEffectiveView is the effective view's answer.
type priceFeedEffectiveView struct {
	TeamID string `json:"team_id"`
	OrgID  string `json:"org_id,omitempty"`
	// At is the instant the fold judged freshness at.
	At time.Time `json:"at"`
	// Scopes are the levels the fold read, least specific first.
	Scopes []priceFeedEffectiveScope `json:"scopes"`
	// Models reports the requested ids, in request order — or, with none
	// requested, every id a level states, sorted.
	Models []priceFeedEffectiveModel `json:"models"`
	// Table is the table a run launched now for the team seals — the
	// same value, encoding to the same bytes; null when no feed answers
	// and the operator table is the floor.
	Table             *pricefeed.Table `json:"table"`
	Warnings          []string         `json:"warnings,omitempty"`
	RunnerEnvFallback string           `json:"runner_env_fallback"`
}

// priceFeedEffectiveScope is one level as the fold read it.
type priceFeedEffectiveScope struct {
	Scope string `json:"scope"` // platform | org | team
	ID    string `json:"id,omitempty"`
	// Feeds are the level's enabled feeds, in config order — the order
	// that decides between two of them stating one id.
	Feeds []string `json:"feeds,omitempty"`
	// Unread is the read error that stepped the level down.
	Unread string `json:"unread,omitempty"`
}

// priceFeedEffectiveModel is one gateway id's answer: the entry (prices,
// window, output cap, flags) and the accepted efforts a run reads from
// the table — each absent when no feed answers it — and every feed whose
// word the fold would take at its level.
type priceFeedEffectiveModel struct {
	ID           string                  `json:"id"`
	Entry        *pricefeed.Entry        `json:"entry,omitempty"`
	Efforts      *pricefeed.EffortsEntry `json:"efforts,omitempty"`
	Contributors []priceFeedContributor  `json:"contributors,omitempty"`
}

// priceFeedContributor is one feed stating an id, least specific level
// first. AnswersEntry / AnswersEfforts say whether its word EQUALS the
// answer the run reads — by value, so two feeds of one name at two levels
// are told apart.
type priceFeedContributor struct {
	Scope          string           `json:"scope"`
	ScopeID        string           `json:"scope_id,omitempty"`
	Feed           string           `json:"feed"`
	Spec           *modelspecs.Spec `json:"spec,omitempty"`
	Efforts        []string         `json:"efforts,omitempty"`
	AnswersEntry   bool             `json:"answers_entry,omitempty"`
	AnswersEfforts bool             `json:"answers_efforts,omitempty"`
}

func (s *Server) handleGetTeamPriceFeedsEffective(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this team")
		return
	}
	// The org level is the team's org, read the way the publisher reads
	// it for the run it seals; a team the view cannot place is refused,
	// never folded without its org.
	if s.authStore() == nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "no identity store to resolve team %s's org", teamID)
		return
	}
	team, err := s.authStore().GetTeam(r.Context(), teamID)
	if errors.Is(err, identity.ErrNotFound) {
		s.httpErrorFor(w, r, http.StatusNotFound, "team %s not found", teamID)
		return
	}
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "resolve team %s's org: %v", teamID, err)
		return
	}
	var models []string
	for _, raw := range r.URL.Query()["model"] {
		if m := pricefeed.NormalizeID(raw); m != "" && !slices.Contains(models, m) {
			models = append(models, m)
		}
	}
	var warnings []string
	warnf := func(format string, args ...any) {
		warnings = append(warnings, truncateFeedError(sanitizeFeedError(fmt.Sprintf(format, args...))))
	}
	fold := foldPriceFeed(r.Context(), s.priceFeedConfigs, s.priceFeedStates, team.OrgID, teamID, models, warnf)
	view := priceFeedEffectiveView{
		TeamID:            teamID,
		OrgID:             team.OrgID,
		At:                fold.now,
		RunnerEnvFallback: priceFeedRunnerEnvFallback,
	}
	// What the bundle would carry: the encoded table, or nothing when it
	// does not encode (warned) — never a table the run would not seal.
	if fold.encode(warnf) != nil {
		view.Table = fold.table
	}
	scopes, contributors := effectivePriceFeedContributors(fold)
	view.Scopes = scopes
	view.Models = effectivePriceFeedModels(fold, contributors, models)
	view.Warnings = warnings
	s.writeJSONFor(w, r, view)
}

// effectivePriceFeedContributors folds every enabled feed of every level
// read ALONE, by the fold's own rules at the fold's instant: per id, each
// feed whose word the fold would take at its level, least specific level
// first, config order within one.
func effectivePriceFeedContributors(fold priceFeedFold) ([]priceFeedEffectiveScope, map[string][]priceFeedContributor) {
	scopes := make([]priceFeedEffectiveScope, 0, len(fold.scopes))
	byID := map[string][]priceFeedContributor{}
	for _, read := range fold.scopes {
		level := priceFeedEffectiveScope{Scope: read.scope.level, ID: read.scope.id}
		if read.err != nil {
			level.Unread = truncateFeedError(sanitizeFeedError(read.err.Error()))
		}
		if read.state != nil {
			for _, feed := range read.state.Config.Feeds {
				if !feed.Enabled {
					continue
				}
				level.Feeds = append(level.Feeds, feed.Name)
				alone := pricefeed.Fold(pricefeed.FoldConfig{Now: fold.now, Budget: math.MaxInt}, pricefeed.ScopeState{
					Config: &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{feed}, UpdatedAt: read.state.Config.UpdatedAt},
					States: read.state.States,
				})
				for _, id := range alone.IDs() {
					c := priceFeedContributor{Scope: read.scope.level, ScopeID: read.scope.id, Feed: feed.Name}
					if entry, ok := alone.Entries[id]; ok {
						spec := entry.Spec
						c.Spec = &spec
						c.AnswersEntry = fold.table != nil && priceFeedEntryEqual(entry, fold.table.Entries[id])
					}
					if efforts, ok := alone.Efforts[id]; ok {
						c.Efforts = efforts.Values
						c.AnswersEfforts = fold.table != nil && priceFeedEffortsEqual(efforts, fold.table.Efforts[id])
					}
					byID[id] = append(byID[id], c)
				}
			}
		}
		scopes = append(scopes, level)
	}
	return scopes, byID
}

// effectivePriceFeedModels reports the requested ids, or every id the
// table or a contributor carries when none is requested.
func effectivePriceFeedModels(fold priceFeedFold, contributors map[string][]priceFeedContributor, requested []string) []priceFeedEffectiveModel {
	ids := requested
	if len(ids) == 0 {
		seen := map[string]bool{}
		for _, id := range fold.table.IDs() {
			seen[id] = true
		}
		for id := range contributors {
			seen[id] = true
		}
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	models := make([]priceFeedEffectiveModel, 0, len(ids))
	for _, id := range ids {
		m := priceFeedEffectiveModel{ID: id, Contributors: contributors[id]}
		if fold.table != nil {
			if entry, ok := fold.table.Entries[id]; ok {
				m.Entry = &entry
			}
			if efforts, ok := fold.table.Efforts[id]; ok {
				m.Efforts = &efforts
			}
		}
		models = append(models, m)
	}
	return models
}

// priceFeedEntryEqual compares two folded entries by value: feed, age,
// window and spec.
func priceFeedEntryEqual(a, b pricefeed.Entry) bool {
	return a.Feed == b.Feed && a.RefreshedAt.Equal(b.RefreshedAt) && a.FreshFor == b.FreshFor &&
		a.Declared == b.Declared && pricefeed.SpecEqual(a.Spec, b.Spec)
}

// priceFeedEffortsEqual compares two folded efforts entries by value.
func priceFeedEffortsEqual(a, b pricefeed.EffortsEntry) bool {
	return a.Feed == b.Feed && slices.Equal(a.Values, b.Values)
}
