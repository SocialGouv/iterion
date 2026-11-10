package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The fork review lane's ADMISSION half (ADR-162 U6): a maintainer's
// /command gesture on a fork pull request admits ONE commit of a PUBLIC
// base repository, on a config that opted in at creation (ForkLane,
// github-only, immutable). Everything the admitted run is denied was
// landed by U1–U5; this file is the gesture that produces the run.
//
// The opt-in is a GESTURE, never a record: iterion stores no "this pull
// request is admitted" row, because a stored opt-in is a standing grant
// and a standing grant goes stale — the gesture would have to be
// invalidated against a push the contributor can make a second later, and
// every such scheme is a race. One gesture authorises one review of one
// commit; the runner refuses the run when the fetch lands elsewhere.
//
// Delivery-row reasons: every refusal below is a TYPED, stable reason
// recorded on the delivery row (200/filtered — a 4xx would teach the
// forge to disable the hook). The rows, and the witness that replays
// each, are the table in docs/webhooks.md § "The fork review gesture".

// forkRefusalRoute: no route on the lane's own table.
const forkRefusalRoute = "fork gesture: no route — the lane routes /revi (and its own command table); /%s has neither"

// forkRefusalBoardMode: a board-mode route cannot carry a fork launch —
// the card holds no seat for the provenance (the same refusal
// dispatchInvocation makes, named here so the row is the gesture's own).
const forkRefusalBoardMode = "fork gesture: a board-mode route cannot carry a fork launch — a card holds only bot args, so the run it produces would read as trusted and unpinned"

// forkRefusalSurface: the command arrived on an issue, not a pull request.
const forkRefusalSurface = "fork gesture: the command is not enabled on %s comments — the gesture reviews a fork pull request"

// forkRefusalBotScope: the routed bot is outside the webhook's scope.
const forkRefusalBotScope = "fork gesture: bot %q is not permitted by this webhook"

// forkRefusalHeld: Q2 — the hold label BLOCKS the gesture, because the
// base repository's configuration may stop a fork run, never steer one.
const forkRefusalHeld = "fork gesture: hold label %q — the base repository's configuration can stop a fork run, never steer it; remove the label to admit gestures"

// forkRefusalNonWriter: decision 2 — live forge permission only, never
// below developer, and the AuthorizedRepliers login short-circuit is not
// consulted because a login is renameable.
const forkRefusalNonWriter = "fork gesture: %s holds no write-level permission on %s — the lane authorizes the commenter's live repository permission only (never the replier allowlist: a login is renameable), and never below the developer role"

// forkRefusalClosed: the pull request is not open.
const forkRefusalClosed = "fork gesture: the pull request is %s — one gesture reviews one commit of an OPEN pull request"

// forkRefusalSameRepo: decision 3 — a PROVEN fork only. A same-repo pull
// request is served by the repository's ordinary webhook.
const forkRefusalSameRepo = "fork gesture: same-repo pull request — the fork review lane admits a PROVEN fork only, and this repository's own pull requests are served by its ordinary webhook, which holds the review capabilities this lane deliberately does not"

// forkRefusalHeadWithheld: the forge declared a head repository and did
// not name it — a deleted or blocked fork has exactly this shape.
const forkRefusalHeadWithheld = "fork gesture: head repo withheld — the forge declared one and did not name it (a deleted or blocked fork has exactly this shape), so the fork cannot be proven"

// forkRefusalHeadNotNamed: the forge's answer names no head repository at
// all — same-repo is never assumed, only proven, and so is a fork.
const forkRefusalHeadNotNamed = "fork gesture: head repo not named — a fork is never assumed, only proven, and an answer that names no head repository proves neither a fork nor a same-repo pull request"

// forkRefusalPrivateBase: decision 3 — a PUBLIC base only. Refused
// BEFORE the per-author bound is metered (the launch tail meters; every
// row in this file precedes it).
const forkRefusalPrivateBase = "fork gesture: the base repository is not PROVEN public (visibility=%q) — an untrusted clone holds no forge token, and one handed to it would persist in the fork's workspace; a private base needs a runner-owned clone, which is out of scope (#874's remainder)"

