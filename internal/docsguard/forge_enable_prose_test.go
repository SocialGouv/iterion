package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The guards in this file hold the pages that describe the forge-enable
// surface — docs/cloud-overview.md, docs/forge-integrations.md,
// docs/merge-gate.md, docs/webhooks.md, docs/cloud-admin-guide.md — against
// the studio, engine and server facts each sentence rests on: a studio
// destination, a derivation, a refusal message, a render condition.
//
// Each one reads the real files, and each one fails in BOTH directions: a page
// that promises a gesture the studio does not serve, and a code fact that moves
// out from under a page nobody edited. The fact readers are pure functions of
// file content so TestTheProseGuardFactReadersRefuseSourceThatLostTheFact can
// see them refuse source that no longer carries what the pages promise.

// forgeProseSource reads a file of this repository by its path components from
// the root. An absent subject is a failure, never a skip: a page or a source
// file a guard cannot find leaves its sentences unchecked, which is the state
// these guards exist to replace.
func forgeProseSource(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v — a guard's subject moved; move the guard with it rather than dropping the claim", path, err)
	}
	return string(raw)
}

// forgeProsePackageSource concatenates the non-test sources of a package, so
// a fact is read from the package that owns it rather than from one file name:
// the refusal types and the undo they drive are split across siblings of
// pkg/forge, and which sibling holds which is not what these pages describe.
func forgeProsePackageSource(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{"..", ".."}, parts...)...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v — a guard's subject moved; move the guard with it rather than dropping the claim", dir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b.WriteString(forgeProseSource(t, slices.Concat(parts, []string{name})...))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		t.Fatalf("%s: no package source read — the facts the pages below are checked against would all go unchecked", dir)
	}
	return b.String()
}

// forgeProseFlat collapses a wrapped page onto one line, so a needle that
// straddles a wrap is still found.
func forgeProseFlat(s string) string { return strings.Join(strings.Fields(s), " ") }

// forgeProseBlock returns the span of src between the first occurrence of
// `from` and the first `until` after it — a function body, a const table, a
// documentation section. Empty when `from` is absent.
func forgeProseBlock(src, from, until string) string {
	at := strings.Index(src, from)
	if at < 0 {
		return ""
	}
	rest := src[at+len(from):]
	if end := strings.Index(rest, until); end >= 0 {
		return rest[:end]
	}
	return rest
}

// forgeProseStripComments drops the comment lines of a source file: a fact is
// read from what the code does, and a comment naming a pair the code no longer
// wires proves nothing.
func forgeProseStripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

var forgeProseHeadingRe = regexp.MustCompile(`^#{1,6} `)

