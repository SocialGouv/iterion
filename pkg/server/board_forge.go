package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
)

// ImportForgeIssues mirrors one forge repo's issues into a native board from a
// raw provider + base URL + token — the self-hosted entry point behind
// `iterion issue import`. It builds the matching forge.IssueClient via the same
// provider switch as forgeAdminForToken (kept DRY here so the CLI never forks
// the construction), then delegates to the store- and cloud-agnostic
// syncForgeIssuesToBoard core. An empty baseURL falls back to the provider's
// canonical SaaS host (required to be set explicitly for self-hosted
// forgejo/gitlab). since == zero re-syncs everything (idempotent). The
// high-water mark is the caller's concern and stays out of this function.
// minAuthorRole is the trust threshold for the triage:auto stamp ("" →
// developer ≡ write); authors below it get needs:approval cards.
func ImportForgeIssues(ctx context.Context, provider forge.Provider, baseURL, token, repo string, board native.BoardStore, since time.Time, minAuthorRole string) (created, updated int, err error) {
	if baseURL == "" {
		baseURL = forge.DefaultBaseURL(provider)
	}
	var admin forge.Admin
	switch provider {
	case forge.ProviderGitLab:
		admin = forgegitlab.New(http.DefaultClient, baseURL, token)
	case forge.ProviderGitHub:
		admin = forgegithub.New(http.DefaultClient, baseURL, token)
	case forge.ProviderForgejo:
		admin = forgeforgejo.New(http.DefaultClient, baseURL, token)
	default:
		return 0, 0, fmt.Errorf("forge: provider %q is not yet supported", provider)
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return 0, 0, fmt.Errorf("forge: provider %q has no issue client", provider)
	}
	// connID is empty for a self-hosted import: there is no persisted forge
	// connection, and the deterministic card ID keys on provider+repo+number.
	return syncForgeIssuesToBoard(ctx, ic, provider, "", repo, board, since,
		authorTrustFunc(admin, string(provider), repo, minAuthorRole, newAuthorTrust()))
}

// authorTrustFunc builds the per-login trust classifier the sync core takes,
// from a provider admin client's optional PermissionClient capability. A
// client without the capability yields a fn that only ever trusts nobody via
// the API path (fail-closed) — the assoc fast path doesn't apply on sync
// (ListIssues carries no author_association).
func authorTrustFunc(admin forge.Admin, provider, repo, minRole string, gate *authorTrust) func(context.Context, string) bool {
	pc, _ := admin.(forge.PermissionClient)
	return func(ctx context.Context, login string) bool {
		return gate.trusted(ctx, pc, provider, repo, login, "", minRole, nil)
	}
}

// forgeSyncNamespace is the fixed UUIDv5 namespace that turns a forge issue
// key ("<provider>:<repo>#<number>") into a deterministic, valid
// "native:<uuid>" card ID, so re-syncing the same issue UPSERTS its card
// instead of duplicating it.
var forgeSyncNamespace = uuid.MustParse("a3f4c1d2-7b9e-5a6f-8c2d-1e0f9b8a7c6d")

func forgeCardID(provider forge.Provider, repo string, number int) string {
	key := fmt.Sprintf("%s:%s#%d", provider, repo, number)
	return "native:" + uuid.NewSHA1(forgeSyncNamespace, []byte(key)).String()
}