// forkRefusalNoAuthor: decision 5's key — the bound meters on the NUMERIC
// forge id, and a launch that cannot name it cannot be metered.
const forkRefusalNoAuthor = "fork gesture: the pull request names no numeric author id — the per-contributor bound is keyed on it (a login is renameable), so the gesture is refused rather than the author's budget left unbounded"

// forkRefusalNoHead: the pull request names no head commit to pin.
const forkRefusalNoHead = "fork gesture: the pull request names no head commit — the gesture pins the commit it admits, and a pin against nothing disables the runner's comparison entirely"

// forkRefusalStaleSHA: the optional SHA argument narrows the gesture to
// that commit; a head that moved needs a new gesture.
const forkRefusalStaleSHA = "fork gesture: the gesture pinned %s but the pull request's head is %s — a new head needs a new gesture (the runner would refuse the fetch anyway, naming both commits)"

// forkRefusalBadArgs: /revi takes an optional commit SHA, nothing else.
const forkRefusalBadArgs = "fork gesture: /revi takes an optional commit SHA to pin, nothing else — %q is neither a SHA nor empty"

// forkGestureDefaultBot is the lane's default route target: the read-only
// reviewer. The lane's OWN table (decision 1): the config's provisioned
// CommandMap wins when it names the command; /revi maps here otherwise.
const forkGestureDefaultBot = "review-pr"

// forkGestureMinRoleRank is the floor decision 2 sets: never below
// developer (GitHub's "write"), whatever the route or webhook named
// lower. ReplierRoleRank("developer") == 3 == prforgePermRank("write").
const forkGestureMinRoleRank = 3

