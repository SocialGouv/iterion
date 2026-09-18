package server

import (
	"net/http"
	"path"
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
// Deliberately ABSENT, because the SERVER still answers them at the root:
// /login, /auth/…, /invitations/accept, /cli-auth, /config/<id> (share links
// carry their token in the fragment), /marketplace (public — browsable with no
// account), /brand/… and /x/… (workspace panes).
//
// /marketplace is the one with a caveat worth stating rather than implying:
// the server keeps it at the root for everyone, and the SPA carries a visitor
// who HAS a session on to the in-studio catalogue, which has the sidebar and
// the submit surface a signed-in operator expects. Both addresses render the
// catalogue; neither 404s.
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
	h := studioLegacyRedirect()
	for _, seg := range legacyStudioSegments {
		// Both spellings: "GET /runs" matches that exact path, "GET /runs/"
		// matches everything beneath it. Registering only the second would
		// leave the bare list page on the SPA catch-all.
		s.mux.Handle("GET /"+seg, h)
		s.mux.Handle("GET /"+seg+"/", h)
	}
}

func studioLegacyRedirect() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A path that does not survive cleaning does not address what it
		// appears to. ServeMux normalises a literal "..", but leaves "%2e%2e"
		// alone — and the WHATWG URL parser every browser uses does the
		// opposite, treating "%2e%2e" as a real dot-dot segment. So
		// `/runs/%2e%2e/%2e%2e/x` reaches here, gets "/studio" prefixed, and
		// then resolves to `/x`: a redirect this product signs, whose target
		// leaves the base it just prepended. Same-origin only, so not an open
		// redirect — but a forge renders this URL as a run's status target and
		// a human reads it as a run.
		//
		// A behaviour check, not a list of spellings to recognise: whatever a
		// future encoding looks like, if it does not survive path.Clean it does
		// not get a redirect. r.URL.Path is DECODED, so "%2e%2e" is ".." here.
		// A trailing slash is the one legitimate difference Clean removes
		// (/account/ is a real published address), so it is allowed back.
		if clean := path.Clean(r.URL.Path); r.URL.Path != clean && r.URL.Path != clean+"/" {
			http.NotFound(w, r)
			return
		}
		target := deeplink.Path(r.URL.EscapedPath())
		// The query survives: /runs/new?bot=x and ?sso_linked=… both carry the
		// only thing that makes the destination the right page.
		//
		// The fragment needs no code: a request never carries one (RFC 9110 —
		// the client strips it before sending), and a Location without a
		// fragment inherits the one the client still holds. An earlier version
		// of this handler appended r.URL.Fragment, which net/http never
		// populates from a request line — a dead branch that read as the
		// mechanism.
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
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
