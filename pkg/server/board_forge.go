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
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
	"github.com/SocialGouv/iterion/pkg/lease"
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
//
// It reports the window it MIRRORED and, of it, the cards created and
// rewritten — a re-import of an unchanged repo mirrors everything and writes
// nothing, which the counters have to say apart from an import that found
// nothing at all.
func ImportForgeIssues(ctx context.Context, provider forge.Provider, baseURL, token, repo string, board native.BoardStore, since time.Time, minAuthorRole string) (mirrored, created, updated int, err error) {
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
		return 0, 0, 0, fmt.Errorf("forge: provider %q is not yet supported", provider)
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return 0, 0, 0, fmt.Errorf("forge: provider %q client (%T) does not implement forge.IssueClient", provider, admin)
	}
	// connID is empty for a self-hosted import: there is no persisted forge
	// connection, and the deterministic card ID keys on provider+repo+number.
	mirrored, created, updated, err = syncForgeIssuesToBoard(ctx, ic, provider, "", repo, board, since,
		authorTrustFunc(admin, string(provider), repo, minAuthorRole, newAuthorTrust()))
	var truncated *forgeIssueWindowTruncated
	if errors.As(err, &truncated) {
		// The remedy this entry point HAS, and the only caller that has it:
		// the window is an argument here (`--since`), where the three cloud
		// readers take theirs from a high-water mark no operator surface
		// writes. The page budget the core names is a rebuild for all four.
		err = fmt.Errorf("%w, or narrow the window with --since", err)
	}
	return mirrored, created, updated, err
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
// The guard names every store the handlers below dereference,
// forgeConnections included (connAdminFor): a mount missing one of them
// would panic on the first request instead of being absent.
func (s *Server) registerBoardForgeRoutes() {
	if s.cfg.CloudBoardFor == nil || s.forgeIntegrations == nil || s.forgeConnections == nil {
		return
	}
	s.mux.Handle("PATCH /api/teams/{id}/forge/integrations/{iid}", s.requireAuth(http.HandlerFunc(s.handlePatchForgeIntegration)))
	s.mux.Handle("POST /api/teams/{id}/forge/integrations/{iid}/sync", s.requireAuth(http.HandlerFunc(s.handleSyncForgeIntegration)))
	// The five per-card routes take the board family's write rung
	// (boardWriteAuth, board_cloud_routes.go): they write the card and the
	// forge — a push, a pull request, a MERGE — and the GETs are inert under
	// it, which keeps the whole /api/v1/native family uniform.
	s.mux.Handle("POST /api/v1/native/issues/{id}/push", s.boardWriteAuth(http.HandlerFunc(s.handlePushIssueToForge)))
	s.mux.Handle("GET /api/v1/native/issues/{id}/pulls", s.boardWriteAuth(http.HandlerFunc(s.handleListIssuePulls)))
	s.mux.Handle("POST /api/v1/native/issues/{id}/pulls", s.boardWriteAuth(http.HandlerFunc(s.handleCreateIssuePull)))
	s.mux.Handle("GET /api/v1/native/issues/{id}/pulls/{number}/ci", s.boardWriteAuth(http.HandlerFunc(s.handleIssuePullCI)))
	s.mux.Handle("POST /api/v1/native/issues/{id}/pulls/{number}/merge", s.boardWriteAuth(http.HandlerFunc(s.handleMergeIssuePull)))
	s.mux.Handle("GET /api/teams/{id}/forge/integrations/{iid}/hooks", s.requireAuth(http.HandlerFunc(s.handleListIntegrationHooks)))
}

// ---------------------------------------------------------------------------
// per-repo sync toggle + manual sync (team-scoped)
// ---------------------------------------------------------------------------

// patchIntegrationReq is deliberately a ONE-FIELD request: the sync toggle
// is this endpoint's whole surface. `routing` is NOT a field here — the
// repo-bots PATCH owns the routing block (the one strict decoder, the
// org-approval gate, the blast-radius screen), and a body carrying it is
// silently dropped, the same documented absence as webhookConfigReq's. A
// handler that starts decoding routing here would bypass that gate.
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