// handleForkReviewGesture is the fork-lane config's whole comment lane:
// route by the lane's own table, gate on the commenter's LIVE repository
// permission (never the login allowlist), resolve the pull request
// through the forge API, admit one commit of a proven public-base fork,
// and launch on the base's clone URL and the base's own pull-request head
// ref — the pair that names ONE repository. Every refusal is a typed
// delivery row; every check precedes the launch tail, so every refusal
// also precedes the metering of either budget.
func (s *Server) handleForkReviewGesture(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	cfg webhooks.Config,
	provider webhooks.Provider,
	p prforge.ParsedNote,
	cmd, cmdArgs string,
	payloadHash, srcIP string,
) {
	meta := prforgeNoteMeta(p)
	filtered := func(reason string) {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP, reason)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered, "reason": reason})
	}
	// The lane is github-only (decision 1, refused at creation); asserted
	// here too because this handler is reachable by any provider's config
	// the store may hold — a hand-edited row must not become a half-built
	// path on a provider whose head-repository proof has no witness.
	if provider != webhooks.ProviderGitHub {
		filtered(forkLaneProviderRefusal(provider).Error())
		return
	}
	// The gesture is a PULL REQUEST comment: an issue carries no head to
	// admit.
	if p.Surface() != "pr" {
		filtered(fmt.Sprintf(forkRefusalSurface, p.Surface()))
		return
	}
	// The lane's own routing table (decision 1): the provisioned CommandMap
	// when it names the command, else the built-in default /revi → the
	// read-only reviewer. A fork-lane config serves no other default —
	// every command it does not name is filtered, not guessed.
	route, ok := forkGestureRoute(cfg, cmd)
	if !ok {
		filtered(fmt.Sprintf(forkRefusalRoute, cmd))
		return
	}
	if !route.AllowsScope(p.Surface()) {
		filtered(fmt.Sprintf(forkRefusalSurface, p.Surface()))
		return
	}
	if !cfg.AllowsBot(route.BotID) {
		filtered(fmt.Sprintf(forkRefusalBotScope, route.BotID))
		return
	}
	if route.Mode == "board" {
		filtered(forkRefusalBoardMode)
		return
	}
	// Q2: the hold label blocks the gesture. Every other /command lane
	// deliberately skips the hold veto (a label pauses automation, not a
	// manual trigger); this lane is the exception the operator decided,
	// because the base's configuration is the one per-PR stop a fork run
	// has — everything else the repo could steer was withdrawn by decision 4.
	if held := webhooks.HeldByLabel(cfg.HoldLabels, p.Labels); held != "" {
		filtered(fmt.Sprintf(forkRefusalHeld, held))
		return
	}
	// Resolve the forge client the same way every GitHub lane does: the
	// covering team connection first, the webhook's forge_token binding as
	// the fallback. One client serves the gate, the pull-request read and
	// the Q1 note, so all three speak as the same identity.
	apiResolve := s.webhookForkGestureAPI
	if apiResolve == nil {
		apiResolve = s.realWebhookForkGestureAPI
	}
	api, apiRefusal := apiResolve(ctx, cfg, provider, p, route.BotID)
	if apiRefusal != "" {
		s.failWebhookAuthorization(ctx, w, cfg, meta, payloadHash, srcIP, "fork gesture forge client", fmt.Errorf("%s", apiRefusal))
		return
	}
	// Decision 2: who may gesture. The self-comment loop guard stays (the
	// note the lane posts would re-trigger itself otherwise), and the
	// authorization is the commenter's LIVE permission on the BASE
	// repository, floored at developer (write). The AuthorizedRepliers
	// login short-circuit is deliberately NOT consulted: a login is
	// renameable — free, instant and unlimited on GitHub — so a
	// login-keyed authorization resets itself on demand.
	authorized, reason, aerr := forkGestureAuthorize(ctx, api, p, route, cfg)
	if aerr != nil {
		s.failWebhookAuthorization(ctx, w, cfg, meta, payloadHash, srcIP, "fork gesture authorization", aerr)
		return
	}
	if !authorized {
		filtered(reason)
		return
	}
	if s.logger != nil {
		s.logger.Debug("webhooks: fork gesture %s#%d (/%s) by %s → %s", p.ProjectPath, p.IssueNumber, cmd, p.AuthorLogin, route.BotID)
	}
	// Decision 3: resolve the pull request through the forge API, at the
	// moment of the gesture — the payload's snapshot is neither fresher
	// nor safer than the API's, and the head commit this admits is the
	// API's. A resolution failure is a visible launch error (502), never
	// a silent fall-back: the forge redelivers.
	pr, rerr := api.GetPullRequest(ctx, p.ProjectPath, int(p.IssueNumber))
	if rerr != nil {
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, payloadHash, srcIP, "fork gesture PR resolution: "+rerr.Error())
		httpError(w, http.StatusBadGateway, "could not resolve the pull request")
		return
	}
	// The admission rows, every one BEFORE the launch tail meters either
	// budget. Order: cheapest and most decidable first; the private base
	// row is proven before the author is even read, so "the private base
	// refused before the bound is metered" holds by structure.
	if pr.State != "open" {
		filtered(fmt.Sprintf(forkRefusalClosed, pr.State))
		return
	}
	if pr.SameRepoAs(p.ProjectPath) {
		filtered(forkRefusalSameRepo)
		return
	}
	if pr.HeadRepoWithheld() {
		filtered(forkRefusalHeadWithheld)
		return
	}
	if pr.HeadRepoFullName == "" {
		filtered(forkRefusalHeadNotNamed)
		return
	}
	if pr.BaseVisibility != "public" {
		filtered(fmt.Sprintf(forkRefusalPrivateBase, pr.BaseVisibility))
		return
	}
	if pr.AuthorID == "" {
		filtered(forkRefusalNoAuthor)
		return
	}
	if pr.HeadSHA == "" {
		filtered(forkRefusalNoHead)
		return
	}
	// The optional SHA argument narrows the gesture to that commit. Empty
	// pins the head the API just named; a SHA must BE the head the API
	// just named — anything else is a stale pin, and a new head needs a
	// new gesture. Prose is not a pin: /revi on this lane takes a SHA or
	// nothing.
	pinned := pr.HeadSHA
	if arg := strings.TrimSpace(cmdArgs); arg != "" {
		if !isCommitSHA(arg) {
			filtered(fmt.Sprintf(forkRefusalBadArgs, arg))
			return
		}
		if !strings.EqualFold(arg, pr.HeadSHA) {
			filtered(fmt.Sprintf(forkRefusalStaleSHA, arg, pr.HeadSHA))
			return
		}
	}
	// The launch pair that names ONE repository: the BASE's clone URL and
	// the base's own pull-request head ref, which resolves to the fork's
	// commit inside the base's object database. The base stays the clone's
	// only remote, so nothing the contributor controls is fetched by name.
	// The API's base clone URL is authoritative; the payload's repository
	// is the same base by construction (the comment event fired on it).
	cloneURL := pr.BaseCloneURL
	if cloneURL == "" {
		cloneURL = p.CloneURL
	}
	repoRef := fmt.Sprintf("refs/pull/%d/head", pr.Number)
	// The vars a review needs, with NEITHER webhook-level var layer:
	// applyWebhookVarLayers refuses to layer them onto an untrusted target
	// (decision 4), and the call is here so the floor is crossed, not
	// bypassed. The operator's routing-narrowing names are withheld with
	// them (trustedVarNamesFor, at the launcher).
	vars := applyWebhookVarLayers(map[string]string{
		"pr_url":        p.PRURL,
		"base_ref":      pr.TargetBranch,
		"scope_notes":   strings.TrimSpace(p.IssueTitle + "\n\n" + p.IssueBody),
		"post_to_board": "false",
		// Inline review anchors are the bot's own formatting choice; the
		// fork run holds no publish grant either way, and publish_review
		// skips with its explicit reason when the grant vars are absent.
		"pr_review_mode": "inline",
		"head_sha":       pinned,
		"pr_author":      pr.Author,
	}, cfg, store.RunTrustFork)
	// One gesture, one launch, one comment: the idem key is per comment,
	// the "forkcmd|" prefix keeping its space disjoint from every other
	// lane's on the same config.
	idemKey := knowledge.ChecksumHex([]byte(fmt.Sprintf("forkcmd|%s|%s|%s|%s", cfg.TenantID, cfg.ID, p.ProjectPath, p.SubjectID())))
	target := forgeLaunchTarget{
		BotID:   route.BotID,
		IdemKey: idemKey,
		Vars:    vars,
		RepoURL: cloneURL,
		RepoRef: repoRef,
		// What the admission proved: a fork's workspace, this one commit.
		// The tail cross-checks the pairing against the config's lane kind
		// before anything is metered, and refuses an unpinned untrusted
		// target outright.
		Trust:       store.RunTrustFork,
		ExpectedSHA: pinned,
		// Who the per-contributor bound meters: the author's NUMERIC forge
		// id, which gateMonthlyCaps composes into the metering subject on
		// the same id the org budget used.
		ForkAuthor: &webhooks.ForkAuthorRef{Provider: string(provider), AuthorID: pr.AuthorID},
		// Q1: the note that answers the gesture where it was made — the
		// run link and the pinned commit, and nothing the run wrote (the
		// run holds no publish grant at all). Posted from the booking
		// context the tail runs the launch in, once the run id exists.
		onLaunched: func(res webhookLaunchResult) {
			if res.Status != webhooks.StatusLaunched || res.RunID == "" {
				return
			}
			s.postForkGestureNote(ctx, r, api, p, res.RunID, pinned)
		},
	}
	s.acceptWebhookTargets(ctx, w, r, cfg, meta, []forgeLaunchTarget{target}, payloadHash, srcIP)
}