// forgeProseAnchoredSection returns the documentation section a `<a name=…>`
// anchor opens, up to the next heading of any level. The anchor is the address
// a server refusal sends a reader to; the heading level that carries it is a
// layout choice, and a `#` inside a fenced block is shell, not a heading.
func forgeProseAnchoredSection(page, anchor string) string {
	at := strings.Index(page, anchor)
	if at < 0 {
		return ""
	}
	var b strings.Builder
	fenced := false
	for _, line := range strings.Split(page[at+len(anchor):], "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			fenced = !fenced
		case !fenced && forgeProseHeadingRe.MatchString(line):
			return b.String()
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// forgeProseQuote is the window a failure prints around the offending needle:
// the claim alone reads as a guess about which paragraph is meant.
func forgeProseQuote(prose string, at, before, after int) string {
	return prose[max(0, at-before):min(len(prose), at+after)]
}

var (
	forgeProseTabLabelRe  = regexp.MustCompile(`label:\s*"([^"]+)"`)
	forgeProseForgesTabRe = regexp.MustCompile(`FORGES_LABEL\s*=\s*"([^"]+)"`)
	forgeProseForbiddenRe = regexp.MustCompile(`httpError\(w, http\.StatusForbidden, "([^"]+)"`)
)

// forgeProseTabLabels reads a page's own tab table. The destination of a
// documented gesture is a fact of the studio, not of the page describing it.
func forgeProseTabLabels(src string) []string {
	var labels []string
	for _, m := range forgeProseTabLabelRe.FindAllStringSubmatch(forgeProseBlock(src, "const TABS", "];"), -1) {
		labels = append(labels, m[1])
	}
	return labels
}

// forgeProseForgesTab reads the name the studio gives the tab that hosts the
// forge connections — the second half of the "Integrations → …" gesture every
// one of these pages spells out.
func forgeProseForgesTab(src string) string {
	m := forgeProseForgesTabRe.FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[1]
}

// forgeProseIntegrationsDestinations returns the names the studio serves under
// `/integrations`: the labels of its own tab table, the forge tab whose label
// is a constant, and the page's name — the one the route and the header carry.
// A page that hangs any of them off the team page names a destination nobody
// can open.
func forgeProseIntegrationsDestinations(src string) []string {
	names := forgeProseTabLabels(src)
	if tab := forgeProseForgesTab(src); tab != "" && !slices.Contains(names, tab) {
		names = append(names, tab)
	}
	if len(names) == 0 {
		return nil
	}
	return append(names, "Integrations")
}

// forgeProseHookPinGesture returns the gesture the server's 422 names — the
// one sentence an operator reads at the moment the forge refuses the webhook
// address, which has to send them to the tab the studio serves.
func forgeProseHookPinGesture(src string) string {
	return forgeProseBlock(src, "func hookPinGesture(", "\n}")
}

// forgeProseSyncFollowsTheGate reports whether the provisioner derives
// `review_on_sync` from the gate pin — the fact behind merge-gate.md's
// statement that the posture select moves per-push re-review, and not only the
// commit status. Read from Provision's own body with its comments dropped, so
// splitting the derivation over two statements still counts as deriving it.
func forgeProseSyncFollowsTheGate(src string) bool {
	body := forgeProseBlock(forgeProseStripComments(src), "func (o *Orchestrator) Provision(", "\nfunc ")
	return strings.Contains(body, "reviewOnSync") && strings.Contains(body, "operatorGateDisabled(")
}

// forgeProseEnableRefusal reads the message the enable route answers a caller
// who cannot manage the team with — out of the grant branch itself, so an
// unrelated earlier 403 is not mistaken for it. Empty when the route no longer
// refuses on the grant at all.
func forgeProseEnableRefusal(src string) string {
	body := forgeProseBlock(src, "func (s *Server) handleEnableForgeRepoBots(", "\nfunc ")
	grant := forgeProseBlock(body, "canManageTeam", "\n\t}")
	if grant == "" {
		return ""
	}
	m := forgeProseForbiddenRe.FindStringSubmatch(grant)
	if m == nil {
		return ""
	}
	return m[1]
}

// forgeProseOffersPosture returns the Enable panel's own condition for
// offering the merge-gate posture, which forge-integrations.md paraphrases.
func forgeProseOffersPosture(src string) string {
	return strings.TrimSpace(forgeProseBlock(src, "const offersPosture", ";"))
}

// forgeProseExcludesIntegratedRepo reports whether that condition still
// excludes a repo already integrated on the connection, in either of the two
// ways the panel's siblings write the same exclusion.
func forgeProseExcludesIntegratedRepo(offers string) bool {
	return strings.Contains(offers, "!existing") || strings.Contains(offers, "existing === null")
}

const (
	forgeProseTeamPage         = "studio/src/views/teams/TeamPage.tsx"
	forgeProseIntegrationsPage = "studio/src/views/integrations/IntegrationsPage.tsx"
	forgeProseEnablePanel      = "studio/src/views/teams/tabs/integrations/EnableRepoPanel.tsx"
	forgeProseOrchestrator     = "pkg/forge/orchestrator.go"
	forgeProseProvisionRoutes  = "pkg/server/forge_provisioning_routes.go"
)

// forgeProseGesturePages walk an operator to the forge connections, so each
// one names that destination and the route to type.
var forgeProseGesturePages = []string{"cloud-overview.md", "forge-integrations.md", "webhooks.md"}

// forgeProseStudioNavPages name a studio destination for an integration
// surface — the forge connections, the webhooks they fire, the secrets bots
// consume — and none of those lives on the team page.
var forgeProseStudioNavPages = slices.Concat(forgeProseGesturePages, []string{"merge-gate.md", "cloud-admin-guide.md"})

// forgeProseTeamPageNavRe matches a navigation chain that attaches one of the
// studio's integration destinations to the team page, written either way
// round: the route, an arrow and the name ("`/teams/<id>` → Webhooks"), or the
// name, its tab and the route ("`Webhooks` tab on `/teams/<id>`"). Markdown
// emphasis and code quoting are allowed between the parts, and a period closes
// the window. It runs on the flattened page (forgeProseTeamPageNavHit), so a
// chain the page wraps across lines is still one.
func forgeProseTeamPageNavRe(names []string) *regexp.Regexp {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, regexp.QuoteMeta(n))
	}
	alt := "\\b(?:" + strings.Join(quoted, "|") + ")\\b"
	mark := "[`*_]{0,3}"
	return regexp.MustCompile(
		"(?:teams/<id>`?[^.\\n]{0,40}(?:→|->)\\s*" + mark + alt +
			"|" + alt + mark + "[^.\\n]{0,40}\\b(?:tabs?|page)\\b[^.\\n]{0,20}\\b(?:on|of|at|under)\\b\\s*[`*_/]{0,4}teams/<id>)")
}

// forgeProseNegationRe marks a window that says where a surface is NOT —
// "Webhooks are not a tab of `/teams/<id>`" — which is the correction, not a
// chain.
var forgeProseNegationRe = regexp.MustCompile(`\b(?:not|no longer|never)\b`)

// forgeProseTeamPageNavHit returns the first navigation chain of a page that
// attaches an integration destination to the team page, read on the flattened
// page and skipping a window that carries a negation; "" when there is none.
func forgeProseTeamPageNavHit(re *regexp.Regexp, page string) string {
	for _, m := range re.FindAllString(forgeProseFlat(page), -1) {
		if !forgeProseNegationRe.MatchString(m) {
			return m
		}
	}
	return ""
}