// registerBoardForgeRoutes mounts the cloud-only forge↔board endpoints: the
// per-repo sync toggle + manual sync (team-scoped), and the per-card
// push-to-forge + linked-PR + CI views (active-team board scoped). All are
// no-ops in self-hosted mode (no CloudBoardFor / forge stores).
func (s *Server) registerBoardForgeRoutes() {
	if s.cfg.CloudBoardFor == nil || s.forgeIntegrations == nil {
		return
	}
	s.mux.Handle("PATCH /api/teams/{id}/forge/integrations/{iid}", s.requireAuth(http.HandlerFunc(s.handlePatchForgeIntegration)))
	s.mux.Handle("POST /api/teams/{id}/forge/integrations/{iid}/sync", s.requireAuth(http.HandlerFunc(s.handleSyncForgeIntegration)))
	s.mux.Handle("POST /api/v1/native/issues/{id}/push", s.requireAuth(http.HandlerFunc(s.handlePushIssueToForge)))
	s.mux.Handle("GET /api/v1/native/issues/{id}/pulls", s.requireAuth(http.HandlerFunc(s.handleListIssuePulls)))
	s.mux.Handle("POST /api/v1/native/issues/{id}/pulls", s.requireAuth(http.HandlerFunc(s.handleCreateIssuePull)))
	s.mux.Handle("GET /api/v1/native/issues/{id}/pulls/{number}/ci", s.requireAuth(http.HandlerFunc(s.handleIssuePullCI)))
	s.mux.Handle("POST /api/v1/native/issues/{id}/pulls/{number}/merge", s.requireAuth(http.HandlerFunc(s.handleMergeIssuePull)))
	s.mux.Handle("GET /api/teams/{id}/forge/integrations/{iid}/hooks", s.requireAuth(http.HandlerFunc(s.handleListIntegrationHooks)))
}

// ---------------------------------------------------------------------------
// per-repo sync toggle + manual sync (team-scoped)
// ---------------------------------------------------------------------------

type patchIntegrationReq struct {
	SyncIssuesEnabled *bool `json:"sync_issues_enabled,omitempty"`
}

func (s *Server) handlePatchForgeIntegration(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	ri, ok := s.forgeIntegrationForTenant(w, r, teamID, r.PathValue("iid"))
	if !ok {
		return
	}
	var req patchIntegrationReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SyncIssuesEnabled != nil {
		ri.SyncIssuesEnabled = *req.SyncIssuesEnabled
	}
	ri.UpdatedAt = time.Now().UTC()
	if err := s.forgeIntegrations.Update(r.Context(), ri); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, ri)
}

type syncResult struct {
	Synced  int `json:"synced"`
	Created int `json:"created"`
	Updated int `json:"updated"`
}

func (s *Server) handleSyncForgeIntegration(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	ri, ok := s.forgeIntegrationForTenant(w, r, teamID, r.PathValue("iid"))
	if !ok {
		return
	}
	created, updated, err := s.syncOneIntegration(r.Context(), teamID, ri)
	if err != nil {
		httpError(w, http.StatusBadGateway, "sync failed: %v", err)
		return
	}
	writeJSON(w, syncResult{Synced: created + updated, Created: created, Updated: updated})
}

// forgeIntegrationForTenant loads an integration and asserts it belongs to the
// team, writing 404 otherwise.
func (s *Server) forgeIntegrationForTenant(w http.ResponseWriter, r *http.Request, teamID, iid string) (forge.RepoIntegration, bool) {
	ri, err := s.forgeIntegrations.Get(r.Context(), iid)
	if err != nil || ri.TenantID != teamID {
		httpError(w, http.StatusNotFound, "integration not found")
		return forge.RepoIntegration{}, false
	}
	return ri, true
}

// ---------------------------------------------------------------------------
// forge → board sync (one-way; the source is the forge)
// ---------------------------------------------------------------------------