// forkGestureRoute resolves a command on the fork lane's OWN table: the
// config's provisioned CommandMap when it names the command, else the
// built-in default /revi → the read-only reviewer. ok=false filters.
func forkGestureRoute(cfg webhooks.Config, cmd string) (webhooks.CommandRoute, bool) {
	if route, ok := cfg.ResolveCommand(cmd, ""); ok {
		return route, true
	}
	if strings.EqualFold(cmd, "revi") {
		return webhooks.CommandRoute{BotID: forkGestureDefaultBot, Scope: "pr"}, true
	}
	return webhooks.CommandRoute{}, false
}

// forkGestureAuthorize is decision 2's gate, the token-free core (tests
// drive it with a fake forge): the bot's own comment never authorizes
// (the Q1 note would echo back as a gesture), and a human commenter must
// hold a LIVE write-level permission on the base repository — the route's
// or the webhook's min_replier_role, floored at developer. The
// AuthorizedRepliers short-circuit is absent on purpose. Returns
// (authorized, refusalReason, err): err only for an authorization the
// gate could not EVALUATE (a configuration miss the caller surfaces as a
// launch error, never as a verdict about the commenter).
func forkGestureAuthorize(ctx context.Context, api prforgeReplierAPI, p prforge.ParsedNote, route webhooks.CommandRoute, cfg webhooks.Config) (bool, string, error) {
	id, err := api.WhoAmI(ctx)
	switch {
	case err != nil:
		return false, loopGuardUnevaluable + ": " + err.Error(), nil
	case id.Login == "":
		return false, loopGuardUnevaluable + ": the forge named no login", nil
	case strings.EqualFold(id.Login, p.AuthorLogin):
		return false, "self comment (loop-guard)", nil
	}
	perm, err := api.CollaboratorPermission(ctx, p.ProjectPath, p.AuthorLogin)
	if err != nil {
		return false, "", err
	}
	minRole := route.MinReplierRole
	if minRole == "" {
		minRole = cfg.MinReplierRole
	}
	minRank := replierMinRoleRank(minRole)
	if minRank < forkGestureMinRoleRank {
		minRank = forkGestureMinRoleRank
	}
	if prforgePermRank(perm) >= minRank {
		return true, "", nil
	}
	return false, fmt.Sprintf(forkRefusalNonWriter, p.AuthorLogin, p.ProjectPath), nil
}

