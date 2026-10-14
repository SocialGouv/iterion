package bots

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Vetty pushes its alignment with an installation token, and GitHub REFUSES
// outright an App push touching .github/workflows/** when that token lacks
// `workflows` (pkg/forge/github/app_client.go). The refusal arrives as a
// failed push, which Vetty read as "the alignment vanished"
// (`hold_lost_alignment`, user-facing reason "the code alignment for this
// bump is missing from the branch") — a permission the operator must grant,
// diagnosed as work the bot lost.
//
// The fix is a probe before the push, and its SCOPE is the whole risk in both
// directions:
//
//   - too narrow and the 403 comes back;
//   - too WIDE and the ordinary Actions digest pin — a bump that touches
//     .github/workflows/** and needs no alignment at all, pushed by
//     Renovate's own App, which does hold `workflows:write` — is held, which
//     freezes the auto-maintenance lane as thoroughly as the 403 did.
//
// The delivery of THIS run is what `commit` is about to push out of the
// working tree: the bump is already committed and on the remote. So the probe
// judges HEAD..worktree plus what HEAD adds on top of origin/<branch>, never
// base..HEAD, and these cases drive both sides.
func TestDepUpdateGuardDeliveryProbeJudgesTheAlignmentNotTheBump(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	command := toolCommand(t, "dep-update-guard/main.bot", "delivery_probe")
	const workflow = ".github/workflows/ci.yml"
	const runtimeToken = "ghs_vetty_runtime_token"

	write := func(t *testing.T, ws, rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// bumpedRepo is the state Vetty actually runs in: a base branch, and on
	// top of it the dependency bot's OWN commit — already pushed — pinning an
	// Action digest inside .github/workflows/**.
	bumpedRepo := func(t *testing.T, origin string) string {
		t.Helper()
		ws := t.TempDir()
		gittest.Run(t, ws, "init", "-q", "-b", "main")
		write(t, ws, "src/app.ts", "export const x = 1\n")
		write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@aaaa\n")
		gittest.Run(t, ws, "add", "-A")
		gittest.Run(t, ws, "commit", "-qm", "base")
		gittest.Run(t, ws, "checkout", "-qb", "renovate/actions-digests")
		write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n")
		gittest.Run(t, ws, "add", "-A")
		gittest.Run(t, ws, "commit", "-qm", "chore(deps): pin actions/checkout to bbbb")
		gittest.Run(t, ws, "remote", "add", "origin", origin)
		// The bump is already ON the remote branch — the dependency bot
		// pushed it under its own App, which does hold workflows:write.
		gittest.Run(t, ws, "update-ref", "refs/remotes/origin/renovate/actions-digests", "HEAD")
		return ws
	}

	for _, tc := range []struct {
		name string
		// alignment leaves the working tree as `align` would have left it.
		alignment func(t *testing.T, ws string)
		gitlab    bool
		noService bool
		allow     bool
		// origin, prURL and token override the fixture's defaults; status,
		// when set, is the HTTP status the preflight stub answers with
		// instead of a verdict.
		origin string
		prURL  string
		token  string
		status int
		body   string
		noRepo bool
		// flaky answers the first request with a 502 and the next one
		// normally; reset DROPS the first request without answering it;
		// truncated answers the first request with headers promising a body
		// and then dies in the middle of it; dead points the probe at a port
		// nothing listens on; deadForge leaves the classic-token branch's
		// forge API unreachable.
		flaky     bool
		reset     bool
		truncated bool
		dead      bool
		deadForge bool
		// forgePort puts a listener on a port and points the origin and the
		// pull request at it, so wantForgeDials can count the connections the
		// classic-token branch actually makes: the probe speaks TLS to the
		// forge API and verifies the chain, so a stub can be DIALLED but
		// never answered -- which is exactly what the address is about.
		forgePort      bool
		wantForgeDials int
		// want is the outcome the row owes: a delivery, an answered denial
		// (the hold), or a preflight that did not answer.
		want      string
		wantCall  bool
		wantIn    string
		wantNotIn string
		wantCalls int
		// wantSlow requires the 2 s pause of a second attempt to have
		// happened. A retry whose only witness is the wording of the
		// message it prints is no witness: the wording is a literal.
		wantSlow bool
	}{
		{
			// THE false-positive case, and the one the auto-maintenance lane
			// lives on: Renovate's digest pin, nothing to align. The workflow
			// file moved in a commit that is already on the remote, so this
			// run pushes nothing touching it.
			name:      "the bump's own workflow commit is never held",
			alignment: func(*testing.T, string) {},
			allow:     false, want: probeDeliver, wantCall: false,
		},
		{
			name: "an ordinary code alignment is delivered without asking",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, "src/app.ts", "export const x = 2\n")
			},
			allow: false, want: probeDeliver, wantCall: false,
		},
		{
			name: "an unstaged workflow alignment with a denied token is held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          fetch-depth: 0\n")
			},
			allow: false, want: probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			name: "a staged workflow alignment is held too",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          persist-credentials: false\n")
				gittest.Run(t, ws, "add", workflow)
			},
			allow: false, want: probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			name: "a workflow the alignment CREATED is held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, ".github/workflows/new-action.yml", "jobs:\n  t:\n    steps: []\n")
			},
			allow: false, want: probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			name: "a workflow alignment the token may push proceeds",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          fetch-depth: 0\n")
			},
			allow: true, want: probeDeliver, wantCall: true,
		},
		{
			// No proof service (a local run, or a server too old to inject the
			// var): the permission cannot be established, so the push is not
			// attempted. Fail-closed, and the reason says what to do.
			name: "no preflight service: the workflow alignment is refused, not attempted",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@cccc\n")
			},
			noService: true, want: probeUnavailable, wantCall: false, wantIn: "relaunch",
		},
		{
			// The GitHub workflows permission does not exist on GitLab: a
			// merge request must never be held by this probe.
			name: "a GitLab merge request is outside this permission",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@dddd\n")
			},
			// The credential a GitLab connection mounts is a GitLab one:
			// the exemption is read from the token's kind, and a `ghs_`
			// installation token on a GitLab remote is a combination
			// iterion's own minting cannot produce.
			gitlab: true, token: "glpat-vetty_project_token",
			allow: false, want: probeDeliver, wantCall: false,
		},
		{
			// pkg/forge.ParsePullURL -- the repository's own PR-URL parser --
			// accepts three shapes, and Vetty's own post_feedback accepts all
			// three. The GitHub `workflows` permission does not exist on
			// Forgejo, so a pull there must never be held by this probe: the
			// hold's stated cure, approving an App's delivery permissions,
			// could not be performed on that forge at all.
			name: "a Forgejo pull request is outside this permission",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@eeee\n")
			},
			origin: "https://forge.example/acme/widgets.git",
			prURL:  "https://forge.example/acme/widgets/pulls/7",
			token:  "a7f3c9d1e5b80426fa91c3d7e4b208615c9fd3a0",
			want:   probeDeliver, wantCall: false, wantIn: "Forgejo/Gitea pull request",
		},
		{
			// The forge the PUSH goes to decides the exemption, and that is
			// the ORIGIN. GitHub's REST API spells this very pull request
			// /pulls/7 — the path Forgejo serves its own on — so a probe that
			// read the path first let a github.com pull request exempt itself
			// and turned the guard off on the one forge family where the
			// permission being proved is the permission at stake.
			name: "a github.com pull request written in the /pulls/ form is still asked about",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@7777\n")
			},
			prURL: "https://github.com/acme/widgets/pulls/7",
			want:  probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			// The same pull request as its REST URL. api.github.com is not
			// the origin's host, so the repository-agreement term refuses to
			// certify it — a refusal, never the silent exemption the path
			// shape used to buy.
			name: "the REST URL of a github.com pull request is refused, not exempted",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@8888\n")
			},
			prURL: "https://api.github.com/repos/acme/widgets/pulls/7",
			want:  probeUnavailable, wantCall: false, wantIn: "different repositories",
		},
		{
			// The `/-` separator is OPTIONAL in the repository's parser, and a
			// self-hosted GitLab still serves the older shape.
			name: "a GitLab merge request without the /- separator is outside it too",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@ffff\n")
			},
			origin: "https://gitlab.example/acme/widgets.git",
			prURL:  "https://gitlab.example/acme/widgets/merge_requests/7",
			token:  "glpat-vetty_project_token",
			want:   probeDeliver, wantCall: false,
		},
		{
			// /pull/7/files is the same pull request as /pull/7. Refusing it
			// held the alignment on the one forge family where the permission
			// being proved is exactly the permission at stake.
			name: "a GHES pull request URL with a path suffix is still asked about",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@1111\n")
			},
			origin: "https://ghe.example/acme/widgets.git",
			prURL:  "https://ghe.example/acme/widgets/pull/7/files",
			allow:  true, want: probeDeliver, wantCall: true,
		},
		{
			// GitHub owner/repo are case-insensitive, and the endpoint this
			// probe asks compares the pair with strings.EqualFold
			// (pkg/server/forge_delivery_preflight.go). Two spellings of one
			// repository must not become a refusal whose reason is false.
			name: "a clone URL spelled in another case names the same repository",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@2222\n")
			},
			origin: "https://github.com/Acme/Widgets.git",
			allow:  true, want: probeDeliver, wantCall: true,
		},
		{
			// Where the hold STOPS. GitHub restricts .github/workflows/** and
			// nothing else under .github/, so an alignment rewriting
			// renovate.json5 -- the file the Actions-pinning pilot needs
			// rewritten -- must not be held under a workflow permission.
			name: "a .github file that is not a workflow is delivered without asking",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, ".github/renovate.json5", "{}\n")
			},
			want: probeDeliver, wantCall: false,
		},
		{
			// A credential-scope guard: without it the probe can certify
			// repository A's permission for a push going to repository B.
			name: "a PR and a push origin naming different repositories is refused",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@3333\n")
			},
			prURL: "https://github.com/acme/other/pull/7",
			want:  probeUnavailable, wantCall: false, wantIn: "different repositories",
		},
		{
			// No recognised pull/merge-request shape, a non-GitHub host and a
			// credential that is not a GitHub one either: nothing here says
			// which forge enforces what, so the question cannot be asked and
			// the push is not attempted.
			name: "a forge the probe cannot identify is refused",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@4444\n")
			},
			origin: "https://forge.example/acme/widgets.git",
			prURL:  "https://forge.example/acme/widgets",
			token:  "a7f3c9d1e5b80426fa91c3d7e4b208615c9fd3a0",
			want:   probeUnavailable, wantCall: false, wantIn: "cannot identify the forge",
		},
		{
			// The credential names GitHub (an installation token is minted by
			// it), so the forge is identified — and the URL still names no
			// pull request. The endpoint refuses such a URL with a 400, so
			// asking would spend the digest on a question nobody can answer
			// and report it as a service that stayed silent.
			name: "a URL naming no pull request is refused before the question",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@4545\n")
			},
			origin: "https://ghe.corp.example/acme/widgets.git",
			prURL:  "https://ghe.corp.example/acme/widgets",
			want:   probeUnavailable, wantCall: false,
			wantIn: "needs the GitHub pull request this delivery targets",
		},
		{
			// `git commit` carries the INDEX. A path whose staged content
			// differs from HEAD while its working-tree content matches HEAD is
			// absent from `diff HEAD` and lands in the commit anyway -- and
			// `align`'s own prompt authorises the stash/checkout dance that
			// separates an index from a working tree.
			name: "a workflow staged and then restored in the working tree is held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          ref: staged\n")
				gittest.Run(t, ws, "add", workflow)
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n")
			},
			want: probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			// `commit` pushes HEAD:<branch>, so an alignment a node already
			// committed is delivered by this run even though the working tree
			// is clean. The bump is on origin/<branch> -- the dependency bot
			// pushed it under its own App -- so reading what HEAD adds on top
			// of the remote branch cannot see the bump.
			name: "a workflow alignment already committed locally is held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@bbbb\n        with:\n          fetch-depth: 2\n")
				gittest.Run(t, ws, "add", workflow)
				gittest.Run(t, ws, "commit", "-qm", "align: workflow inputs")
			},
			want: probeHold, wantCall: true, wantIn: "workflows",
		},
		{
			// A 5xx on someone else's dependency PR must not post a red
			// REQUIRED check naming a permission problem that does not
			// exist. A persistent outage still fails closed -- but it is
			// asked twice, and the REASON names the service, not the grant.
			name: "a preflight outage is not published as a missing permission",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@5555\n")
			},
			status: http.StatusBadGateway,
			want:   probeUnavailable, wantCall: true, wantIn: "did not answer (asked twice)",
			wantNotIn: "is missing or unverified", wantCalls: 2,
		},
		{
			// The same blip, cleared on the second request. The preflight
			// and the publish endpoint are one server (both are built from
			// the same base in pkg/server/forge_publish.go), so a rolling
			// restart that 502s one request would otherwise hold an
			// alignment and red a third party's PR for a few seconds of
			// deploy.
			name: "a preflight outage that clears on the retry is never published",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@8888\n")
			},
			flaky: true, allow: true,
			want: probeDeliver, wantCall: true, wantCalls: 2,
		},
		{
			// NOT an answer about any permission: the endpoint carries a
			// verdict in a 200 body (writeJSON) and writes its 4xx through
			// httpError, an error envelope. So a 401 on the run grant — like
			// every other status — is asked again once and then reported as
			// the service, never as a permission the credential lacks.
			name: "a preflight that refuses the run grant is not a permission verdict",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@7070\n")
			},
			status: http.StatusUnauthorized,
			want:   probeUnavailable, wantCall: true, wantIn: "did not answer (asked twice)",
			wantNotIn: "is missing or unverified", wantCalls: 2,
		},
		{
			// The same rollout, one instant earlier: the pod going down
			// DROPS the request it is holding instead of answering it. urllib
			// surfaces that as http.client.RemoteDisconnected -- a
			// ConnectionResetError, NOT a urllib.error.URLError -- so a retry
			// written around URLError alone publishes a few seconds of deploy
			// as a permission verdict on a third party's pull request.
			name: "a preflight that drops the first connection is asked again, not published",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@5151\n")
			},
			reset: true, allow: true,
			want: probeDeliver, wantCall: true, wantCalls: 2, wantSlow: true,
		},
		{
			// Nothing listening at all: a transport failure is not a
			// verdict about any permission, and the retry must cover it
			// too -- a restarting pod refuses the connection before it
			// starts answering 502s.
			name: "an unreachable preflight is not a missing permission",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@9999\n")
			},
			dead: true,
			want: probeUnavailable, wantCall: false, wantIn: "did not answer (asked twice)",
			wantNotIn: "is missing or unverified", wantSlow: true,
		},
		{
			// A classic PAT takes the forge-API branch, BEFORE the
			// installation-token question is ever asked, and it owes the same
			// contract: no response is not an answer about any scope. The
			// reason names the service that went silent instead of falling
			// into the catch-all, which says only "verify the credential and
			// preflight service".
			name: "an unreachable forge API is not a missing scope",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@4242\n")
			},
			token:     "ghp_classic_personal_access_token",
			origin:    "https://127.0.0.1/acme/widgets.git",
			prURL:     "https://127.0.0.1/acme/widgets/pull/7",
			deadForge: true,
			want:      probeUnavailable, wantCall: false, wantIn: "the forge API did not answer (asked twice)",
			wantNotIn: "could not establish delivery permission", wantSlow: true,
		},
		{
			// GitHub's refusal, and this preflight, are about an APP push:
			// forge_delivery_preflight.go denies on conn.Kind before it reads
			// any permission, so a PAT or OAuth connection would be held for
			// ever under a cure -- approve the App's delivery permissions --
			// that an operator with no App cannot perform. A fine-grained PAT
			// carrying Workflows:write performs this push today.
			name: "a non-App credential is not held for an App-only restriction",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@6666\n")
			},
			token: "github_pat_vetty_fine_grained_token",
			want:  probeDeliver, wantCall: false, wantIn: "not a GitHub App installation token",
		},
		{
			// The whole class, one row per kind the doctrine names as
			// UNVERIFIED (docs/auto-maintenance.md): an external App token
			// carries no managed mint evidence, so nothing here can prove it
			// either way and the push reports its own refusal.
			name: "an external App token is delivered unverified, not held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@6a6a\n")
			},
			token: "v1.vetty_external_app_token",
			want:  probeDeliver, wantCall: false, wantIn: "not a GitHub App installation token",
		},
		{
			// A user-to-server token has no X-OAuth-Scopes list and no mint
			// proof: the third unverifiable kind.
			name: "a user-to-server token is delivered unverified, not held",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@6b6b\n")
			},
			token: "ghu_vetty_user_to_server_token",
			want:  probeDeliver, wantCall: false, wantIn: "not a GitHub App installation token",
		},
		{
			// Not a usable checkout at all. The refusal used to name
			// `base_ref`, a launch var this probe never reads — it binds
			// BASE=HEAD — in a feature whose whole job is to stop reporting
			// one cause as another.
			name:      "a workspace that is not a git checkout is refused by name",
			alignment: func(*testing.T, string) {},
			noRepo:    true,
			want:      probeUnavailable, wantCall: false, wantIn: "readable git checkout",
		},
		{
			// 200 and not a verdict: an intercepting proxy's HTML. Unreadable
			// is not refused — it is unknown, and the reason says which.
			name: "an unreadable preflight answer is not a missing permission either",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@7777\n")
			},
			body: "<html>502 Bad Gateway</html>",
			want: probeUnavailable, wantCall: true, wantIn: "did not answer (asked twice)",
			wantNotIn: "is missing or unverified", wantCalls: 2,
		},
		{
			// The same dying pod, one instant later again: the headers are
			// already out and the body is cut in the middle. urllib does NOT
			// raise on that — http.client.HTTPResponse.read(amt) returns a
			// SHORT read on a Content-Length body and only the chunked path
			// raises — so a retry written around the exception alone publishes
			// a few seconds of deploy as a permission verdict.
			name: "a preflight answer cut mid-body is asked again, not published",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@5252\n")
			},
			truncated: true, allow: true,
			want: probeDeliver, wantCall: true, wantCalls: 2, wantSlow: true,
		},
		{
			// A GHES does not have to listen on 443, and the classic-token
			// branch builds its API base out of the origin. parsed.hostname
			// DROPS the port, so the question went to an address nobody
			// serves and the refusal named an outage that was not real. What
			// this row can see is the ADDRESS: zero connections to the port
			// the origin names, against two once it is kept.
			name: "a forge API on a non-443 port is dialled, not declared unreachable",
			alignment: func(t *testing.T, ws string) {
				write(t, ws, workflow, "jobs:\n  t:\n    steps:\n      - uses: actions/checkout@4343\n")
			},
			token:     "ghp_classic_personal_access_token",
			forgePort: true, wantForgeDials: 2,
			want: probeUnavailable, wantCall: false, wantSlow: true,
			wantIn:    "the forge API did not answer (asked twice)",
			wantNotIn: "is missing or unverified",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, prURL := "https://github.com/acme/widgets.git", "https://github.com/acme/widgets/pull/7"
			if tc.gitlab {
				origin, prURL = "https://gitlab.example/acme/widgets.git", "https://gitlab.example/acme/widgets/-/merge_requests/7"
			}
			if tc.origin != "" {
				origin = tc.origin
			}
			if tc.prURL != "" {
				prURL = tc.prURL
			}
			// The credential the run mounts: the probe may only ask about
			// THIS one, and what it does with it depends on its kind.
			token := runtimeToken
			if tc.token != "" {
				token = tc.token
			}
			// The forge API's address. It has to exist BEFORE the fixture,
			// because the origin is what carries its port.
			var forgeDials atomic.Int32
			if tc.forgePort {
				l, lerr := net.Listen("tcp", "127.0.0.1:0")
				if lerr != nil {
					t.Fatal(lerr)
				}
				defer func() { _ = l.Close() }()
				go func() {
					for {
						conn, aerr := l.Accept()
						if aerr != nil {
							return
						}
						forgeDials.Add(1)
						_ = conn.Close()
					}
				}()
				origin = "https://" + l.Addr().String() + "/acme/widgets.git"
				prURL = "https://" + l.Addr().String() + "/acme/widgets/pull/7"
			}
			ws := bumpedRepo(t, origin)
			if tc.noRepo {
				// Not a checkout at all — nothing is removed, the probe is
				// simply pointed at an empty directory.
				ws = t.TempDir()
			}
			tc.alignment(t, ws)

			var calls atomic.Int32
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if tc.reset && n == 1 {
					// The instance going down drops the request it holds.
					conn, _, hijackErr := w.(http.Hijacker).Hijack()
					if hijackErr != nil {
						t.Error(hijackErr)
						return
					}
					_ = conn.Close()
					return
				}
				if tc.truncated && n == 1 {
					// The instance dies with the headers already out: a body
					// shorter than the Content-Length it promised.
					conn, _, hijackErr := w.(http.Hijacker).Hijack()
					if hijackErr != nil {
						t.Error(hijackErr)
						return
					}
					_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 300\r\n\r\n{\"ok\": true, \"code\": \"\", \"rea"))
					_ = conn.Close()
					return
				}
				if tc.flaky && n == 1 {
					// The instance that is going down answers this one.
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				if tc.body != "" {
					// An answer that is not a verdict — a proxy's error page is
					// the ordinary way this arrives.
					_, _ = w.Write([]byte(tc.body))
					return
				}
				if tc.status != 0 {
					// Transport trouble, not a verdict: the probe must not read
					// a 502 as a permission the credential does not hold.
					w.WriteHeader(tc.status)
					return
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				// The endpoint answers about the token the run actually holds,
				// identified by digest. A probe that asked about anything else
				// would certify a credential this push does not use.
				sum := sha256.Sum256([]byte(token))
				if payload["credential_sha256"] != hex.EncodeToString(sum[:]) {
					t.Errorf("preflight asked about %q, want the digest of the mounted token", payload["credential_sha256"])
				}
				if payload["pr_url"] != prURL {
					t.Errorf("preflight asked about %q, want %q", payload["pr_url"], prURL)
				}
				if got := r.Header.Get("X-Iterion-Run"); got != "run-token" {
					t.Errorf("preflight authenticated with %q, want the run grant", got)
				}
				code := "FORGE_PERMISSION_DENIED"
				reason := "workflows:write is missing or unverified on the runtime token; an operator must approve the App's delivery permissions, refresh its token and relaunch"
				if tc.allow {
					code, reason = "", "runtime token has confirmed workflows:write and contents:write"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": tc.allow, "code": code, "reason": reason})
			}))
			defer stub.Close()

			tokenPath := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			// Neither the host's gh credentials nor a stray GITHUB_TOKEN may
			// stand in for the token this run actually mounts.
			t.Setenv("GH_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "")

			checkURL := stub.URL
			if tc.noService {
				checkURL = ""
			}
			if tc.dead {
				l, lerr := net.Listen("tcp", "127.0.0.1:0")
				if lerr != nil {
					t.Fatal(lerr)
				}
				checkURL = "http://" + l.Addr().String() + "/api/v1/forge/delivery-preflight"
				_ = l.Close()
			}
			cmd := command
			for ref, val := range map[string]string{
				"{{vars.workspace_dir}}":                ws,
				"{{vars.pr_url}}":                       prURL,
				"{{vars.forge_delivery_preflight_url}}": checkURL,
				"{{vars.forge_publish_token}}":          "run-token",
				"{{secrets.forge_token.path}}":          tokenPath,
			} {
				cmd = strings.ReplaceAll(cmd, ref, val)
			}
			if rest := firstRef(cmd); rest != "" {
				t.Fatalf("unsubstituted ref %s left in the probe command — it would run in a state production never reaches", rest)
			}

			if tc.deadForge {
				// The classic-token branch builds https://<origin host>/api/v3
				// out of the remote, so "the forge API is unreachable" is this
				// machine's own :443. A listener there would answer the
				// question this case is about nobody answering.
				if conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:443", time.Second); dialErr == nil {
					_ = conn.Close()
					t.Skip("something listens on 127.0.0.1:443 — this case needs the forge API unreachable")
				}
			}
			start := time.Now()
			out, err := exec.Command("sh", "-c", cmd).Output()
			elapsed := time.Since(start)
			if err != nil {
				stderr := ""
				if ee, ok := err.(*exec.ExitError); ok {
					stderr = string(ee.Stderr)
				}
				t.Fatalf("delivery_probe failed to execute: %v (out %q, stderr %q)", err, out, stderr)
			}
			var got deliveryVerdict
			if uerr := json.Unmarshal(out, &got); uerr != nil {
				t.Fatalf("delivery_probe output is not delivery_probe_state JSON: %v (out %q)", uerr, out)
			}

			if outcome := outcomeOf(t, got); outcome != tc.want {
				t.Fatalf("outcome = %s, want %s (reason %q)", outcome, tc.want, got.Reason)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("the verdict carries no reason — it becomes the hold's user-facing cause")
			}
			if tc.wantIn != "" && !strings.Contains(got.Reason, tc.wantIn) {
				t.Errorf("reason %q does not name %q", got.Reason, tc.wantIn)
			}
			if tc.wantNotIn != "" && strings.Contains(got.Reason, tc.wantNotIn) {
				t.Errorf("reason %q names %q — it diagnoses the cause as something else", got.Reason, tc.wantNotIn)
			}
			if strings.Contains(got.Reason, token) {
				t.Error("the refusal echoes the runtime token")
			}
			if (calls.Load() > 0) != tc.wantCall {
				t.Errorf("preflight called %d time(s), want called=%v — a delivery carrying no workflow file must not even ask", calls.Load(), tc.wantCall)
			}
			if tc.forgePort && int(forgeDials.Load()) != tc.wantForgeDials {
				t.Errorf("the forge API's own port was dialled %d time(s), want exactly %d — a question asked at the wrong address is reported as an outage of a service that is up", forgeDials.Load(), tc.wantForgeDials)
			}
			if tc.wantCalls != 0 && int(calls.Load()) != tc.wantCalls {
				t.Errorf("preflight called %d time(s), want exactly %d — a transport failure is asked again once, and only once", calls.Load(), tc.wantCalls)
			}
			// The second attempt, measured rather than read off the message.
			// The dead and deadForge cases point at a DIFFERENT server, so no
			// call counter can see them; the 2 s pause is the only witness
			// that the question was asked twice at all.
			if tc.wantSlow && elapsed < 2*time.Second {
				t.Errorf("the probe answered in %v — under the 2 s pause, so the question was asked once and the second attempt never happened", elapsed.Round(time.Millisecond))
			}
		})
	}
}