// syncResult is what one manual pass reports to the operator who asked for
// it — the studio's banner and `iterion remote forge integrations sync`.
// Synced is the window it MIRRORED (every issue whose card now matches the
// forge), of which Created and Updated are the cards it had to write: a repo
// already in sync answers "250 synced, 0 created, 0 updated", where counting
// only the writes would answer "0 synced" and read as a sync that did nothing.
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
	// An operator click reads the same issue windows as the leased sweep and
	// the webhook projection, on whichever replica served the request: its
	// requests are charged to their own lane, or the hourly forge tally files
	// them under "other" and an operator cannot tell this spend from any
	// other interactive forge call.
	mirrored, created, updated, err := s.syncOneIntegration(withForgeLane(r.Context(), forgeLaneBoardManualSync), teamID, ri)
	if err != nil {
		httpError(w, http.StatusBadGateway, "sync failed: %v", err)
		return
	}
	writeJSON(w, syncResult{Synced: mirrored, Created: created, Updated: updated})
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
// upsertForgeCard for the column policy.
//
// It returns what the window MIRRORED — every issue in it whose card now says
// what the forge says, written or already matching — and, of those, how many
// cards were created and how many rewritten. A repo already in sync mirrors
// its whole window and writes nothing, which is what the operator-facing
// counters report (syncResult).
//
// trust classifies an issue author's repo rights: a trusted author's fresh
// open card is stamped triage:auto (fires the auto-triage), an untrusted one
// needs:approval (parked until an operator approves). nil trust fails CLOSED —
// every new card parks — because this is the budget security boundary, not an
// operator preference. It is called LAZILY, once per distinct author of a card
// this pass actually creates open: that one arm reads the verdict, and
// resolving it eagerly buys a collaborator-permission request per ENTRY of a
// window the page budget lets reach 20 000 — on a pass that may write nothing.
//
// The high-water mark (LastSyncedAt) is the caller's concern — it is the only
// piece that differs between the cloud integration store and a self-hosted
// entry point, so it stays out of this pure core. What the core owes that
// caller is the VERDICT on the window: a nil error means every page arrived
// AND every issue in them landed on the board, and nothing weaker may move a
// mark.
func syncForgeIssuesToBoard(ctx context.Context, ic forge.IssueClient, provider forge.Provider, connID, repo string, board native.BoardStore, since time.Time, trust func(ctx context.Context, login string) bool) (mirrored, created, updated int, err error) {
	b, berr := board.Board()
	if berr != nil {
		return 0, 0, 0, fmt.Errorf("read board: %w", berr)
	}
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
	refused := 0
	readErr := forEachForgeIssuePage(ctx, ic, repo, since, func(page []forge.IssueRef) {
		for _, is := range page {
			if is.IsPullRequest {
				continue // the board syncs ISSUES; PRs surface via the card PR panel
			}
			c, u, e := upsertForgeCard(board, b, openCol, doneCol, provider, connID, repo, is, func() bool { return trusted(is.Author) })
			if e != nil {
				refused++
				if err == nil {
					err = e
				}
				continue
			}
			mirrored++
			created += c
			updated += u
		}
	})
	if err != nil {
		// One refusal is kept as the cause — the others are the same outage
		// said again — but the COUNT has to travel with it: `mirrored` is the
		// operator-facing number and it drops by the refused entries with
		// nothing else saying how many the board is missing.
		// ISSUES, not entries: the loop skips every pull request the issues
		// endpoint serves before it reaches the upsert, so this denominator
		// is a strictly smaller set than the ENTRIES the read counts — and
		// the two numbers reach one operator on one line.
		err = fmt.Errorf("board sync: %d of %d issues refused, first: %w", refused, refused+mirrored, err)
	}
	// Both halves hold the caller's mark, and both have to reach its log: a
	// refused card names the board's failure, and an incomplete read is the
	// only message naming the budget that cut this repo's window.
	return mirrored, created, updated, errors.Join(err, readErr)
}