// TestTheForgeGesturePagesNameTheDestinationTheStudioServes.
//
// The forge surfaces live at `/integrations`, whose forge tab the studio names
// itself; the team page serves no Integrations tab. These pages name that
// destination and its route, so the one gesture the allowlist-refused walk
// cannot skip — pinning a webhook base on the connection — has a home a reader
// can reach. The server's own refusal names it too: its 422 is where an
// operator meets the gesture first.
func TestTheForgeGesturePagesNameTheDestinationTheStudioServes(t *testing.T) {
	teamTabs := forgeProseTabLabels(forgeProseSource(t, filepath.FromSlash(forgeProseTeamPage)))
	if len(teamTabs) == 0 {
		t.Fatalf("%s: no tab label parsed — the table these pages are checked against moved, and the claim about where a forge gesture lives is unchecked", forgeProseTeamPage)
	}
	forgesTab := forgeProseForgesTab(forgeProseSource(t, filepath.FromSlash(forgeProseIntegrationsPage)))
	if forgesTab == "" {
		t.Fatalf("%s: FORGES_LABEL is gone — the name of the tab the documented gesture ends on has to come from the studio, not from memory", forgeProseIntegrationsPage)
	}
	if slices.Contains(teamTabs, "Integrations") {
		t.Fatalf("the team page serves an Integrations tab again (%v): these pages name /integrations because it does not, so the correction has to be re-decided rather than kept green", teamTabs)
	}

	destination := "Integrations → " + forgesTab
	for _, page := range forgeProseGesturePages {
		prose := forgeProseFlat(forgeProseSource(t, "docs", page))
		if !strings.Contains(prose, destination) {
			t.Errorf("docs/%s does not name %q — the studio serves the forge connections there (team page tabs: %v)", page, destination, teamTabs)
		}
		if !strings.Contains(prose, "`/integrations`") {
			t.Errorf("docs/%s names no route for the gesture: a reader who cannot find the tab by its name has nothing to type", page)
		}
	}

	gesture := forgeProseHookPinGesture(forgeProseSource(t, filepath.FromSlash(forgeProseProvisionRoutes)))
	if gesture == "" {
		t.Fatalf("%s: hookPinGesture is gone — the refusal's own pin gesture is the sentence an operator reads when the forge refuses the address, and it names the same destination as these pages", forgeProseProvisionRoutes)
	}
	if !strings.Contains(gesture, destination+" →") {
		t.Errorf("%s: the 422's pin gesture does not walk through %q (%q) — a rename that moves the pages has to move the sentence the refused operator reads, or they are sent to a tab that does not exist", forgeProseProvisionRoutes, destination, gesture)
	}
}

// TestNoPageRoutesAnIntegrationSurfaceThroughTheTeamPage.
//
// `/integrations` serves the forge connections, the webhooks, the secrets and
// the bot bindings; `/teams/<id>` serves the team's own settings. A page that
// hangs one of the first list off the team page sends an operator to a tab
// that is not there, and leaves them nothing to type — which is how the manual
// webhook fallback of a refused hook URL becomes unreachable.
func TestNoPageRoutesAnIntegrationSurfaceThroughTheTeamPage(t *testing.T) {
	integrationsPage := forgeProseSource(t, filepath.FromSlash(forgeProseIntegrationsPage))
	destinations := forgeProseIntegrationsDestinations(integrationsPage)
	if len(destinations) == 0 {
		t.Fatalf("%s: no tab name parsed — the names these pages are checked against come from the studio's own table, and an empty one leaves every navigation unchecked", forgeProseIntegrationsPage)
	}
	teamTabs := forgeProseTabLabels(forgeProseSource(t, filepath.FromSlash(forgeProseTeamPage)))
	if len(teamTabs) == 0 {
		t.Fatalf("%s: no tab label parsed — what the team page does serve is half of this verdict", forgeProseTeamPage)
	}
	re := forgeProseTeamPageNavRe(destinations)
	for _, page := range forgeProseStudioNavPages {
		if m := forgeProseTeamPageNavHit(re, forgeProseSource(t, "docs", page)); m != "" {
			t.Errorf("docs/%s routes an integration surface through the team page — %q — while the studio serves %v under /integrations and %v on /teams/<id>",
				page, m, destinations, teamTabs)
		}
	}
}