// The probe is worth nothing unless it sits BEFORE the push, on the one path
// that pushes, and unless its refusal reaches both the pull request (the hold
// comment and the red gate, which is what a reviewer and the ruleset read)
// and the run (`failure_code`, which is what a machine reads).
func TestDepUpdateGuardProbesDeliveryBeforePushing(t *testing.T) {
	wf := compilePlanPhaseBot(t, "dep-update-guard")

	// The server injects forge_delivery_preflight_url on every gating launch,
	// but the IR DROPS a launch var the bot does not declare — and with no
	// URL the probe refuses every workflow alignment. The declaration is the
	// whole opt-in.
	if v, ok := wf.Vars["forge_delivery_preflight_url"]; !ok {
		t.Error("the bot does not declare forge_delivery_preflight_url — the server's injection is dropped by the IR and every workflow alignment is refused for want of a proof service")
	} else if !v.HasDefault || fmt.Sprintf("%v", v.Default) != "" {
		t.Errorf("forge_delivery_preflight_url must default to empty (a local run has no server), got %v", v.Default)
	}

	probe, ok := wf.Nodes["delivery_probe"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("delivery_probe is %T, want a deterministic tool node", wf.Nodes["delivery_probe"])
	}
	// The scope assertion, pinned at the binding: base_ref is the BUMP's diff
	// — the dependency bot's own commits, already on the remote under its own
	// App — and judging it holds every Actions pin the lane depends on.
	if !strings.Contains(probe.Command, "BASE=HEAD") {
		t.Error("delivery_probe does not bind BASE=HEAD: this run pushes what `commit` makes out of the working tree, not the branch")
	}
	if strings.Contains(probe.Command, "vars.base_ref") {
		t.Error("delivery_probe judges base_ref..HEAD — that is the dependency bot's own bump, so every Actions digest pin would be held for a permission its push never needed")
	}

	var toProbe, toCommit, toHold, toStandDown *ir.Edge
	var fromProbe []*ir.Edge
	for _, e := range wf.Edges {
		if e.From == "delivery_probe" {
			fromProbe = append(fromProbe, e)
		}
		switch {
		case e.From == "validate_gate" && e.To == "commit":
			t.Error("validate_gate still routes straight to commit: an alignment touching .github/workflows/** is pushed with a token that may not carry workflows:write, and GitHub's refusal is reported as a lost alignment")
		case e.From == "validate_gate" && e.To == "delivery_probe":
			toProbe = e
		case e.From == "delivery_probe" && e.To == "commit":
			toCommit = e
		case e.From == "delivery_probe" && e.To == "post_feedback":
			toHold = e
		case e.From == "delivery_probe" && e.To == "preflight_standdown":
			toStandDown = e
		}
	}
	if toProbe == nil || toProbe.Condition != "stable" || toProbe.Negated {
		t.Fatalf("no `validate_gate -> delivery_probe when stable` edge: %s", describeEdge(toProbe))
	}
	if toCommit == nil || toCommit.Condition != "ok" || toCommit.Negated {
		t.Fatalf("no `delivery_probe -> commit when ok` edge: %s", describeEdge(toCommit))
	}
	// commit's inputs used to ride the validate_gate edge; moving the probe in
	// front of it must not drop them, or the agent commits with an empty
	// file list and the placeholders leak into its prompt.
	for _, key := range []string{"files_changed", "breaking_summary"} {
		if mappingOf(toCommit, key) == nil {
			t.Errorf("the edge into commit does not map %s — the agent reads the raw placeholder", key)
		}
	}
	// THE INVERSION, read off the graph. The hold is guarded on `denied` —
	// which only a WELL-FORMED ANSWER sets — and not on `not ok`, which every
	// transport failure, every unparseable body and every timeout also
	// satisfies. Three adversarial rounds each found one more shape that
	// reached a blocking verdict on a third party's pull request through
	// `not ok`; nothing reaches it through `denied` but an answered refusal.
	if toHold == nil || toHold.Condition != "denied" || toHold.Negated {
		t.Fatalf("no `delivery_probe -> post_feedback when denied` edge — either the answered refusal never reaches the pull request, or the hold is guarded on a condition a preflight that did not answer also satisfies: %s", describeEdge(toHold))
	}
	// And the complement is the FALLBACK, so an outcome nobody foresaw lands
	// there by construction rather than by enumeration.
	if toStandDown == nil || !toStandDown.IsElse {
		t.Fatalf("no `delivery_probe -> preflight_standdown else` edge: a preflight that did not answer routes nowhere, or routes somewhere guarded: %s", describeEdge(toStandDown))
	}
	// COUNTED: a fourth edge out of the probe is another path to a verdict,
	// and the `else` would hide it — it fires only when no `when` matched.
	if len(fromProbe) != 3 {
		var all []string
		for _, e := range fromProbe {
			all = append(all, describeEdge(e))
		}
		t.Fatalf("%d edges leave delivery_probe (%s) — the probe answers three outcomes and owes exactly one edge each", len(fromProbe), strings.Join(all, ", "))
	}
	if m := mappingOf(toHold, "verdict"); m == nil || strings.Trim(m.Raw, `"`) != "hold_delivery_permission" {
		t.Errorf("the refusal edge stamps verdict %v, want \"hold_delivery_permission\"", m)
	}
	if m := mappingOf(toHold, "align_summary"); m == nil || !strings.Contains(m.Raw, "outputs.delivery_probe.reason") {
		t.Errorf("the refusal edge does not carry the probe's reason into the comment, so the hold names no cause: %v", m)
	} else if !strings.Contains(m.Raw, "Delivery refused before the push") {
		// align_summary renders under the comment's "Code alignment" heading:
		// unlabelled, a permission refusal there reads as a description of the
		// alignment, which is the wrong-cause diagnosis in another costume.
		t.Errorf("the probe's reason reaches the comment unlabelled: %q", m.Raw)
	}
	// The verdict pins the revision it covers, and on this path the only
	// revision that exists is the head the run READ: nothing was committed, so
	// outputs.commit.sha is empty and the gate would certify nothing at all.
	if m := mappingOf(toHold, "audited_sha"); m == nil || !strings.Contains(m.Raw, "outputs.prepare.head_sha") {
		t.Errorf("the refusal edge pins audited_sha to %v, want the head this run read — no commit was made on this path", m)
	}

	// The stand-down: what the unanswered path still owes the pull request.
	// Nothing is published on it — a service that did not answer proves
	// nothing about the credential — but an arming an EARLIER pass placed
	// outlives the run that placed it, and the forge merges the bump the
	// moment its own checks go green. So the path runs through
	// `arm_automerge` with a verdict that is not green.
	stand, ok := wf.Nodes["preflight_standdown"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("preflight_standdown is %T, want a deterministic *ir.ComputeNode", wf.Nodes["preflight_standdown"])
	}
	standExprs := map[string]string{}
	for _, ex := range stand.Exprs {
		standExprs[ex.Key] = ex.Raw
	}
	for _, green := range []string{"clean", "committed"} {
		if strings.Contains(standExprs["verdict"], green) {
			t.Errorf("the stand-down hands arm_automerge the verdict %s — arm_automerge arms on %q, so the bump would merge without the alignment", standExprs["verdict"], green)
		}
	}
	if standExprs["verdict"] == "" {
		t.Error("the stand-down names no verdict: arm_automerge reads it to decide, and an unmapped field leaves the raw placeholder")
	}
	// A bool reaching a typed field as the TEXT "false" is truthy wherever it
	// is read (C152): the gate it describes never landed on this path.
	if standExprs["gate_posted"] != "false" {
		t.Errorf("the stand-down's gate_posted = %q, want the expression false — no comment and no commit status are posted on this path", standExprs["gate_posted"])
	}
	if !strings.Contains(standExprs["audited_sha"], "outputs.prepare.head_sha") {
		t.Errorf("the stand-down pins audited_sha to %q, want the head this run read — nothing was committed", standExprs["audited_sha"])
	}
	for _, e := range wf.Edges {
		if e.From == "preflight_standdown" && e.To != "arm_automerge" {
			t.Errorf("preflight_standdown -> %s: the one refusal that publishes NOTHING on the pull request must reach the stand-down and nothing else", e.To)
		}
		if e.From == "preflight_standdown" && e.To == "arm_automerge" {
			for _, key := range []string{"pr_url", "verdict", "gate_posted", "gate_state", "gate_sha", "audited_sha", "committed_sha"} {
				if mappingOf(e, key) == nil {
					t.Errorf("the edge into arm_automerge does not map %s — an unmapped input leaves the literal placeholder in the script", key)
				}
			}
		}
	}

	// And the run itself says which refusal it was. A hold comment is for a
	// human; `iterion runs list`, the studio and the alert sinks read the code.
	fn, ok := wf.Nodes["delivery_permission_denied"].(*ir.FailNode)
	if !ok {
		t.Fatalf("delivery_permission_denied is %T, want a named *ir.FailNode", wf.Nodes["delivery_permission_denied"])
	}
	if fn.Code != "FORGE_PERMISSION_DENIED" {
		t.Errorf("fail code = %q, want FORGE_PERMISSION_DENIED (the code Billy already stamps for this refusal)", fn.Code)
	}
	if fn.Message == nil || !strings.Contains(fn.Message.Raw, "outputs.delivery_probe.reason") {
		t.Error("the typed failure does not carry the probe's reason — the operator reads a restatement of the code")
	}
	// What the stand-down DID is arm_automerge's to report, never this
	// message's to assert: arming is off by default and then the forge is
	// deliberately not touched at all, and on an unreachable forge the same
	// reason says "a commitment to merge may still stand". An operator who
	// believes a stand-down that did not happen leaves the arming live.
	if fn.Message == nil || !strings.Contains(fn.Message.Raw, "outputs.arm_automerge.reason") {
		t.Error("the typed failure drops arm_automerge's reason — nothing tells the operator whether a commitment to merge still stands")
	} else if strings.Contains(fn.Message.Raw, "stood down") {
		t.Errorf("the typed failure asserts auto-merge stood down and then quotes a reason that contradicts it: %q", fn.Message.Raw)
	}
	// Terminal, not resumable: a resume re-clones the repository, so the
	// alignment this run wrote is gone, and nothing about the credential
	// changes by replaying the run. The cure is a new launch after the
	// operator has granted the permission.
	if fn.Resumable {
		t.Error("delivery_permission_denied is resumable — a resume re-clones the repo, losing the alignment, and asks the same credential the same question")
	}
	var into []*ir.Edge
	for _, e := range wf.Edges {
		if e.To == "delivery_permission_denied" {
			into = append(into, e)
		}
	}
	if len(into) == 0 {
		t.Fatal("nothing routes to delivery_permission_denied — the run ends green on a bump it refused to deliver")
	}
	// COUNTED, not "the last one wins": a second edge into the typed
	// failure is the bypass this placement exists to forbid — reached
	// earlier, it ends the run before arm_automerge stands a previous
	// pass's arming down, and the forge merges the bump without its
	// alignment the moment its own checks go green.
	if len(into) != 1 {
		var from []string
		for _, e := range into {
			from = append(from, describeEdge(e))
		}
		t.Fatalf("%d edges reach delivery_permission_denied (%s) — one of them can end the run before arm_automerge takes a previous pass's arming down", len(into), strings.Join(from, ", "))
	}
	reaches := into[0]
	// The refusal is typed only AFTER the comment, the gate and the stand-down
	// have been published: a hold that ends the run before arm_automerge
	// leaves an arming an earlier pass placed standing, and the forge merges
	// the bump the moment its checks go green.
	if reaches.From != "arm_automerge" {
		t.Errorf("delivery_permission_denied is reached from %q: route it after arm_automerge, which is what takes a previous pass's arming down", reaches.From)
	}
	if !strings.Contains(reaches.ExpressionSrc, "hold_delivery_permission") {
		t.Errorf("the edge into the typed failure is guarded by %q — it must fire only on the verdict this refusal published", describeEdge(reaches))
	}

	// The OTHER delivery refusal, typed apart from it: an unattended lane
	// must be able to tell an outage of ours — relaunch, nothing to approve —
	// from a permission an operator has to grant. One code for both would
	// make every preflight blip read as a configuration decision.
	un, ok := wf.Nodes["delivery_preflight_unavailable"].(*ir.FailNode)
	if !ok {
		t.Fatalf("delivery_preflight_unavailable is %T, want a named *ir.FailNode", wf.Nodes["delivery_preflight_unavailable"])
	}
	if un.Code == fn.Code {
		t.Errorf("a preflight that did not answer is typed %s, exactly like a credential that refused: the run says configuration decision where it means relaunch", un.Code)
	}
	if un.Code != "FORGE_PREFLIGHT_UNAVAILABLE" {
		t.Errorf("fail code = %q, want FORGE_PREFLIGHT_UNAVAILABLE", un.Code)
	}
	if un.Resumable {
		t.Error("delivery_preflight_unavailable is resumable — a resume re-clones the repo, losing the alignment, and the cure is a new launch once the service answers")
	}
	for _, ref := range []string{"outputs.delivery_probe.reason", "outputs.arm_automerge.reason"} {
		if un.Message == nil || !strings.Contains(un.Message.Raw, ref) {
			t.Errorf("the typed failure does not carry %s — the operator reads a restatement of the code, and nothing about whether a commitment to merge still stands", ref)
		}
	}
	var intoUnavailable []*ir.Edge
	for _, e := range wf.Edges {
		if e.To == "delivery_preflight_unavailable" {
			intoUnavailable = append(intoUnavailable, e)
		}
	}
	if len(intoUnavailable) != 1 || intoUnavailable[0].From != "arm_automerge" {
		var from []string
		for _, e := range intoUnavailable {
			from = append(from, describeEdge(e))
		}
		t.Fatalf("%d edges reach delivery_preflight_unavailable (%s) — want exactly one, from arm_automerge, which is what takes a previous pass's arming down", len(intoUnavailable), strings.Join(from, ", "))
	}
	if !strings.Contains(intoUnavailable[0].ExpressionSrc, "FORGE_PREFLIGHT_UNAVAILABLE") {
		t.Errorf("the edge into the unavailable failure is guarded by %q — it must fire on the probe's own code, which is the only signal on a path that published no verdict", describeEdge(intoUnavailable[0]))
	}
}

// describeEdge renders an edge (or its absence) for a failure message.
func describeEdge(e *ir.Edge) string {
	if e == nil {
		return "<no such edge>"
	}
	return e.From + " -> " + e.To + " (" + edgeGuard(e) + ")"
}