const (
	// forgeIssueSyncPerPage is one page of the incremental read: a window of N
	// entries costs about N/100 round-trips, plus N board reads — the upsert
	// reads the existing card of EVERY entry, which is an uncached FindOne per
	// card on the cloud board store — plus one collaborator-permission read
	// per distinct author of a card the pass CREATES open (the trust stamp,
	// resolved lazily — an entry already mirrored, or closed, buys
	// none). 100 is what the GitHub and GitLab clients forward verbatim
	// (pkg/forge/github/issues.go, pkg/forge/gitlab/issues.go); a
	// Gitea/Forgejo instance clamps it SERVER-SIDE to its own
	// `[api] MAX_RESPONSE_ITEMS` (50 by default), which is why the walk ends
	// on an EMPTY page and never on a short one — and why a page count is not
	// an entry count.
	forgeIssueSyncPerPage = 100

	// forgeIssueSyncMaxPages bounds one SWEEP read, so a pass cannot outlive
	// its own ticker or spend an installation's whole request budget on a
	// single repo. It is sized on the FIRST sweep of a repo, which has no mark
	// and therefore reads the repo's entire history: 200 pages is 20 000
	// ENTRIES — issues AND pull requests, which the issues endpoint conflates
	// on GitHub and Forgejo — past the largest repo this syncs, where the
	// steady-state window is a handful.
	forgeIssueSyncMaxPages = 200

	// forgeIssueSyncFastPathMaxPages bounds the WEBHOOK projection's read. The
	// projection runs on whichever replica took the delivery — every replica,
	// under no lease — inside a 30 s ceiling sized for a single call
	// (scheduleForgeBoardProjection), so an unbounded read makes every
	// delivery pay whatever requests fit that ceiling and mark nothing. Ten
	// pages is a thousand entries, past any window a few minutes of activity
	// produces; a wider one is the leased sweep's, which reads it under no
	// ceiling, and the card the delivery named waits for that sweep rather
	// than every delivery re-reading the repo's history. The bound counts
	// PAGES: a first delivery for a repo with no mark also pays the trust
	// read of every card it creates open (see forgeIssueSyncPerPage), once
	// for that repo's history; a steady delivery creates almost nothing.
	forgeIssueSyncFastPathMaxPages = 10

	// forgeIssueSyncMarkOverlap is how far BEHIND the read's start the mark is
	// stamped. The mark is stamped from this process's clock but it is only
	// ever spent as the forge's `since`, which the forge evaluates against
	// `updated_at` — its own clock, served from its own list index. A mark
	// equal to the read's start therefore declares synced the part of that
	// interval the forge dated earlier than this pod (clock skew) or had not
	// yet served (an index that lags its writes), and `since` is the only way
	// back to those entries: they are lost for good, in silence. The overlap
	// re-reads that much of the interval on every pass, which buys back any
	// disagreement smaller than it; it is free in neither axis but cheap in
	// both — a card the forge did not change is not rewritten, and a mirrored
	// entry buys no collaborator-permission read, so the overlap costs the
	// pages its extra entries fill plus one board read each. A disagreement
	// WIDER than the overlap still loses entries; nothing short of a mark
	// stamped in the forge's own clock domain closes that.
	forgeIssueSyncMarkOverlap = time.Minute
)

type forgeIssuePageBudgetKey struct{}

// withForgeIssuePageBudget bounds the issue read of every sync run under ctx
// to pages. Unset — the sweep, the manual Sync-now endpoint, the self-hosted
// import — means forgeIssueSyncMaxPages.
func withForgeIssuePageBudget(ctx context.Context, pages int) context.Context {
	return context.WithValue(ctx, forgeIssuePageBudgetKey{}, pages)
}

