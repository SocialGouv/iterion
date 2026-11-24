// Package deeplink builds the URLs that point a human at a page of the studio.
//
// The studio is served under a base path so that "/" can serve the product
// home instead: every studio route lives under StudioBase. That prefix is a
// single decision, and it is made here — six packages used to concatenate
// `base + "/runs/" + id` on their own, which is six places for the next prefix
// change to be applied in five.
//
// Callers keep their own policy about an EMPTY base: with base == "" these
// helpers return a root-relative path, which a same-origin consumer (the
// web-push service worker) resolves and an email cannot. Whether that is
// acceptable is the caller's call, exactly as it was before this package
// existed; nothing here silently turns a link into an empty string.
package deeplink

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"
)

// StudioBase is the path prefix every studio route sits under. The studio's
// own copy of this value is STUDIO_BASE in studio/src/lib/scope.ts — the two
// must agree, and pkg/server/studio_legacy_redirect.go is what makes an
// already-published URL without the prefix still resolve.
const StudioBase = "/studio"

// Studio joins base, StudioBase and a studio-relative path ("/runs/abc").
func Studio(base, path string) string {
	return strings.TrimRight(base, "/") + StudioBase + path
}

// Path is the root-relative studio path for a studio route — what a
// same-origin redirect needs when it lands an operator back in the studio (an
// OAuth callback, a post-login bounce). Path("") is the studio root.
func Path(route string) string {
	return Studio("", route)
}

// Run is the studio page for one run.
func Run(base, runID string) string {
	return Studio(base, "/runs/"+url.PathEscape(runID))
}

// Runs is the studio's run list.
func Runs(base string) string {
	return Studio(base, "/runs")
}

// RunAttachment is the API route that serves one of a run's attachments
// back to a reader: the stored bytes, with their MIME and a
// Content-Disposition, behind the run's own team gate
// (pkg/server/runs_uploads.go; `…/url` presigns the same name).
//
// An API address rather than a studio page, built HERE for the reason
// every studio route is: the path is one decision. The engine publishes
// a declared deliverable and announces where a human downloads it; the
// server owns the route; nothing else concatenates the two.
func RunAttachment(base, runID, name string) string {
	return strings.TrimRight(base, "/") + "/api/runs/" + url.PathEscape(runID) +
		"/attachments/" + url.PathEscape(name)
}

// PublicBaseEnv names the externally-reachable origin of the deployment
// (`ITERION_PUBLIC_URL`, pkg/config Auth.PublicURL). The server and the
// runner read the same configmap, so a process EXECUTING a run carries it
// too — which is what lets a run build the address of its own page.
const PublicBaseEnv = "ITERION_PUBLIC_URL"

// PublicBase is that origin as the current process sees it, without its
// trailing slash. Empty when unset. The value is NOT judged here — see
// PublicOrigin, which is what a caller publishing outside must use.
func PublicBase() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(PublicBaseEnv)), "/")
}

// ErrPublicBaseUnusable reports a PublicBaseEnv value no reader could
// follow from outside the deployment. Typed and greppable so the one
// warning an operator gets names this condition.
var ErrPublicBaseUnusable = errors.New("deeplink: " + PublicBaseEnv + " is not a usable public origin")