// syncForgeIssuesToBoard mirrors one repo's forge issues into a native board.
// It is store- and cloud-agnostic — it takes an already-resolved issue client
// + board — so the cloud per-team sync and a (future) self-hosted single-store
// import can share ONE implementation. Forge is the source of truth (one-way);
// pull requests are skipped (they surface via the card PR panel). Cards land in
// the first non-terminal column on create and refresh in place on update; see
// upsertForgeCard for the column policy. Returns per-issue create/update counts.
//
// trust classifies an issue author's repo rights: a trusted author's fresh
// open card is stamped triage:auto (fires the auto-triage), an untrusted one
// needs:approval (parked until an operator approves). nil trust fails CLOSED —
// every new card parks — because this is the budget security boundary, not an
// operator preference. Classification is memoized per sweep.
//
// The high-water mark (LastSyncedAt) is the caller's concern — it is the only
// piece that differs between the cloud integration store and a self-hosted
// entry point, so it stays out of this pure core.
func syncForgeIssuesToBoard(ctx context.Context, ic forge.IssueClient, provider forge.Provider, connID, repo string, board native.BoardStore, since time.Time, trust func(ctx context.Context, login string) bool) (created, updated int, err error) {
	issues, err := ic.ListIssues(ctx, repo, forge.IssueListOptions{
		State: "all", Since: since, PerPage: 100,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("list issues: %w", err)
	}
	b := board.Board()
	openCol := defaultOpenColumn(b)
	doneCol := terminalColumn(b)
	trustMemo := map[string]bool{}
	trusted := func(login string) bool {
		if trust == nil || login == "" {
			return false
		}
		if v, ok := trustMemo[login]; ok {
			return v
		}
		v := trust(ctx, login)
		trustMemo[login] = v
		return v
	}
	for _, is := range issues {
		if is.IsPullRequest {
			continue // the board syncs ISSUES; PRs surface via the card PR panel
		}
		c, u, e := upsertForgeCard(board, b, openCol, doneCol, provider, connID, repo, is, trusted(is.Author))
		if e != nil {
			if err == nil {
				err = e
			}
			continue
		}
		created += c
		updated += u
	}
	return created, updated, err
}

// syncOneIntegration mirrors one repo's forge issues into the team board and
// stamps the integration's LastSyncedAt high-water mark. It is the cloud
// wrapper around syncForgeIssuesToBoard: it resolves the connection → issue
// client → team board, then persists the high-water mark. It is the unit both
// the manual "Sync now" endpoint and the periodic worker call.
func (s *Server) syncOneIntegration(ctx context.Context, teamID string, ri forge.RepoIntegration) (created, updated int, err error) {
	conn, err := s.forgeConnections.Get(ctx, ri.ConnectionID)
	if err != nil {
		return 0, 0, fmt.Errorf("connection: %w", err)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return 0, 0, fmt.Errorf("admin client: %w", err)
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return 0, 0, fmt.Errorf("provider %s has no issue client", conn.Provider)
	}
	board := s.cfg.CloudBoardFor(teamID)
	if board == nil {
		return 0, 0, errors.New("no board for team")
	}
	created, updated, err = syncForgeIssuesToBoard(ctx, ic, conn.Provider, ri.ConnectionID, ri.RepoFullName, board, ri.LastSyncedAt,
		authorTrustFunc(admin, string(conn.Provider), ri.RepoFullName, ri.MinAuthorRole, s.authorTrustGate()))
	// Advance the high-water mark unconditionally: a partial-failure sync
	// still records progress on the issues it did upsert.
	ri.LastSyncedAt = time.Now().UTC()
	if uerr := s.forgeIntegrations.Update(ctx, ri); uerr != nil && err == nil {
		err = uerr
	}
	return created, updated, err
}

// upsertForgeCard creates or updates the board card mirroring one forge issue.
// On create the card lands in openCol (or doneCol when already closed); a
// fresh OPEN card is additionally stamped with the trust label (triage:auto
// for a trusted author, needs:approval otherwise — never on closed issues,
// never re-stamped on update). On update the card's content/labels refresh
// but its COLUMN is left to the operator — except a forge-close pulls a
// still-open card to the terminal column — and board-local label namespaces
// are preserved (forge labels are mirrored verbatim; triage:/needs:/cmd:/
// source: labels belong to the board and must survive the sweep). Returns
// (created, updated) as 0/1 flags.
func upsertForgeCard(board native.BoardStore, b *native.Board, openCol, doneCol string, provider forge.Provider, connID, repo string, is forge.IssueRef, trusted bool) (int, int, error) {
	cardID := forgeCardID(provider, repo, is.Number)
	ext := &native.ExternalRef{
		Provider:     string(provider),
		ConnectionID: connID,
		Repo:         repo,
		Number:       is.Number,
		URL:          is.URL,
		State:        is.State,
		Author:       is.Author,
	}
	existing, gerr := board.Get(cardID)
	if gerr != nil {
		col := openCol
		labels := is.Labels
		if is.State == "closed" && doneCol != "" {
			col = doneCol
		} else {
			trustLabel := native.LabelNeedsApproval
			if trusted {
				trustLabel = native.LabelTriageAuto
			}
			labels = append(append([]string(nil), is.Labels...), trustLabel)
		}
		if _, err := board.Create(native.Issue{
			ID:       cardID,
			Title:    is.Title,
			Body:     is.Body,
			State:    col,
			Labels:   labels,
			Assignee: firstString(is.Assignees),
			External: ext,
		}); err != nil {
			return 0, 0, err
		}
		return 1, 0, nil
	}
	labels := mergeForgeLabels(is.Labels, existing.Labels)
	if _, err := board.Update(cardID, native.Patch{
		Title:    &is.Title,
		Body:     &is.Body,
		Labels:   &labels,
		External: ext,
	}); err != nil {
		return 0, 0, err
	}
	if is.State == "closed" && doneCol != "" && !isTerminalState(b, existing.State) {
		// CAS on the snapshot: an operator who moved the card between our
		// read and this write wins — the sync must not clobber a fresh
		// human decision with a stale forge fact.
		if _, _, err := board.SetStateFrom(cardID, existing.State, doneCol); err != nil {
			return 0, 0, err
		}
	}
	return 0, 1, nil
}

// boardLocalLabelPrefixes are the label namespaces owned by the BOARD, not
// the forge: the ingest trust labels (triage:, needs:), command idempotency
// markers (cmd:), provenance (source:) and the project-board field labels
// (area:, mode:, prio: — written by the PROJECT import, present on no forge
// repo). The forge sync's label refresh preserves them; everything else
// mirrors the forge verbatim.
var boardLocalLabelPrefixes = append(
	[]string{"triage:", "needs:", "cmd:", "source:"},
	projectFieldLabelPrefixes()...,
)

// projectFieldLabelPrefixes lists the namespaces the project import owns, read
// from the same declaration the import writes through — so adding a bound
// field cannot leave its labels unprotected against the next issue import.
func projectFieldLabelPrefixes() []string {
	fields := forge.DefaultLabelFields()
	out := make([]string, 0, len(fields))
	for _, lf := range fields {
		out = append(out, lf.Prefix)
	}
	return out
}

func isBoardLocalLabel(l string) bool {
	ll := strings.ToLower(l)
	for _, p := range boardLocalLabelPrefixes {
		if strings.HasPrefix(ll, p) {
			return true
		}
	}
	// Legacy dash form stamped by the triage decision tree.
	return ll == "needs-manual-triage"
}

// mergeForgeLabels refreshes a synced card's labels from the forge while
// keeping the card's board-local labels: forge labels verbatim ∪ (existing ∩
// board-local namespaces), deduplicated case-insensitively, order stable
// (forge first, then surviving board-local).
func mergeForgeLabels(forgeLabels, existing []string) []string {
	out := make([]string, 0, len(forgeLabels)+4)
	seen := map[string]bool{}
	add := func(l string) {
		k := strings.ToLower(l)
		if l == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, l)
	}
	for _, l := range forgeLabels {
		add(l)
	}
	for _, l := range existing {
		if isBoardLocalLabel(l) {
			add(l)
		}
	}
	return out
}