// forgeIssuePageBudgetOf reads the budget withForgeIssuePageBudget put on ctx,
// defaulting to forgeIssueSyncMaxPages.
func forgeIssuePageBudgetOf(ctx context.Context) int {
	if pages, _ := ctx.Value(forgeIssuePageBudgetKey{}).(int); pages > 0 {
		return pages
	}
	return forgeIssueSyncMaxPages
}

// forgeIssueWindowTruncated is a read the caller's page budget cut short: the
// pages that arrived were handed on, the rest of the window was never read.
//
// A TYPE and not a sentence to match, because the remedy differs per entry
// point and only the entry point knows it: `iterion issue import` can narrow
// the window it asked for (--since), the three cloud readers take theirs from
// the integration's high-water mark, which no operator surface writes. The
// message therefore states only what holds everywhere — how much was read,
// under which budget — and each entry point adds its own (ImportForgeIssues).
type forgeIssueWindowTruncated struct {
	// Entries is what the read actually saw: issues AND, on GitHub and
	// Forgejo, the pull requests that endpoint serves alongside them. Not
	// pages × per-page: an instance that clamps the page size serves fewer.
	Entries int
	// Pages is the budget this read ran under (forgeIssuePageBudgetOf).
	Pages int
}

func (e *forgeIssueWindowTruncated) Error() string {
	return fmt.Sprintf("list issues: %d entries read in %d pages and the forge has not reported the window exhausted — "+
		"the rest of it is NOT synced; raise the page budget this read ran under "+
		"(forgeIssueSyncMaxPages, forgeIssueSyncFastPathMaxPages)", e.Entries, e.Pages)
}

// forEachForgeIssuePage reads one repo's issue window page by page until the
// provider answers an EMPTY page, and hands each page to fn as it arrives, so
// the first sweep of a repo with no mark costs one page of memory rather than
// its entire history.
//
// An empty page is the only statement a provider makes about the end of a
// window. A SHORT one states nothing: the asked-for size is a request, not a
// contract — a Gitea/Forgejo instance clamps it to its own
// `[api] MAX_RESPONSE_ITEMS` (50 against the 100 asked here) — so ending on a
// short page would read a clamped FIRST page as the whole window. The cost of
// the honest rule is one extra request per repo per pass.
//
// It reports why the read is INCOMPLETE: a page the provider refused, or a
// window the caller's page budget (forgeIssuePageBudgetOf) could not exhaust.
// The pages that DID arrive have been handed to fn either way — those issues
// are valid and worth mirroring — and the error is what holds the caller's
// high-water mark: the window is only ever reachable through `since`, so a
// mark advanced over an incomplete read writes off whatever it did not read,
// for good.
func forEachForgeIssuePage(ctx context.Context, ic forge.IssueClient, repo string, since time.Time, fn func([]forge.IssueRef)) error {
	budget := forgeIssuePageBudgetOf(ctx)
	read := 0
	for page := 1; ; page++ {
		batch, err := ic.ListIssues(ctx, repo, forge.IssueListOptions{
			State: "all", Since: since, PerPage: forgeIssueSyncPerPage, Page: page,
		})
		if err != nil {
			return fmt.Errorf("list issues (page %d): %w", page, err)
		}
		fn(batch)
		read += len(batch)
		if len(batch) == 0 {
			return nil
		}
		if page >= budget {
			// The entries READ are the fact; the per-page size asked for is
			// not one — an instance that clamps it serves half of what
			// multiplying the pages by it would suggest. What a HIGH-WATER
			// MARK does about it is not stated here: this core also serves
			// `iterion issue import`, which keeps none (syncOneIntegration,
			// the one caller that stamps, names that consequence itself).
			return &forgeIssueWindowTruncated{Entries: read, Pages: budget}
		}
	}
}

