package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The READ half of the per-run forge-publish grant.
//
// A run's delivery tail has a decision to take before it acts: is the pull
// request it is about to push onto, or post a verdict on, still open? Nothing
// in a workspace can answer it — a squash merge leaves the source branch
// present and its head no ancestor of the base, so git alone reads a merged
// pull request as an open one — and the run deliberately holds no forge
// credential (see forge_publish.go). So the server answers, under the same
// token, the same repo scope and the same connection as the write half.
//
// Bot-agnostic by construction: the endpoint knows a grant and a pull-request
// URL, never which bot is asking or what it will do with the answer.

// forgePullRequestResponse is the pull request as the grant's connection sees
// it, reduced to what a delivery tail decides on.
type forgePullRequestResponse struct {
	// Open is the DECISION, precomputed so every caller resolves an
	// unreported state the same way: a provider that names no state has said
	// nothing, and reading silence as a closure would strand a run's work on
	// a guess. Open is therefore true for both "open" and "".
	Open bool `json:"open"`
	// State is the provider's own word ("open" | "closed" | "merged", or ""
	// when it reports none) — what a message to a human quotes.
	State string `json:"state"`
	// HeadSHA is the revision the pull request currently points at, so a tail
	// can tell "my work is on this head" from "the head moved under me".
	HeadSHA string `json:"head_sha,omitempty"`
	// SourceBranch / TargetBranch name the branches a tail pushes onto and
	// diffs against.
	SourceBranch string `json:"source_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	// Number is the pull request's own number, echoed for the caller's logs.
	Number int `json:"number,omitempty"`
}

// handleForgePullRequest answers the state of the pull request a run's grant
// covers. GET, read-only, and scoped exactly as the publish endpoint is: the
// token IS the authority (a run carries no JWT), the grant's repo bounds what
// it may read, and the connection must belong to the grant's team and serve
// the URL's host.
func (s *Server) handleForgePullRequest(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Iterion-Run")
	if token == "" {
		httpError(w, http.StatusUnauthorized, "missing X-Iterion-Run header")
		return
	}
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge publishing is not enabled on this server (no forge connections wired)")
		return
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		httpError(w, http.StatusUnauthorized, "unknown or expired run token")
		return
	}
	prURL := strings.TrimSpace(r.URL.Query().Get("pr_url"))
	if prURL == "" {
		httpError(w, http.StatusBadRequest, "pr_url is required")
		return
	}
	host, repo, number, err := forge.ParsePullURL(prURL)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !strings.EqualFold(repo, grant.Repo) {
		httpError(w, http.StatusForbidden, "run token is scoped to repo %q, not %q", grant.Repo, repo)
		return
	}
	conn, err := s.forgeConnections.Get(r.Context(), grant.ConnectionID)
	if err != nil || conn.TenantID != grant.TeamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		httpError(w, http.StatusBadRequest, "pr_url host %q is not on the connection's forge host", host)
		return
	}
	gc, err := s.gateClientFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusBadGateway, "forge client: %v", err)
		return
	}
	if gc == nil {
		httpError(w, http.StatusNotImplemented, "provider %s cannot read pull requests", conn.Provider)
		return
	}
	pr, err := gc.GetPullRequest(r.Context(), repo, number)
	if err != nil {
		// Explicit, never a default answer: a tail told "open" because the
		// forge was unreachable would push onto a branch nobody checked. The
		// shared table says WHICH refusal it was — a rate limit the caller
		// may wait out, a grant it will never get — instead of one 502 for
		// every cause.
		if !writeForgeUpstreamError(w, err, "read pull request %s#%d: %v", repo, number, err) {
			// Default, and a clean one. This route reads and nothing
			// else, so no post-forge store write can mix in the way the
			// avatar route's does — and the read's own local half is
			// covered: an App's GetPullRequest goes through scopedREST,
			// which mints an installation token and signs the App JWT
			// before opening a socket, and those failures carry
			// forge.ErrLocalPreflight, which writeForgeUpstreamError
			// answers 500. What reaches here is an unclassified failure
			// of a round trip that did happen.
			//
			// This ARM, not the route: `forge client:` above still
			// answers 502 for a gateClientFor failure, which touches no
			// network at all — an App config that did not resolve, a
			// token that will not unseal. The sibling list-repos route
			// answers 500 for the identical failure, and board_forge /
			// forge_publish answer 502 like this one. Nothing marks
			// those and no marker would reach them (the arms never ask
			// the junction), so aligning them is its own change across
			// four routes. Residual on #969.
			httpError(w, http.StatusBadGateway, "read pull request %s#%d: %v", repo, number, err)
		}
		return
	}
	writeJSON(w, forgePullRequestResponse{
		Open:         pr.State == "" || pr.State == "open",
		State:        pr.State,
		HeadSHA:      pr.HeadSHA,
		SourceBranch: pr.SourceBranch,
		TargetBranch: pr.TargetBranch,
		Number:       number,
	})
}