// defaultOpenColumn is the landing column for a newly-synced open issue: the
// first non-terminal state on the board (the inbox in the default layout).
func defaultOpenColumn(b *native.Board) string {
	for _, st := range b.States {
		if !st.Terminal {
			return st.Name
		}
	}
	if len(b.States) > 0 {
		return b.States[0].Name
	}
	return ""
}

// terminalColumn is where a closed issue lands: the first terminal state.
func terminalColumn(b *native.Board) string {
	for _, st := range b.States {
		if st.Terminal {
			return st.Name
		}
	}
	return ""
}

func isTerminalState(b *native.Board, name string) bool {
	for _, st := range b.States {
		if st.Name == name {
			return st.Terminal
		}
	}
	return false
}

func firstString(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// ---------------------------------------------------------------------------
// board → forge: push a card to the forge as an issue (discreet card action)
// ---------------------------------------------------------------------------

type pushIssueReq struct {
	ConnectionID string `json:"connection_id,omitempty"`
	Repo         string `json:"repo,omitempty"`
}

type pushIssueResp struct {
	URL      string `json:"url"`
	Number   int    `json:"number"`
	Provider string `json:"provider"`
}

func (s *Server) handlePushIssueToForge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	board, ok := s.activeTeamBoard(w, id)
	if !ok {
		return
	}
	cardID, err := board.Resolve(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "card not found")
		return
	}
	card, err := board.Get(cardID)
	if err != nil {
		httpError(w, http.StatusNotFound, "card not found")
		return
	}
	var req pushIssueReq
	_ = decodeJSONOptional(r, &req)

	_, connID, repo, number := forgeLinkOf(card)
	// Already linked → update the existing forge issue from the card.
	if connID != "" && repo != "" && number > 0 {
		ic, conn, ok := s.issueClientForConn(w, r.Context(), id.TeamID, connID)
		if !ok {
			return
		}
		state := "open"
		body := card.Body
		title := card.Title
		labels := card.Labels
		ref, err := ic.UpdateIssue(r.Context(), repo, number, forge.IssuePatch{
			Title: &title, Body: &body, State: &state, Labels: &labels,
		})
		if err != nil {
			httpError(w, http.StatusBadGateway, "update forge issue: %v", err)
			return
		}
		writeJSON(w, pushIssueResp{URL: ref.URL, Number: ref.Number, Provider: string(conn.Provider)})
		return
	}
	// Unlinked → create a new forge issue; body+repo required.
	if strings.TrimSpace(req.ConnectionID) == "" || strings.TrimSpace(req.Repo) == "" {
		httpError(w, http.StatusBadRequest, "connection_id and repo are required to push an unlinked card")
		return
	}
	ic, conn, ok := s.issueClientForConn(w, r.Context(), id.TeamID, req.ConnectionID)
	if !ok {
		return
	}
	ref, err := ic.CreateIssue(r.Context(), req.Repo, forge.NewIssue{
		Title: card.Title, Body: card.Body, Labels: card.Labels,
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, "create forge issue: %v", err)
		return
	}
	// Stamp the card with its new forge linkage so future pushes update it.
	// A failed stamp must surface: an unlinked card whose forge issue exists
	// would silently create a duplicate on the next push.
	if _, err := board.Update(cardID, native.Patch{External: &native.ExternalRef{
		Provider:     string(conn.Provider),
		ConnectionID: req.ConnectionID,
		Repo:         req.Repo,
		Number:       ref.Number,
		URL:          ref.URL,
		State:        ref.State,
	}}); err != nil {
		httpError(w, http.StatusInternalServerError, "forge issue created (%s) but linking the card failed: %v — re-push would create a duplicate; link manually", ref.URL, err)
		return
	}
	writeJSON(w, pushIssueResp{URL: ref.URL, Number: ref.Number, Provider: string(conn.Provider)})
}