// syncOneIntegration mirrors one repo's forge issues into the team board and
// stamps the integration's LastSyncedAt high-water mark. It is the cloud
// wrapper around syncForgeIssuesToBoard: it resolves the connection → issue
// client → team board, then persists the high-water mark. It is the unit both
// the manual "Sync now" endpoint and the periodic worker call.
//
// The mark is one forgeIssueSyncMarkOverlap BEFORE the instant the read
// started, written only after a sweep that read the whole window and mirrored
// all of it. Anything short of that keeps the mark where it was and retries
// next sweep: the forge is only ever asked for what changed since the mark, so
// a mark that outruns the sweep writes off every issue updated in the window
// it jumped, and nothing offers them again. Re-reading is cheap by comparison
// — an unchanged card is not rewritten.
//
// The overlap is what makes the mark honest about a window THE FORGE dates:
// stamping the read's own start declares synced the entries the forge dated
// before this pod's clock, or served only after the read (see the constant).
// A constant shift keeps each pass's mark ordered with its own start, so a
// serial sequence of passes only moves it forward.
//
// The mark is stamped through StampSynced, a single-field write that arbitrates
// nothing (see its contract in pkg/forge): a paged pass is minutes long, and
// writing back the integration this call was HANDED would revert whatever an
// operator changed while it ran. Three entry points stamp — this sweep, the
// manual "Sync now" and the webhook projection, which runs on every replica —
// so two passes in flight can stamp out of order and the mark can step back by
// as much as their start instants differ. That only ever widens the next
// window, which is a re-read.
func (s *Server) syncOneIntegration(ctx context.Context, teamID string, ri forge.RepoIntegration) (mirrored, created, updated int, err error) {
	conn, err := s.forgeConnections.Get(ctx, ri.ConnectionID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("connection: %w", err)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("admin client: %w", err)
	}
	ic, ok := admin.(forge.IssueClient)
	if !ok {
		return 0, 0, 0, forgeCapabilityErr(conn, admin, "IssueClient")
	}
	board := s.cfg.CloudBoardFor(teamID)
	if board == nil {
		return 0, 0, 0, errors.New("no board for team")
	}
	startedAt := time.Now().UTC().Add(-forgeIssueSyncMarkOverlap)
	mirrored, created, updated, err = syncForgeIssuesToBoard(ctx, ic, conn.Provider, ri.ConnectionID, ri.RepoFullName, board, ri.LastSyncedAt,
		authorTrustFunc(admin, string(conn.Provider), ri.RepoFullName, ri.MinAuthorRole, s.authorTrustGate()))
	if err != nil {
		// The mark belongs to this wrapper, so its consequence is this
		// wrapper's to name: the core serves a self-hosted import that keeps
		// no mark and has no next pass.
		return mirrored, created, updated, fmt.Errorf("%w — the high-water mark stays put, so the next pass retries from the same mark", err)
	}
	if uerr := s.forgeIntegrations.StampSynced(ctx, ri.ID, startedAt); uerr != nil {
		return mirrored, created, updated, uerr
	}
	return mirrored, created, updated, nil
}