// TestTheMergeGatePostureSentenceSaysTheGateMovesPerPushReview.
//
// The repo page's select writes `gate_enabled` through a re-provision, and the
// provisioner re-derives `review_on_sync` from that pin: turning the gate off
// also stops per-push re-review, for every bot co-enabled on the repo. The
// page says so, because an admin dropping a commit status is otherwise
// dropping every bot's re-review with it.
func TestTheMergeGatePostureSentenceSaysTheGateMovesPerPushReview(t *testing.T) {
	if !forgeProseSyncFollowsTheGate(forgeProseSource(t, filepath.FromSlash(forgeProseOrchestrator))) {
		t.Fatalf("%s: Provision no longer derives review_on_sync from operatorGateDisabled: docs/merge-gate.md states that the posture select moves per-push re-review for every co-enabled bot, and that sentence has to move with the derivation", forgeProseOrchestrator)
	}
	prose := forgeProseFlat(forgeProseSource(t, "docs", "merge-gate.md"))
	for _, required := range []string{
		"the pin is the whole posture",
		"stops forcing `review_on_sync`, so pushes are no longer re-reviewed",
		"for every bot co-enabled on the repo",
		"an operator pinned through the webhook API",
	} {
		if !strings.Contains(prose, required) {
			t.Errorf("docs/merge-gate.md no longer states %q: the select flips review_on_sync, and a page that does not say so promises a gesture narrower than the one it describes", required)
		}
	}
	for _, refuted := range []string{
		"The options name only the status",
		"which the select does not change",
	} {
		if strings.Contains(prose, refuted) {
			at := strings.Index(prose, refuted)
			t.Errorf("docs/merge-gate.md states %q again — %q — while the provisioner derives review_on_sync from the gate pin", refuted, forgeProseQuote(prose, at, 40, 220))
		}
	}
}

// TestTheAdminOnlyClaimNamesTheControlItHoldsFor.
//
// "Admin-only" holds for the repo page's select, which is disabled without the
// grant, and not for the Enable panel: the connect wizard renders it to a
// member, who reads the role only off the server's refusal. The page names each
// surface, and quotes that refusal.
func TestTheAdminOnlyClaimNamesTheControlItHoldsFor(t *testing.T) {
	refusal := forgeProseEnableRefusal(forgeProseSource(t, filepath.FromSlash(forgeProseProvisionRoutes)))
	if refusal == "" {
		t.Fatalf("%s: the enable route no longer refuses a caller without the team-management grant — docs/merge-gate.md rests the Enable panel's admin posture on that refusal, so the page has to move with it", forgeProseProvisionRoutes)
	}
	prose := forgeProseFlat(forgeProseSource(t, "docs", "merge-gate.md"))
	if strings.Contains(prose, "Both controls are admin-only") {
		t.Errorf("docs/merge-gate.md claims both merge-gate controls are admin-only; the Enable panel carries no role term — a member gets the fieldset and the Enable button, and learns the role from the server's %q", refusal)
	}
	if !strings.Contains(prose, "The repo page's select is admin-only") {
		t.Errorf("docs/merge-gate.md no longer says which control is admin-only: the select is disabled without the grant, and that is the only one of the two that is")
	}
	if !strings.Contains(prose, refusal) {
		t.Errorf("docs/merge-gate.md does not quote the enable route's own refusal (%q): the Enable panel is an admin surface because the server says so, and a reader who meets a 403 should find the same words here", refusal)
	}
}

// TestTheEnablePagePromisesThePostureOnlyWhereThePanelOffersIt.
//
// forge-integrations.md lists the posture among the gestures of every enable.
// The panel offers it only for a repo with no integration on this connection;
// an enabled repo gets a read-only line pointing at the repo page.
func TestTheEnablePagePromisesThePostureOnlyWhereThePanelOffersIt(t *testing.T) {
	offers := forgeProseOffersPosture(forgeProseSource(t, filepath.FromSlash(forgeProseEnablePanel)))
	if offers == "" {
		t.Fatalf("%s: offersPosture is gone — docs/forge-integrations.md describes when the Enable panel offers the merge-gate posture, and the condition it paraphrases has to be read from the panel", forgeProseEnablePanel)
	}
	if !forgeProseExcludesIntegratedRepo(offers) {
		t.Fatalf("%s: offersPosture no longer excludes a repo that already has an integration (%q) — docs/forge-integrations.md promises the posture only for a repo not yet enabled on this connection", forgeProseEnablePanel, offers)
	}
	prose := forgeProseFlat(forgeProseSource(t, "docs", "forge-integrations.md"))
	for _, required := range []string{
		"offered only for a repo not yet enabled on this connection",
		"an enabled repo changes it on the repo page",
	} {
		if !strings.Contains(prose, required) {
			t.Errorf("docs/forge-integrations.md does not state %q: an operator co-enabling a bot on a connected repo looks for a control the panel replaces with a read-only line (offersPosture: %q)", required, offers)
		}
	}
}

// forgeProseHookRefusedDetail returns the body of the server's refusal
// detail — the four gestures docs/webhooks.md § hook_url_refused describes.
func forgeProseHookRefusedDetail(src string) string {
	return forgeProseBlock(src, "func hookRefusedDetail(", "\n}")
}

// forgeProseUndoConfigWrite returns the body of the one place a provision
// unwinds its config write from, and forgeProseRestoreConfig the restore it
// calls: webhooks.md states the rule both implement.
func forgeProseUndoConfigWrite(src string) string {
	return forgeProseBlock(src, ") undoConfigWrite(", "\n}")
}