// ---------------------------------------------------------------------------
// linked PRs + CI (read-only card panel)
// ---------------------------------------------------------------------------

func (s *Server) handleListIssuePulls(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	board, ok := s.activeTeamBoard(w, id)
	if !ok {
		return
	}
	card, ok := s.cardFromPath(w, r, board)
	if !ok {
		return
	}
	_, connID, repo, number := forgeLinkOf(card)
	if repo == "" || connID == "" {
		writeJSON(w, struct {
			Pulls []forge.PullRef `json:"pulls"`
		}{Pulls: []forge.PullRef{}})
		return
	}
	pc, _, ok := s.pullClientForConn(w, r.Context(), id.TeamID, connID)
	if !ok {
		return
	}
	all, err := pc.ListPullRequests(r.Context(), repo, forge.PullListOptions{State: "all", PerPage: 100})
	if err != nil {
		httpError(w, http.StatusBadGateway, "list pull requests: %v", err)
		return
	}
	// Keep only PRs that reference this card's forge issue number.
	out := make([]forge.PullRef, 0, len(all))
	for _, pr := range all {
		if number > 0 && slices.Contains(pr.LinkedIssues, number) {
			out = append(out, pr)
		}
	}
	writeJSON(w, struct {
		Pulls []forge.PullRef `json:"pulls"`
	}{Pulls: out})
}