// upsertForgeCard creates or updates the board card mirroring one forge issue.
// On create the card lands in openCol (or doneCol when already closed); a
// fresh OPEN card is additionally stamped with the trust label (triage:auto
// for a trusted author, needs:approval otherwise — never on closed issues,
// never re-stamped on update).
//
// trusted is that one arm's classifier and is called only there, at most once:
// resolving it costs the caller a forge round-trip per distinct author
// (authorTrust), so an entry whose card already exists, or whose issue is
// closed, must not buy one. A nil classifier trusts nobody, the same
// fail-closed rule as the sync core's nil trust.
//
// On update the card's content/labels refresh but its COLUMN is left to the
// operator — except a forge-close pulls a still-open card to the terminal
// column — and board-local label namespaces are preserved (forge labels are
// mirrored verbatim; triage:/needs:/cmd:/source: labels belong to the board
// and must survive the sweep). Returns (created, updated) as 0/1 flags.
func upsertForgeCard(board native.BoardStore, b *native.Board, openCol, doneCol string, provider forge.Provider, connID, repo string, is forge.IssueRef, trusted func() bool) (int, int, error) {
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
	if gerr != nil && !errors.Is(gerr, tracker.ErrNotFound) {
		// Only the not-found sentinel means "no card yet" — both twins answer
		// a missing card with it and wrap everything else. Creating on any
		// error would have this import write blind through a store outage.
		return 0, 0, fmt.Errorf("board sync: read card %s: %w", cardID, gerr)
	}
	if gerr != nil || existing == nil {
		col := openCol
		labels := is.Labels
		if is.State == "closed" && doneCol != "" {
			col = doneCol
		} else {
			trustLabel := native.LabelNeedsApproval
			if trusted != nil && trusted() {
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
	// Patch.External REPLACES the whole ref, so the project sync state — which
	// this import knows nothing about and never sets — has to be carried
	// across. Dropping it would silently reset every card on any plain issue
	// import: the next project pass would read "first sight" and overwrite a
	// native move it should have pushed to the board instead.
	if existing.External != nil && existing.External.Project != nil {
		p := *existing.External.Project
		ext.Project = &p
	}
	updated := 0
	// Write only a card the forge actually changed. Patch.Labels and
	// Patch.External replace wholesale, so the store marks them changed
	// whatever they hold (native.Store.Update) — an unconditional Update
	// rewrites the card and emits issue_updated on EVERY pass, for every card
	// in the window, and a mark held by a refused card or a truncated window
	// re-reads that window every sweep.
	if existing.Title != is.Title || existing.Body != is.Body ||
		!slices.Equal(labels, existing.Labels) || !sameForgeRef(ext, existing.External) {
		if _, err := board.Update(cardID, native.Patch{
			Title:    &is.Title,
			Body:     &is.Body,
			Labels:   &labels,
			External: ext,
		}); err != nil {
			return 0, 0, err
		}
		updated = 1
	}
	if is.State == "closed" && doneCol != "" && !isTerminalState(b, existing.State) {
		// CAS on the snapshot: an operator who moved the card between our
		// read and this write wins — the sync must not clobber a fresh
		// human decision with a stale forge fact.
		if _, _, err := board.SetStateFrom(cardID, existing.State, doneCol); err != nil {
			return 0, 0, err
		}
		updated = 1
	}
	return 0, updated, nil
}

// sameForgeRef reports whether a card's stored forge linkage already says what
// this sync would write, which is what lets an unchanged card go unwritten.
//
// Every field participates: the project sync state through its OWN Equal — its
// timestamps denote instants, and == on a time.Time compares representations
// (a monotonic reading, a location) — and every other field by value, which a
// field added to the ref joins for free. A non-comparable field added there
// stops the build rather than slipping through uncompared.
func sameForgeRef(a, b *native.ExternalRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.Project == nil) != (b.Project == nil) {
		return false
	}
	if a.Project != nil && !a.Project.Equal(*b.Project) {
		return false
	}
	x, y := *a, *b
	x.Project, y.Project = nil, nil
	return x == y
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
//
// It reads the DEFAULTS, which is exactly what every stored binding carries:
// forge.BindRequest.LabelFields is reachable from no surface (see its own
// doc). Wiring one means feeding the binding's prefixes here too, or the next
// issue import strips the labels the project import just wrote.
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
		writeForgePullError(w, "list pull requests", err)
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
		writeForgePullError(w, "create pull request", err)
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
		writeForgePullError(w, "merge pull request", err)
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
		// forgeAdminFor is network-free in BOTH branches — the App client is
		// constructed and cached (tokens are minted later, on use) and the
		// token branch only unseals — so a failure here never reached the
		// forge. 502 would send the operator to the provider's status page
		// for our own outage.
		httpError(w, http.StatusInternalServerError, "admin client: %v", err)
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
		writeForgePullError(w, "get pull request", err)
		return
	}
	ref := pr.HeadSHA
	if ref == "" {
		ref = pr.SourceBranch
	}
	status, err := pc.GetCIStatus(r.Context(), repo, ref)
	if err != nil {
		writeForgePullError(w, "get ci status", err)
		return
	}
	// The two halves are read independently and only the STATUS one is the
	// verdict: it answered, so the panel answers. A failed history read is
	// named rather than dropped — an empty timeline is indistinguishable
	// from "nothing ever ran", which is the wrong thing to tell an operator
	// looking at a merge decision.
	history, histErr := pc.ListCIHistory(r.Context(), repo, ref, 20)
	historyError := ""
	if histErr != nil {
		history, historyError = nil, histErr.Error()
		if s.logger != nil {
			s.logger.Warn("board forge: ci history for %s@%s: %v", repo, ref, histErr)
		}
	}
	writeJSON(w, struct {
		Status  forge.CIStatus `json:"status"`
		History []forge.CIRun  `json:"history"`
		// HistoryError names why the history half is missing; absent when
		// the read succeeded.
		HistoryError string `json:"history_error,omitempty"`
	}{Status: status, History: history, HistoryError: historyError})
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
		// Network-free failure — see the sibling note above; 500, not 502.
		httpError(w, http.StatusInternalServerError, "admin client: %v", err)
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
		httpError(w, http.StatusNotImplemented, "%v", forgeCapabilityErr(conn, admin, "IssueClient"))
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
		httpError(w, http.StatusNotImplemented, "%v", forgeCapabilityErr(conn, admin, "PullClient"))
		return nil, forge.Connection{}, false
	}
	return pc, conn, true
}