func forgeProseRestoreConfig(src string) string {
	return forgeProseBlock(src, ") restoreConfig(", "\n}")
}

func forgeProseRejectionTest(src string) string {
	return forgeProseBlock(src, "func forgeRejectedTheWrite(", "\n}")
}

// TestTheRefusedHookSectionDescribesTheRefusalAndTheRestoreThatShipped.
//
// The section is the operator's page for a refused hook, and four of its
// claims are claims about code: the detail quotes the forge's own message and
// names one remedy per case (a pinned base the forge refuses, no public URL, a
// public URL that is not an absolute http(s) URL, a base to pin), a
// re-provision puts its previous webhook config back only when the forge
// provably did not apply the write — a 4xx other than 408 and 499, a request
// that never left, a step that ended on a read, a hook request that was never
// sent, or no forge call — under a
// compare-and-set, so a concurrent provision that won is not clobbered, and
// nothing serialises two provisions of the same repo, which the section owes
// the operator as a known limit.
func TestTheRefusedHookSectionDescribesTheRefusalAndTheRestoreThatShipped(t *testing.T) {
	routes := forgeProseSource(t, filepath.FromSlash(forgeProseProvisionRoutes))
	detail := forgeProseHookRefusedDetail(routes)
	if detail == "" {
		t.Fatalf("%s: hookRefusedDetail is gone — the section documents what it says, so the guard has to follow the rename", forgeProseProvisionRoutes)
	}
	if !strings.Contains(detail, "e.Detail") {
		t.Errorf("%s: the refusal detail no longer carries the forge's own message (%q): the section promises it quoted, which is what tells a refused ADDRESS from a 422 about something else", forgeProseProvisionRoutes, detail)
	}
	if !strings.Contains(detail, "no public URL") || !strings.Contains(detail, "ITERION_PUBLIC_URL") {
		t.Errorf("%s: the refusal detail no longer leads with the deployment's missing public URL (%q): the section sends the reader of a relative address to ITERION_PUBLIC_URL rather than to a per-connection pin", forgeProseProvisionRoutes, detail)
	}
	// Each other branch the page gives a bullet: a pinned base the forge
	// refuses, a public URL that is not an absolute http(s) URL, and the
	// unpinned case — the PIC's own.
	if !strings.Contains(detail, "The usual cause is a forge that only delivers to allowlisted hosts") {
		t.Errorf("%s: the refusal detail no longer names the allowlist as the usual cause (%q): the section's \"Unpinned\" bullet sends the reader to pin a base the forge accepts", forgeProseProvisionRoutes, detail)
	}
	if !strings.Contains(detail, "This connection pins") {
		t.Errorf("%s: the refusal detail no longer names the connection's pin (%q): the section's \"Pinned\" bullet sends the reader to the forge's allowlist or to changing the pin, not to pinning one", forgeProseProvisionRoutes, detail)
	}
	if !strings.Contains(detail, "absoluteHTTPURL(") || !strings.Contains(detail, "not an absolute http(s) URL") {
		t.Errorf("%s: the refusal detail no longer tells a public URL without its scheme from a missing one (%q): the section's \"Not an absolute http(s) URL\" bullet asks for the scheme", forgeProseProvisionRoutes, detail)
	}

	forgePkg := forgeProsePackageSource(t, "pkg", "forge")
	undo := forgeProseUndoConfigWrite(forgePkg)
	if undo == "" {
		t.Fatalf("pkg/forge: undoConfigWrite is gone — the section describes the one place a provision unwinds its config write from")
	}
	if !strings.Contains(undo, "forgeRejectedTheWrite") {
		t.Errorf("pkg/forge: the undo no longer splits a forge REFUSAL from a failure that proves nothing (%q): the section says an indeterminate failure keeps the new config and asks for a re-run", undo)
	}
	rejection := forgeProseRejectionTest(forgePkg)
	for _, fact := range []struct{ needle, claim string }{
		{"se.Code >= 400 && se.Code < 500", "a forge 4xx is a refusal the forge could not have applied"},
		{"se.Code != statusRequestTimeout && se.Code != statusClientClosedRequest", "a 408 or a 499 proves nothing, a gateway may have forwarded the write"},
	} {
		if !strings.Contains(rejection, fact.needle) {
			t.Errorf("pkg/forge: forgeRejectedTheWrite no longer carries %q: the section states that %s", fact.needle, fact.claim)
		}
	}
	// The classes whose verdict is a branch's return: read with the return,
	// whitespace-tolerant, so an inverted verdict is no longer the fact.
	for _, fact := range []struct {
		re    *regexp.Regexp
		claim string
	}{
		{regexp.MustCompile(`requestNeverLeft\(err\)\s*\{\s*return true`), "a request that never left (DNS, dial, TLS certificate) restores"},
		{regexp.MustCompile(`errors\.As\(err, &read\)\s*\{\s*return read\.priorWrite == nil`), "a step that ended on a read applied nothing"},
		{regexp.MustCompile(`errors\.As\(err, &notSent\)\s*\{\s*return true`), "a hook request that was never sent restores"},
	} {
		if !fact.re.MatchString(rejection) {
			t.Errorf("pkg/forge: forgeRejectedTheWrite no longer reads %q as a refusal the forge could not have applied: the section states that %s", fact.re.String(), fact.claim)
		}
	}
	restore := forgeProseRestoreConfig(forgePkg)
	if restore == "" {
		t.Fatalf("pkg/forge: restoreConfig is gone — the section describes its compare-and-set")
	}
	if !strings.Contains(restore, "cur.TokenHash != w.wroteHash") {
		t.Errorf("pkg/forge: the restore is no longer a compare-and-set on the hash this provision wrote (%q): the section promises a concurrent winner is left alone", restore)
	}

	page := forgeProseSource(t, "docs", "webhooks.md")
	section := forgeProseAnchoredSection(page, `<a name="hook-url-refused">`)
	if section == "" {
		t.Fatalf("docs/webhooks.md: the `hook_url_refused` section is gone, and the 422 the server answers names it as the page to read")
	}
	prose := forgeProseFlat(section)
	for _, required := range []string{
		`(HTTP 422: "Invalid url given")`,
		"`ITERION_PUBLIC_URL`",
		"**Pinned.**",
		"**Unpinned.**",
		"**Not an absolute http(s) URL.**",
		"compare-and-set",
		"the token hash",
		"A concurrent provision that won the race is left alone",
		"re-run the same request",
		"concurrent re-provisions of the same repo are not serialised",
		"Re-run the request if two raced",
	} {
		if !strings.Contains(prose, required) {
			t.Errorf("docs/webhooks.md § hook_url_refused does not state %q: the server quotes the forge, names the missing public URL, restores under a compare-and-set and orders nothing between two provisions of one repo — and a reader of this section acts on what it says", required)
		}
	}
	// The classification lives in two bullets, and each class belongs to ONE of
	// them: a page that swaps a class between the bullets states the restore
	// rule backwards while every phrase still appears in the section.
	applied := forgeProseBlock(prose, "**A failure the forge could not have applied**", "**A failure that proves nothing**")
	unknown := forgeProseBlock(prose, "**A failure that proves nothing**", "The restore is a compare-and-set")
	if applied == "" || unknown == "" {
		t.Fatalf("docs/webhooks.md § hook_url_refused: the two restore bullets are gone, and the classification they state is the operator's whole answer to a failed re-provision")
	}
	for _, class := range []string{
		"a forge 4xx other than 408 and 499",
		"a request that never left",
		"a step that ended on a read",
		"a hook request that was never sent",
		"no forge call at all",
	} {
		if !strings.Contains(applied, class) {
			t.Errorf("docs/webhooks.md: %q is not in the bullet of failures the forge could not have applied — the restore runs for it", class)
		}
		if strings.Contains(unknown, class) {
			t.Errorf("docs/webhooks.md: %q is stated among the failures that prove nothing — the restore runs for it", class)
		}
	}
	for _, class := range []string{"That is a 408 or a 499", "a 5xx", "a timeout or a reset mid-flight", "a cancelled request"} {
		if !strings.Contains(unknown, class) {
			t.Errorf("docs/webhooks.md: %q is not in the bullet of failures that prove nothing — the fresh config stays for it", class)
		}
		if strings.Contains(applied, class) {
			t.Errorf("docs/webhooks.md: %q is stated among the failures the forge could not have applied — the fresh config stays for it", class)
		}
	}
	for _, refuted := range []string{
		"the forge then refuses, iterion restores",
	} {
		if strings.Contains(prose, refuted) {
			at := strings.Index(prose, refuted)
			t.Errorf("docs/webhooks.md § hook_url_refused promises an unconditional restore again — %q — while a failure the forge never answered keeps the new config (undoConfigWrite)", forgeProseQuote(prose, at, 40, 260))
		}
	}
}