// ---------------------------------------------------------------------------
// board → forge: open / merge a PR from a card (bot-driven lifecycle, item 1)
// ---------------------------------------------------------------------------

type createPullReq struct {
	ConnectionID string `json:"connection_id,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Draft        bool   `json:"draft,omitempty"`
}

// handleCreateIssuePull opens a PR/MR tied to a card. A forge-linked card reuses
// its connection+repo; an unlinked one requires connection_id+repo in the body.
func (s *Server) handleCreateIssuePull(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	board, ok := s.activeTeamBoard(w, id)
	if !ok {
		return
	}
	card, ok := s.cardFromPath(w, r, board)
	if !ok {
		return
	}
	var req createPullReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SourceBranch) == "" || strings.TrimSpace(req.TargetBranch) == "" {
		httpError(w, http.StatusBadRequest, "source_branch and target_branch are required")
		return
	}
	_, connID, repo, _ := forgeLinkOf(card)
	if connID == "" || repo == "" {
		connID, repo = strings.TrimSpace(req.ConnectionID), strings.TrimSpace(req.Repo)
	}
	if connID == "" || repo == "" {
		httpError(w, http.StatusBadRequest, "connection_id and repo are required for an unlinked card")
		return
	}
	pc, _, ok := s.pullClientForConn(w, r.Context(), id.TeamID, connID)
	if !ok {
		return
	}
	title := req.Title
	if title == "" {
		title = card.Title
	}
	ref, err := pc.CreatePull(r.Context(), repo, forge.NewPull{
		Title:        title,
		Body:         req.Body,
		SourceBranch: req.SourceBranch,
		TargetBranch: req.TargetBranch,
		Draft:        req.Draft,
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, "create pull request: %v", err)
		return
	}
	writeJSON(w, ref)
}

type mergePullReq struct {
	Method        string `json:"method,omitempty"` // "merge" | "squash" | "rebase"
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	DeleteBranch  bool   `json:"delete_branch,omitempty"`
}

// handleMergeIssuePull merges the PR/MR linked to a card via its forge
// connection. The card must be forge-linked (so the connection+repo are known).
func (s *Server) handleMergeIssuePull(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	board, ok := s.activeTeamBoard(w, id)
	if !ok {
		return
	}
	card, ok := s.cardFromPath(w, r, board)
	if !ok {
		return
	}
	_, connID, repo, _ := forgeLinkOf(card)
	if repo == "" || connID == "" {
		httpError(w, http.StatusBadRequest, "card is not linked to a forge")
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid pull number")
		return
	}
	var req mergePullReq
	_ = decodeJSONOptional(r, &req)
	pc, _, ok := s.pullClientForConn(w, r.Context(), id.TeamID, connID)
	if !ok {
		return
	}
	ref, err := pc.MergePull(r.Context(), repo, number, forge.MergeOptions{
		Method:        forge.MergeMethod(req.Method),
		CommitTitle:   req.CommitTitle,
		CommitMessage: req.CommitMessage,
		DeleteBranch:  req.DeleteBranch,
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, "merge pull request: %v", err)
		return
	}
	writeJSON(w, ref)
}

// ---------------------------------------------------------------------------
// webhook introspection: list the forge-side hooks on an integration (item 4)
// ---------------------------------------------------------------------------

// handleListIntegrationHooks returns every webhook currently registered on the
// integration's repo (forge-side truth), so the operator can audit orphaned or
// divergent hooks against iterion's own delivery records.
func (s *Server) handleListIntegrationHooks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	ri, ok := s.forgeIntegrationForTenant(w, r, teamID, r.PathValue("iid"))
	if !ok {
		return
	}
	conn, err := s.forgeConnections.Get(r.Context(), ri.ConnectionID)
	if err != nil || conn.TenantID != teamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusBadGateway, "admin client: %v", err)
		return
	}
	hooks, err := admin.ListHooks(r.Context(), ri.RepoFullName)
	if err != nil {
		httpError(w, http.StatusBadGateway, "list hooks: %v", err)
		return
	}
	writeJSON(w, struct {
		Hooks []forge.HookHandle `json:"hooks"`
	}{Hooks: hooks})
}

func (s *Server) handleIssuePullCI(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	board, ok := s.activeTeamBoard(w, id)
	if !ok {
		return
	}
	card, ok := s.cardFromPath(w, r, board)
	if !ok {
		return
	}
	_, connID, repo, _ := forgeLinkOf(card)
	if repo == "" || connID == "" {
		httpError(w, http.StatusBadRequest, "card is not linked to a forge")
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid pull number")
		return
	}
	pc, _, ok := s.pullClientForConn(w, r.Context(), id.TeamID, connID)
	if !ok {
		return
	}
	pr, err := pc.GetPullRequest(r.Context(), repo, number)
	if err != nil {
		httpError(w, http.StatusBadGateway, "get pull request: %v", err)
		return
	}
	ref := pr.HeadSHA
	if ref == "" {
		ref = pr.SourceBranch
	}
	status, err := pc.GetCIStatus(r.Context(), repo, ref)
	if err != nil {
		httpError(w, http.StatusBadGateway, "get ci status: %v", err)
		return
	}
	history, _ := pc.ListCIHistory(r.Context(), repo, ref, 20)
	writeJSON(w, struct {
		Status  forge.CIStatus `json:"status"`
		History []forge.CIRun  `json:"history"`
	}{Status: status, History: history})
}

// ---------------------------------------------------------------------------
// shared resolution helpers
// ---------------------------------------------------------------------------

// activeTeamBoard returns the caller's active-team board, 404 if none.
func (s *Server) activeTeamBoard(w http.ResponseWriter, id auth.Identity) (native.BoardStore, bool) {
	if s.cfg.CloudBoardFor == nil || id.TeamID == "" {
		httpError(w, http.StatusNotFound, "board not available")
		return nil, false
	}
	b := s.cfg.CloudBoardFor(id.TeamID)
	if b == nil {
		httpError(w, http.StatusNotFound, "board not available")
		return nil, false
	}
	return b, true
}

func (s *Server) cardFromPath(w http.ResponseWriter, r *http.Request, board native.BoardStore) (*native.Issue, bool) {
	cardID, err := board.Resolve(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "card not found")
		return nil, false
	}
	card, err := board.Get(cardID)
	if err != nil {
		httpError(w, http.StatusNotFound, "card not found")
		return nil, false
	}
	return card, true
}

// connAdminFor resolves a team connection and its forge.Admin client,
// writing the appropriate HTTP error and returning ok=false on any
// failure. Shared by issueClientForConn/pullClientForConn, which each
// perform their own type assertion on the returned admin client.
func (s *Server) connAdminFor(w http.ResponseWriter, ctx context.Context, teamID, connID string) (forge.Admin, forge.Connection, bool) {
	conn, err := s.forgeConnections.Get(ctx, connID)
	if err != nil || conn.TenantID != teamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return nil, forge.Connection{}, false
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		httpError(w, http.StatusBadGateway, "admin client: %v", err)
		return nil, forge.Connection{}, false
	}
	return admin, conn, true
}

// issueClientForConn resolves a team connection to a forge.IssueClient.
func (s *Server) issueClientForConn(w http.ResponseWriter, ctx context.Context, teamID, connID string) (forge.IssueClient, forge.Connection, bool) {
	admin, conn, ok := s.connAdminFor(w, ctx, teamID, connID)
	if !ok {
		return nil, forge.Connection{}, false
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		httpError(w, http.StatusNotImplemented, "provider %s has no issue client", conn.Provider)
		return nil, forge.Connection{}, false
	}
	return ic, conn, true
}

func (s *Server) pullClientForConn(w http.ResponseWriter, ctx context.Context, teamID, connID string) (forge.PullClient, forge.Connection, bool) {
	admin, conn, ok := s.connAdminFor(w, ctx, teamID, connID)
	if !ok {
		return nil, forge.Connection{}, false
	}
	pc, ok := admin.(forge.PullClient)
	if !ok {
		httpError(w, http.StatusNotImplemented, "provider %s has no pull/CI client", conn.Provider)
		return nil, forge.Connection{}, false
	}
	return pc, conn, true
}

// forgeLinkOf extracts a card's forge linkage from its typed External ref.
func forgeLinkOf(card *native.Issue) (provider forge.Provider, connID, repo string, number int) {
	if card == nil || card.External == nil {
		return "", "", "", 0
	}
	e := card.External
	return forge.Provider(e.Provider), e.ConnectionID, e.Repo, e.Number
}

// decodeJSONOptional decodes an OPTIONAL request body into v, tolerating an
// empty/absent body (returns nil) — used by push, where a forge-linked card
// sends no body.
func decodeJSONOptional(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// near-real-time forge → board projection (webhook-driven)
// ---------------------------------------------------------------------------

// projectForgeWebhookToBoard refreshes the board card(s) for a repo the moment
// an inbound forge webhook touches it, instead of waiting up to a full
// runBoardSyncWorker interval. It runs an INCREMENTAL sync (ListIssues since
// LastSyncedAt) of every sync-enabled integration whose RepoFullName matches
// the event's repo slug — reusing the exact same syncOneIntegration path, so
// the changed issue (and any other recently-touched one) lands on its team
// board immediately. The periodic worker stays the reconciliation net.
//
// Best-effort and non-blocking: it is invoked on a detached context from the
// webhook tail so it never delays the webhook ack; failures are logged. No-op
// when the cloud board / forge stores aren't wired (self-hosted mode).
func (s *Server) projectForgeWebhookToBoard(ctx context.Context, repo string) {
	if s.cfg.CloudBoardFor == nil || s.forgeIntegrations == nil || strings.TrimSpace(repo) == "" {
		return
	}
	ris, err := s.forgeIntegrations.ListSyncEnabledForRepo(ctx, repo)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("board projection: list sync-enabled integrations: %v", err)
		}
		return
	}
	for _, ri := range ris {
		c, u, serr := s.syncOneIntegration(ctx, ri.TenantID, ri)
		if serr != nil && s.logger != nil {
			s.logger.Warn("board projection: %s/%s: %v", ri.TenantID, ri.RepoFullName, serr)
		} else if (c > 0 || u > 0) && s.logger != nil {
			s.logger.Info("board projection: %s %s → %d created, %d updated", ri.TenantID, ri.RepoFullName, c, u)
		}
	}
}

// ---------------------------------------------------------------------------
// periodic forge → board sync worker
// ---------------------------------------------------------------------------

// runBoardSyncWorker periodically sweeps every sync-enabled integration and
// mirrors its forge issues onto the team board. Cloud-only; started from
// server start alongside the forge/OAuth refresh workers. Best-effort: one
// integration's failure is logged and the sweep continues.
func (s *Server) runBoardSyncWorker(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncAllEnabledIntegrations(ctx)
		}
	}
}

func (s *Server) syncAllEnabledIntegrations(ctx context.Context) {
	ris, err := s.forgeIntegrations.ListSyncEnabled(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("board sync: list sync-enabled integrations: %v", err)
		}
		return
	}
	for _, ri := range ris {
		c, u, err := s.syncOneIntegration(ctx, ri.TenantID, ri)
		if err != nil && s.logger != nil {
			s.logger.Warn("board sync: %s/%s: %v", ri.TenantID, ri.RepoFullName, err)
		} else if (c > 0 || u > 0) && s.logger != nil {
			s.logger.Info("board sync: %s %s → %d created, %d updated", ri.TenantID, ri.RepoFullName, c, u)
		}
	}
}