// writeForgePullError answers a failed PullClient call on the card's PR/CI
// panel. A *forge.PermissionError is a configuration gap on the connection —
// its credential lacks a named grant (an App installation approved before
// `checks: read` was requested, a fine-grained PAT short of a permission) —
// so it is answered 422 with the permission and the operator step, the same
// mapping the create-repo and security-read routes give a withheld
// installation grant.
//
// A *forge.NotFoundError is the forge saying it has no such object under this
// credential — a PR that was deleted, a ref that moved, or (GitHub answers
// 404 rather than 403 for what a credential may not see) a grant that was
// never approved. It is answered 404 with the operation and the grants it is
// gated on, not the 502 a bare sentinel used to produce. Anything else is the
// upstream failure it always was.
// Both cases, and the rate limit and upstream 5xx behind them, come from the
// one shared table (forgeUpstreamStatus); the 502 stays the answer for what
// that table does not recognise as an answer from the forge.
func writeForgePullError(w http.ResponseWriter, op string, err error) {
	if writeForgeUpstreamError(w, err, "%s: %v", op, err) {
		return
	}
	httpError(w, http.StatusBadGateway, "%s: %v", op, err)
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
	// The read is bounded tighter than the sweep's: this runs on whichever
	// replica took the delivery, under a 30 s ceiling sized for a single call
	// (scheduleForgeBoardProjection), so a wide window would otherwise cost
	// every delivery whatever requests fit that ceiling — and mark nothing,
	// since an incomplete read holds the mark. Past the bound the window is
	// reported and left to the leased sweep, which reads it whole.
	ctx = withForgeIssuePageBudget(ctx, forgeIssueSyncFastPathMaxPages)
	// On its own tally lane: this spend is per DELIVERY and per replica, which
	// is not what the leased sweep's line measures.
	ctx = withForgeLane(ctx, forgeLaneBoardWebhookProjection)
	ris, err := s.forgeIntegrations.ListSyncEnabledForRepo(ctx, repo)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("board projection: list sync-enabled integrations: %v", err)
		}
		return
	}
	for _, ri := range ris {
		// Short-circuit on shutdown so a webhook that matched N integrations
		// does not run N Mongo trips each failing on ctx.Done during a
		// SIGTERM — the join budget covers the projection collectively, but
		// only the loop iteration checking ctx cuts the tail.
		if err := ctx.Err(); err != nil {
			return
		}
		_, c, u, serr := s.syncOneIntegration(ctx, ri.TenantID, ri)
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

const (
	// forgeBoardSyncInterval is how often the sweep mirrors every sync-enabled
	// repo. It is the reconciliation net's cadence, not the board's latency:
	// an inbound webhook projects its own repo immediately
	// (projectForgeWebhookToBoard).
	forgeBoardSyncInterval = 5 * time.Minute

	// forgeBoardSyncLeaseTTLFactor sizes the sweep's lease in sweep intervals.
	// The holder renews every interval and steps down before the expiry of the
	// last renewal it can prove; a holder that dies without releasing is
	// replaced at most TTL + one retry after its last renewal. The webhook
	// projection is unleased and keeps running on every replica, so that gap
	// delays the net, never the board's reaction to a live event — the
	// projection reads the same windows, which is why its own read is bounded
	// (forgeIssueSyncFastPathMaxPages).
	forgeBoardSyncLeaseTTLFactor = 3
)

// runBoardSyncWorker mirrors every sync-enabled repo's forge issues onto its
// team board, periodically, on the ONE replica holding the board-issue-sync
// lease — and campaigns for it on every other. Cloud-only; started from server
// start alongside the forge/OAuth refresh workers.
//
// The election is inside, not around, so there is no unelected entry point to
// start by mistake. What every replica sweeping cost is the forge's request
// budget: a pass lists EVERY sync-enabled repo's issue window, and the
// installation pays that once per replica. The sweep is idempotent — it
// re-reads before it writes — so the lease only has to make the cost one
// replica's while keeping the net's reach: a holder that stops releases on its
// way out, one that dies is outlived by the TTL, and each term opens with a
// pass.
func (s *Server) runBoardSyncWorker(ctx context.Context, interval time.Duration) {
	err := lease.Run(ctx, s.leases, lease.Spec{
		Name:  leaseBoardIssueSync,
		Owner: s.replicaID,
		TTL:   forgeBoardSyncLeaseTTLFactor * interval,
		Retry: interval,
		Info:  s.infof,
		Warn:  s.warnf,
	}, func(ctx context.Context) { s.boardSyncPasses(ctx, interval) })
	if err != nil {
		s.warnf("forge→board issue sync NOT started: %v — a forge issue whose webhook was dropped never reaches its team board", err)
	}
}

// boardSyncPasses sweeps every sync-enabled integration on interval until the
// term ends. Best-effort: one integration's failure is logged and the sweep
// continues.
//
// A term opens with a pass rather than with a wait. The loop only ever runs
// inside an elected term, and a term that ended before its first tick — a
// lease-store blip, a rollout — would otherwise leave the net never actually
// sweeping. Terms are paced by the campaign's retry (one interval), so this
// cannot sweep faster than the interval either.
func (s *Server) boardSyncPasses(ctx context.Context, interval time.Duration) {
	// Every forge request of every pass is charged to the sweep's own lane:
	// the hourly tally is where an operator reads what this net spends, and
	// that request budget is what the lease exists to protect.
	ctx = withForgeLane(ctx, forgeLaneBoardIssueSync)
	t := time.NewTicker(interval)
	defer t.Stop()
	s.syncAllEnabledIntegrations(ctx)
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
		// Short-circuit on a term that ended — a step-down, a rollout, a
		// SIGTERM — so a pass that matched N repos does not run N connection
		// reads each failing on ctx.Done, one warn line apiece.
		if err := ctx.Err(); err != nil {
			return
		}
		_, c, u, err := s.syncOneIntegration(ctx, ri.TenantID, ri)
		if err != nil && s.logger != nil {
			s.logger.Warn("board sync: %s/%s: %v", ri.TenantID, ri.RepoFullName, err)
		} else if (c > 0 || u > 0) && s.logger != nil {
			s.logger.Info("board sync: %s %s → %d created, %d updated", ri.TenantID, ri.RepoFullName, c, u)
		}
	}
}