// TestTheProseGuardFactReadersRefuseSourceThatLostTheFact.
//
// Every guard above rests half of its verdict on a fact read out of real
// source. A reader that answered the same whatever it was handed would make
// those halves decorative — and the pages would keep their sentences the day
// the studio renames a tab or the provisioner stops deriving the sync.
func TestTheProseGuardFactReadersRefuseSourceThatLostTheFact(t *testing.T) {
	if labels := forgeProseTabLabels(`const OTHER = [{ id: "members", label: "Team access" }];`); len(labels) != 0 {
		t.Errorf("tab labels read from a file with no TABS table: %v", labels)
	}
	if labels := forgeProseTabLabels(`const TABS = [{ id: "members", label: "Team access" }, { id: "audit", label: "Audit log" }];`); !slices.Equal(labels, []string{"Team access", "Audit log"}) {
		t.Errorf("tab labels misread: %v", labels)
	}
	if tab := forgeProseForgesTab(`const OTHER_LABEL = "Repositories";`); tab != "" {
		t.Errorf("the forge tab name was read from a file that does not declare it: %q", tab)
	}
	if tab := forgeProseForgesTab(`const FORGES_LABEL = "Projects";`); tab != "Projects" {
		t.Errorf("the forge tab name misread: %q", tab)
	}
	if names := forgeProseIntegrationsDestinations(`const OTHER = [{ id: "webhooks", label: "Webhooks" }];`); names != nil {
		t.Errorf("integration destinations read from a file with no TABS table: %v", names)
	}
	if names := forgeProseIntegrationsDestinations(
		"const FORGES_LABEL = \"Projects\";\nconst TABS = [{ id: \"forges\", label: FORGES_LABEL }, { id: \"webhooks\", label: \"Webhooks\" }];",
	); !slices.Equal(names, []string{"Webhooks", "Projects", "Integrations"}) {
		t.Errorf("integration destinations misread: %v", names)
	}
	navRe := forgeProseTeamPageNavRe([]string{"Repositories", "Webhooks", "Bot bindings", "Integrations"})
	for _, chain := range []string{
		"a webhook through the studio (`Webhooks` tab on `/teams/<id>`) or the API",
		"UI path: `/teams/<id>` → Webhooks → \"Create webhook\"",
		"open `/teams/<id>` → **Integrations** → the connection",
		"the Integrations tab of `/teams/<id>`",
		"the **Bot bindings** page under /teams/<id>",
		"a webhook through the studio (the `Webhooks`\ntab on `/teams/<id>`) or the API",
	} {
		if forgeProseTeamPageNavHit(navRe, chain) == "" {
			t.Errorf("a navigation that sends an operator to a team-page integration surface was not matched: %q", chain)
		}
	}
	for _, fine := range []string{
		"In the studio, **Integrations → Repositories** (`/integrations`). The team page (`/teams/<id>`) carries no Integrations tab.",
		"a webhook through the studio (the `Webhooks` tab of **Integrations**, `/integrations`) or the API",
		"UI path: `/teams/<id>` → Team access → \"Invite a member\"",
		"UI path: `/teams/<id>` → Audit log.",
		"All under `/api/teams/{id}/forge/` — Repositories are listed there",
		"Webhooks are not a tab of `/teams/<id>`: the studio serves them under **Integrations**.",
	} {
		if m := forgeProseTeamPageNavHit(navRe, fine); m != "" {
			t.Errorf("a sentence that routes nothing through the team page was reported as a chain: %q matched %q", fine, m)
		}
	}
	if gesture := forgeProseHookPinGesture("func otherGesture(connectionID string) string {\n\treturn \"\"\n}\n"); gesture != "" {
		t.Errorf("the pin gesture was read from a file that does not declare it: %q", gesture)
	}
	if gesture := forgeProseHookPinGesture("func hookPinGesture(connectionID string) string {\n\treturn \"(Integrations → Projects → the row)\"\n}\n"); !strings.Contains(gesture, "Integrations → Projects →") {
		t.Errorf("the pin gesture misread: %q", gesture)
	}
	if forgeProseSyncFollowsTheGate("func (o *Orchestrator) Provision(ctx context.Context) error {\n\treviewOnSync := anyBotGatesMerges(desiredBots, frByBot)\n}\n") {
		t.Error("the sync derivation was reported as following the gate pin on code that ignores it")
	}
	if forgeProseSyncFollowsTheGate("func (o *Orchestrator) Provision(ctx context.Context) error {\n\t// reviewOnSync := x && !operatorGateDisabled(operatorVars)\n\treviewOnSync := x\n}\n") {
		t.Error("a commented-out derivation counted as the code deriving the sync from the gate pin")
	}
	if forgeProseSyncFollowsTheGate("func (o *Orchestrator) backfillBotRules(ctx context.Context) error {\n\tgateOff := operatorGateDisabled(cfg.OperatorLaunchVars)\n}\n\nfunc (o *Orchestrator) Provision(ctx context.Context) error {\n\treviewOnSync := anyBotGatesMerges(desiredBots, frByBot)\n}\n") {
		t.Error("a gate read by another method of the orchestrator counted as Provision deriving the sync from it")
	}
	if !forgeProseSyncFollowsTheGate("func (o *Orchestrator) Provision(ctx context.Context) error {\n\treviewOnSync := anyBotGatesMerges(desiredBots, frByBot) && !operatorGateDisabled(operatorVars)\n}\n") {
		t.Error("the single-expression derivation was not recognised")
	}
	if !forgeProseSyncFollowsTheGate("func (o *Orchestrator) Provision(ctx context.Context) error {\n\tgateOff := operatorGateDisabled(operatorVars)\n\treviewOnSync := anyBotGatesMerges(desiredBots, frByBot)\n\tif gateOff {\n\t\treviewOnSync = false\n\t}\n}\n") {
		t.Error("a derivation split over two statements was read as ignoring the gate pin")
	}
	if msg := forgeProseEnableRefusal("func (s *Server) handleEnableForgeRepoBots(w http.ResponseWriter, r *http.Request) {\n\t_ = r\n}\n"); msg != "" {
		t.Errorf("an enable handler with no grant check reported a refusal: %q", msg)
	}
	if msg := forgeProseEnableRefusal("func (s *Server) handleEnableForgeRepoBots(w http.ResponseWriter, r *http.Request) {\n\tif !s.canManageTeam(ctx, id, teamID) {\n\t\thttpError(w, http.StatusForbidden, \"admin or owner required\")\n\t\treturn\n\t}\n}\n"); msg != "admin or owner required" {
		t.Errorf("the enable refusal misread: %q", msg)
	}
	if msg := forgeProseEnableRefusal("func (s *Server) handleEnableForgeRepoBots(w http.ResponseWriter, r *http.Request) {\n\tif !s.isTeamMember(ctx, id, teamID) {\n\t\thttpError(w, http.StatusForbidden, \"not a member\")\n\t\treturn\n\t}\n\tif !s.canManageTeam(ctx, id, teamID) {\n\t\thttpError(w, http.StatusForbidden, \"admin or owner required\")\n\t\treturn\n\t}\n}\n"); msg != "admin or owner required" {
		t.Errorf("a 403 ahead of the grant check was read as the grant's own refusal: %q", msg)
	}
	if offers := forgeProseOffersPosture(`const showsPosture = repo !== "" && !existing;`); offers != "" {
		t.Errorf("offersPosture read from a panel that no longer declares it: %q", offers)
	}
	if offers := forgeProseOffersPosture("  const offersPosture =\n    repo !== \"\" && integrationsReady && !existing;\n"); !forgeProseExcludesIntegratedRepo(offers) {
		t.Errorf("offersPosture misread: %q", offers)
	}
	if !forgeProseExcludesIntegratedRepo(`repo !== "" && existing === null`) {
		t.Error("the exclusion written as an explicit null comparison was read as missing")
	}
	if forgeProseExcludesIntegratedRepo(`repo !== "" && integrationsReady`) {
		t.Error("a condition that does not exclude an integrated repo was read as excluding one")
	}
	if detail := forgeProseHookRefusedDetail("func otherDetail(e *forge.HookRefusedError) string {\n\treturn \"\"\n}\n"); detail != "" {
		t.Errorf("the refusal detail was read from a file that does not declare it: %q", detail)
	}
	if detail := forgeProseHookRefusedDetail("func hookRefusedDetail(e *forge.HookRefusedError) string {\n\treturn head(e.Detail)\n}\n"); !strings.Contains(detail, "e.Detail") {
		t.Errorf("the refusal detail misread: %q", detail)
	}
	if undo := forgeProseUndoConfigWrite("func (o *Orchestrator) rollbackConfig(ctx context.Context) error {\n\treturn nil\n}\n"); undo != "" {
		t.Errorf("the undo body was read from a file with no undoConfigWrite: %q", undo)
	}
	if undo := forgeProseUndoConfigWrite("func (o *Orchestrator) undoConfigWrite(ctx context.Context, w configWrite) error {\n\treturn o.restoreConfig(ctx, w)\n}\n"); strings.Contains(undo, "forgeRejectedTheWrite") {
		t.Errorf("an unconditional undo was read as splitting a refusal from an indeterminate failure: %q", undo)
	}
	if rejection := forgeProseRejectionTest("func otherTest(err error) bool {\n\treturn se.Code >= 400 && se.Code < 500\n}\n"); rejection != "" {
		t.Errorf("the rejection test was read from a file that does not declare it: %q", rejection)
	}
	if restore := forgeProseRestoreConfig("func (o *Orchestrator) restoreConfig(ctx context.Context, w configWrite) error {\n\treturn o.Webhooks.Update(ctx, w.prev)\n}\n"); strings.Contains(restore, "cur.TokenHash") {
		t.Errorf("a blind restore was read as a compare-and-set: %q", restore)
	}
	if section := forgeProseAnchoredSection("## Elsewhere\n\nno anchor here\n", `<a name="hook-url-refused">`); section != "" {
		t.Errorf("a section was read from a page that carries no such anchor: %q", section)
	}
	h3 := "# Page\n\n### <a name=\"hook-url-refused\"></a>`hook_url_refused`\n\nthe body\n\n```sh\n# a shell comment, not a heading\n```\n\nstill the body\n\n## Next\n\nelsewhere\n"
	h2 := strings.Replace(h3, "### <a name=", "## <a name=", 1)
	wantBody := []string{"the body", "a shell comment, not a heading", "still the body"}
	for _, page := range []string{h3, h2} {
		section := forgeProseAnchoredSection(page, `<a name="hook-url-refused">`)
		for _, want := range wantBody {
			if !strings.Contains(section, want) {
				t.Errorf("the anchored section dropped %q — the heading level above the anchor is not what bounds it: %q", want, section)
			}
		}
		if strings.Contains(section, "elsewhere") {
			t.Errorf("the anchored section ran past the next heading: %q", section)
		}
	}
}