// isCommitSHA reports whether s is a full-length commit SHA (40 hex on
// GitHub). The gesture pins EXACTLY: a shortened SHA names several
// commits and the runner's comparison is byte-for-byte.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// realWebhookForkGestureAPI resolves the fork gesture's forge client —
// the same resolution every GitHub/Forgejo lane reads through, one client
// for the gate, the pull request and the note.
func (s *Server) realWebhookForkGestureAPI(ctx context.Context, cfg webhooks.Config, provider webhooks.Provider, p prforge.ParsedNote, botID string) (prforgeReplierAPI, string) {
	baseURL, refusal := prforgeBaseURL(cfg, p)
	if refusal != "" {
		return nil, refusal
	}
	return s.prforgeReplierAPIFor(ctx, cfg, provider, baseURL, p.ProjectPath, botID)
}

// postForkGestureNote is Q1: the server writes a note on the fork pull
// request naming the run link and the pinned commit — and NOTHING the run
// wrote (the run holds no publish grant; its findings live on the run).
// Best-effort on a detached, bounded context: a forge refusal cannot fail
// the delivery, but it is said loudly — the maintainer's answer where the
// gesture was made is the whole point of Q1, and a silent miss reads as
// "the bot did nothing".
func (s *Server) postForkGestureNote(ctx context.Context, r *http.Request, api prforgeReplierAPI, p prforge.ParsedNote, runID, pinnedSHA string) {
	commenter, ok := api.(forgeIssueCommenter)
	if !ok {
		if s.logger != nil {
			s.logger.Warn("webhooks: fork gesture note for run %s: this forge client cannot comment on the pull request — the maintainer's answer is the delivery row and the run link it carries", runID)
		}
		return
	}
	noteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchBookingTimeout)
	defer cancel()
	runURL := deeplink.Run(s.publicBaseURL(r), runID)
	body := fmt.Sprintf("**Fork review launched.**\n\n- run: %s\n- pinned commit: `%s`\n\nThe review runs sandboxed on exactly this commit and publishes nothing else; its findings are on the run.",
		runURL, pinnedSHA)
	if _, err := commenter.CommentIssue(noteCtx, p.ProjectPath, int(p.IssueNumber), body); err != nil {
		if s.logger != nil {
			s.logger.Warn("webhooks: fork gesture note on %s#%d for run %s: %v — the run is launched; the maintainer's answer where the gesture was made could not be posted", p.ProjectPath, p.IssueNumber, runID, err)
		}
	}
}