// PublicOrigin is PublicBase judged as an address a STRANGER will click.
//
// ("", nil) means unset — the documented "this deployment publishes no
// public address" case. ("", err) means set but unusable, and the three
// refusals are each a link that would do harm rather than nothing:
//
//   - no scheme or no host ("iterion.example"): the result is a RELATIVE
//     link. Posted in a pull-request body it resolves against the FORGE
//     and sends the reader to someone else's 404 — the exact failure the
//     empty-string policy exists to prevent.
//   - whitespace anywhere: the published link is cut at the first space.
//     The test is unicode.IsSpace, the same alphabet PublicBase trims
//     with — a no-break space pasted from a wiki or a ticket is the
//     realistic case, and an ASCII-only check would judge a value its own
//     trimmer handles differently.
//   - userinfo ("https://admin:pw@host"): publishing the link publishes
//     the credential, in a comment on someone else's repository.
//   - a query or a fragment: Studio() CONCATENATES, so "/studio/runs/<id>"
//     would land inside the query ("?utm=x/studio/runs/<id>") — right
//     host, wrong page, and nothing in the link says so. pkg/config
//     refuses both on this same value for the same reason.
//
// A path PREFIX is kept: a deployment served under one is legitimate, and
// the studio sits under that prefix too. What is returned is the PARSED
// form, not the raw env value, so the origin that was judged is the origin
// that gets published.
func PublicOrigin() (string, error) {
	base := PublicBase()
	if base == "" {
		return "", nil
	}
	if i := strings.IndexFunc(base, unicode.IsSpace); i >= 0 {
		return "", fmt.Errorf("%w: %q holds whitespace (at byte %d), which truncates every link built from it", ErrPublicBaseUnusable, base, i)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("%w: %q does not parse: %v", ErrPublicBaseUnusable, base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: %q carries no http(s) scheme, so a link built from it is relative and resolves against whatever page it was posted on", ErrPublicBaseUnusable, base)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: %q names no host", ErrPublicBaseUnusable, base)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: %q carries userinfo, and a published link would publish the credential", ErrPublicBaseUnusable, base)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q carries a query or a fragment, and a run's path appended to it would address the deployment's root instead of the run", ErrPublicBaseUnusable, base)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// RunFromEnv is the absolute studio page of a run, built from PublicOrigin.
//
// Unlike Run it states a policy for the unusable base, because its callers
// publish the result OUTSIDE the deployment (a pull-request body, a chat
// message): with no origin to stand on there is no address a reader could
// follow, so this returns "" rather than the root-relative path Run("")
// would hand back. Empty means "print the run id alone" — never a
// fabricated host. The REASON an origin was refused is reported where a
// logger exists (pkg/runtime, when an engine is built — once per run and
// once per subbot child), so a typo is a warning an operator can read and
// not a link that quietly went missing.
func RunFromEnv(runID string) string {
	base, err := PublicOrigin()
	if err != nil || base == "" || strings.TrimSpace(runID) == "" {
		return ""
	}
	return Run(base, runID)
}

// RunAttachmentFromEnv is the absolute download address of one of a
// run's attachments, built from PublicOrigin.
//
// Same policy as RunFromEnv, for a sharper reason: a root-relative path
// would be worse than nothing. The studio reaches its own API through
// apiBase() (studio/src/lib/scope.ts), which is "/x/<id>/api" inside a
// scoped pane and an absolute origin in the desktop app, so a published
// "/api/runs/…" would address the wrong server in both. Empty therefore
// means "announce no link": the file is published either way and the run
// carries run_attachment_published, which is the pointer a gate reads.
// The REASON an origin was refused is reported where a logger exists
// (pkg/runtime, once per engine).
func RunAttachmentFromEnv(runID, name string) string {
	base, err := PublicOrigin()
	if err != nil || base == "" || strings.TrimSpace(runID) == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return RunAttachment(base, runID, name)
}

// LegacyRun is the spelling Run had before the studio moved under StudioBase.
//
// It exists for READING, not for writing: URLs in this shape are already out
// in the world — on forge commit statuses of pull requests still in flight, in
// delivered mail, in posted pull-request comments — and a reader that only
// knows the current spelling would fail to recognise them. Nothing should emit
// it.
//
// Deliberately UNESCAPED, unlike Run. This reconstructs a string another build
// already wrote, and that build concatenated the id raw (`base + "/runs/" +
// runID`). Percent-escaping here would produce a URL that never existed, so a
// run whose id needs escaping would not recognise its own status — and a run
// id is caller-chosen (`run_id` on the launch API).
//
// Reachable, though narrower than "anything goes": store.SanitizePathComponent
// rejects `..`, a path separator and control characters, so a space or a
// non-ASCII letter reaches this function and a slash does not. Faithfulness to
// what was written beats consistency with Run.
func LegacyRun(base, runID string) string {
	return strings.TrimRight(base, "/") + "/runs/" + runID
}
