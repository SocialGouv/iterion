package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/deeplink"
)

// legacyStudioSegments are the first path segments the studio answered on
// before it moved under deeplink.StudioBase.
//
// The list is FROZEN. It is a record of what the URL space looked like at one
// past moment, not a mirror of the studio's route table: a route added after
// the move never had a root-level spelling to redirect, and adding one here
// would invent an address that was never published. Deleting an entry, on the
// other hand, breaks links that are already out in the world — in delivered
// mail, in posted pull-request comments, in operators' bookmarks, and in this
// repository's own docs, which cite run URLs as evidence.
//
// Deliberately ABSENT, because they still answer at the root and always will:
// /login, /auth/…, /invitations/accept, /cli-auth, /config/<id> (share links
// carry their token in the fragment), /marketplace (public), /brand/… and
// /x/… (workspace panes).
var legacyStudioSegments = []string{
	"account",
	"admin",
	"board",
	"bots",
	"config-editor",
	"dispatcher",
	"editor",
	"insights",
	"integrations",
	"orgs",
	"pipelines",
	"plugins",
	"repos",
	"runs",
	"secrets",
	"skills",
	"teams",
	"triggers",
	"whats-next",
}

// registerStudioLegacyRedirects points every published pre-move studio URL at
// its current address.
//
// Server-side rather than in the SPA: these URLs are followed by things that
// do not run the bundle — a forge rendering a commit status target, a mail
// client, `curl` in a runbook — and a 302 is the only answer all of them
// understand. The SPA's own guard covers the desktop panes, which the asset
// proxy serves without passing through here.
//
// 302 and not 301: a permanent redirect is cached by the browser until its
// storage is cleared, so a wrong one cannot be taken back. These are app deep
// links, not indexed pages; nothing is gained by making them permanent, and a
// revert would be unrecoverable for anyone who visited once.
func (s *Server) registerStudioLegacyRedirects() {
	for _, seg := range legacyStudioSegments {
		h := studioLegacyRedirect()
		// Both spellings: "GET /runs" matches that exact path, "GET /runs/"
		// matches everything beneath it. Registering only the second would
		// leave the bare list page on the SPA catch-all.
		s.mux.Handle("GET /"+seg, h)
		s.mux.Handle("GET /"+seg+"/", h)
	}
}

func studioLegacyRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := deeplink.Path(r.URL.EscapedPath())
		// The query survives: /runs/new?bot=x and ?sso_linked=… both carry the
		// only thing that makes the destination the right page.
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		if f := r.URL.Fragment; f != "" {
			target += "#" + f
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// isLegacyStudioPath reports whether p is a pre-move studio URL. Exported for
// the tests that hold the frozen list to its promise; the redirect itself is
// wired through the mux, which does the matching.
func isLegacyStudioPath(p string) bool {
	trimmed := strings.TrimPrefix(p, "/")
	head, _, _ := strings.Cut(trimmed, "/")
	for _, seg := range legacyStudioSegments {
		if head == seg {
			return true
		}
	}
	return false
}
