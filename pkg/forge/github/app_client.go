package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AppConfig is the global GitHub-App identity (registered once on GitHub),
// shared across every installation. The private key never leaves the
// process; it is loaded from deployment config, not from Mongo.
type AppConfig struct {
	AppID         int64
	PrivateKeyPEM string
	AppSlug       string // for the install URL github.com/apps/<slug>/installations/new
	// ClientID/ClientSecret are the App's user-authorization OAuth
	// credentials. Optional: when both are set (and the App has "Request
	// user authorization (OAuth) during installation" enabled on GitHub),
	// the install callback verifies the completing user actually owns the
	// installation before minting a token for it — see
	// VerifyInstallationOwnership. Empty → verification is unavailable.
	ClientID     string
	ClientSecret string
}

func (c AppConfig) Configured() bool { return c.AppID != 0 && c.PrivateKeyPEM != "" }

// UserAuthConfigured reports whether the App carries the OAuth client
// credentials needed to verify installation ownership at connect time.
func (c AppConfig) UserAuthConfigured() bool { return c.ClientID != "" && c.ClientSecret != "" }

// RuntimeInstallationPermissions is the least-privilege permission subset an
// installation token minted for iterion is pinned to — exactly what the forge
// layer needs (read/push code, open+comment PRs, comment the source issue for
// the MR back-link, manage the per-repo webhook, the mandatory metadata
// baseline) and nothing more. Mirrors the App manifest (BuildAppManifest);
// pinning it at mint time means a token stays minimal even if the installation
// is later granted broader permissions on the forge.
func RuntimeInstallationPermissions() map[string]string {
	return map[string]string{
		"contents":         "write",
		"pull_requests":    "write",
		"issues":           "write", // finalize_mr posts the PR URL back on the source issue
		"metadata":         "read",
		"repository_hooks": "write",
	}
}

// DeliveryInstallationPermissions are what a bot needs to SHIP an application
// rather than only change code: publish the CI definition that builds it, and
// publish the resulting image.
//
// They are separated from the runtime baseline because `workflows: write` is a
// genuine escalation — an actor who can rewrite CI can run arbitrary code in
// it — so an App only gets them when the operator opts in at creation.
//
// GitHub enforces this hard, not softly: pushing a commit that touches
// .github/workflows/** with a token lacking `workflows` is REJECTED outright
// ("refusing to allow a GitHub App to create or update workflow … without
// `workflows` permission"), which blocks the whole build-and-deploy chain at
// its first step.
func DeliveryInstallationPermissions() map[string]string {
	return map[string]string{
		"workflows": "write",
		"packages":  "write",
	}
}

// SecurityReadInstallationPermissions is the grant set minted for the
// security-read token (org-wide Dependabot alerts): the vulnerability_alerts
// read permission plus the mandatory metadata baseline. It is a separate
// opt-in profile — NEVER folded into the runtime baseline — because alert
// data is sensitive (it names every vulnerable dependency of every repo) and
// only bots that declare the dependabot_tokens secret should ever hold it.
func SecurityReadInstallationPermissions() map[string]string {
	return map[string]string{
		"vulnerability_alerts": "read",
		"metadata":             "read",
	}
}

// ProjectsInstallationPermissions is the grant set minted ONLY for project-board
// calls (GitHub Projects v2, ADR-097): the ORGANIZATION-level projects
// permission plus the mandatory metadata baseline.
//
// It is a separate opt-in profile, never folded into the runtime baseline, for
// the same reason `administration` and `vulnerability_alerts` are: a token that
// can rewrite an org's roadmap is a broader privilege than one that can push a
// branch, and it is org-scoped — an existing installation cannot acquire it
// silently, an org owner has to approve the new grant.
func ProjectsInstallationPermissions() map[string]string {
	return map[string]string{
		"organization_projects": "write",
		"metadata":              "read",
	}
}

// IssuesReadInstallationPermissions is the grant set minted for READING issues
// (the forge→board sync's ListIssues/GetIssue) — the issues read permission
// plus the mandatory metadata baseline.
//
// It is its own profile rather than a reuse of the cached runtime token
// because that token carries issues:WRITE (finalize_mr posts back on the
// source issue): letting a listing ride it would hand a read the permission to
// rewrite every issue in the installation, and a sync pass reads far more
// often than anything writes.
func IssuesReadInstallationPermissions() map[string]string {
	return map[string]string{
		"issues":   "read",
		"metadata": "read",
	}
}

// IssuesWriteInstallationPermissions is the grant set minted for WRITING
// issues (board→forge push, a bot's reply on the source issue): the issues
// write permission plus the mandatory metadata baseline. Narrower than the
// runtime token, which also carries contents/pull_requests/hooks writes.
func IssuesWriteInstallationPermissions() map[string]string {
	return map[string]string{
		"issues":   "write",
		"metadata": "read",
	}
}

// IssueCommentInstallationPermissions is the grant set minted for POSTing a
// comment: issues write, pull_requests write, and the mandatory metadata
// baseline.
//
// It carries BOTH writes because the endpoint is shared and the permission is
// not: GitHub serves a pull request's comments from
// /repos/{owner}/{repo}/issues/{number}/comments — the same path as an
// issue's — but gates the call on `pull_requests` when that number is a pull
// request, and on `issues` when it is an issue. One call, two grants, decided
// by a number the client cannot classify without an extra round trip.
//
// Answering 403 "Resource not accessible by integration" is what a token
// short of either grant gets, and every caller posts a courtesy notice it
// logs at Debug and drops — so the gap would be invisible.
func IssueCommentInstallationPermissions() map[string]string {
	return map[string]string{
		"issues":        "write",
		"pull_requests": "write",
		"metadata":      "read",
	}
}

// IssueReadOrPullInstallationPermissions is the grant set minted for a read
// whose target may be either: issues read, pull_requests read, and the
// mandatory metadata baseline.
//
// It is the read counterpart of IssueCommentInstallationPermissions, and it
// exists for the same reason: GET /repos/{owner}/{repo}/issues/{number} serves
// a pull request too, and GitHub gates the call on the RESOURCE, not the path
// — `pull_requests` for a PR, `issues` for an issue. A number the client
// cannot classify without an extra round trip needs both.
//
// GitHub hides what a token cannot see, so the refusal is a 404, not a 403 —
// indistinguishable from a deleted PR at the call site. The hold-label veto
// reads through here and fails closed, which stops the autofix and
// gate-relaunch lanes launching at all; that is why the grant is not optional.
//
// ListIssues deliberately does NOT use this profile: it reads the issues
// COLLECTION, which is gated on `issues` alone, and the board-sync pass runs
// often enough that keeping its token narrow is worth a fourth cached set.
func IssueReadOrPullInstallationPermissions() map[string]string {
	return map[string]string{
		"issues":        "read",
		"pull_requests": "read",
		"metadata":      "read",
	}
}

// PermissionChecks is the grant the board card's CI panel lists a ref's
// check-runs with. It is requested at App creation (BuildAppManifest) and
// minted ONLY into the CI profiles below — never into the runtime baseline:
// the management and run tokens are minted from that baseline, and an
// installation approved before the grant existed (every one, until its owner
// approves the pending request) would then fail EVERY mint, not just the CI
// read.
const PermissionChecks = "checks"

// PullListInstallationPermissions is the grant set minted for listing pull
// requests: pull_requests read plus the mandatory metadata baseline.
//
// The pull profiles exist for the same reason the issue profiles do: a
// PullClient method implemented on *AdminClient alone is invisible to the
// `admin.(forge.PullClient)` the card's PR/CI panel asserts, and the App
// client is the connection shape the connect wizard creates by default. Each
// profile is the endpoint's own rule (GitHub's published per-endpoint
// permission data), no wider — a read never acquires a write, and none of
// them rides the runtime baseline, whose contents/pull_requests/issues/hooks
// WRITES no read here has a use for.
func PullListInstallationPermissions() map[string]string {
	return map[string]string{
		"pull_requests": "read",
		"metadata":      "read",
	}
}

// PullGetInstallationPermissions is the grant set minted for reading ONE pull
// request. GitHub gates GET /repos/{owner}/{repo}/pulls/{number} on contents
// read as well as pull_requests read — the object carries content-derived
// fields (mergeability, diff stats) — which the collection read does not.
func PullGetInstallationPermissions() map[string]string {
	return map[string]string{
		"pull_requests": "read",
		"contents":      "read",
		"metadata":      "read",
	}
}

// PullWriteInstallationPermissions is the grant set minted for opening or
// updating a pull request: pull_requests write plus the metadata baseline.
func PullWriteInstallationPermissions() map[string]string {
	return map[string]string{
		"pull_requests": "write",
		"metadata":      "read",
	}
}

// PullMergeInstallationPermissions is the grant set minted for merging a pull
// request. GitHub gates PUT .../pulls/{number}/merge on contents WRITE (the
// merge writes the base branch), not on pull_requests write; the
// pull_requests read serves the re-fetch that returns the merged ref, and
// contents write also covers the optional source-branch deletion.
func PullMergeInstallationPermissions() map[string]string {
	return map[string]string{
		"contents":      "write",
		"pull_requests": "read",
		"metadata":      "read",
	}
}

// CIStatusInstallationPermissions is the grant set minted for the CURRENT CI
// state of a ref: checks read for the check-runs list, statuses read for the
// legacy combined commit status, plus the metadata baseline.
func CIStatusInstallationPermissions() map[string]string {
	return map[string]string{
		PermissionChecks:   "read",
		PermissionStatuses: "read",
		"metadata":         "read",
	}
}

// CIHistoryInstallationPermissions is the grant set minted for the CI history
// of a ref, which reads check-runs alone.
func CIHistoryInstallationPermissions() map[string]string {
	return map[string]string{
		PermissionChecks: "read",
		"metadata":       "read",
	}
}

// MissingCIPermissions lists the grants the board card's CI panel needs and
// an installation does NOT have, so the connection health view names them
// before a card shows a dead panel. Empty when nothing is missing, or when
// the grant set is unknown (absence of data is not evidence of a gap).
func MissingCIPermissions(granted map[string]string) []string {
	if len(granted) == 0 {
		return nil
	}
	var missing []string
	for _, name := range []string{PermissionChecks, PermissionStatuses} { // sorted: stable output
		if _, ok := granted[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// MissingProjectPermissions lists the project-board grants an installation does
// NOT have, so a board binding fails at BIND time naming the missing permission
// rather than hours later on the first status write. Empty when nothing is
// missing, or when the grant set is unknown (absence of data is not evidence of
// a gap).
func MissingProjectPermissions(granted map[string]string) []string {
	if len(granted) == 0 {
		return nil
	}
	if _, ok := granted["organization_projects"]; !ok {
		return []string{"organization_projects"}
	}
	return nil
}

// MissingSecurityPermissions lists the security-read grants an installation
// does NOT have, so the connection health view can name them before an
// hourly vuln-watch run discovers the 422 in production. Empty when nothing
// is missing (or when the grant set is unknown — absence of data is not
// evidence of a gap).
func MissingSecurityPermissions(granted map[string]string) []string {
	if len(granted) == 0 {
		return nil
	}
	var missing []string
	if _, ok := granted["vulnerability_alerts"]; !ok {
		missing = append(missing, "vulnerability_alerts")
	}
	return missing
}

// MissingDeliveryPermissions lists the delivery grants an installation does
// NOT have, so the connection health view can name them BEFORE a run spends
// hours discovering them at push time. Empty when nothing is missing (or when
// the grant set is unknown — absence of data is not evidence of a gap).
func MissingDeliveryPermissions(granted map[string]string) []string {
	if len(granted) == 0 {
		return nil
	}
	var missing []string
	for _, name := range []string{"packages", "workflows"} { // sorted: stable output
		if _, ok := granted[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// RuntimePermissionsFor narrows the permissions iterion WANTS to those the
// installation actually granted.
//
// Minting is not forgiving: asking for a permission the installation lacks
// fails the whole call (422), so a token cannot simply request the superset.
// Intersecting keeps one code path for both an App created before delivery
// permissions existed and one created with them.
//
// granted == nil means "unknown" (a connection stored before grants were
// recorded); those keep exactly the historical baseline.
func RuntimePermissionsFor(granted map[string]string) map[string]string {
	base := RuntimeInstallationPermissions()
	if len(granted) == 0 {
		return base
	}
	out := map[string]string{}
	for name, level := range base {
		if _, ok := granted[name]; ok {
			out[name] = level
		}
	}
	for name, level := range DeliveryInstallationPermissions() {
		if _, ok := granted[name]; ok {
			out[name] = level
		}
	}
	// An installation that granted nothing we recognise would yield an empty
	// map, which the mint reads as "no constraint" (the installation's FULL
	// set) — the opposite of least privilege. Keep the baseline instead.
	if len(out) == 0 {
		return base
	}
	return out
}

// InstallationTokenOptions narrows a minted installation token below the
// installation's full grant (least-privilege). Both fields are optional; a nil
// field means "don't constrain that dimension" (GitHub returns the
// installation's full set).
//
//   - Repositories: short repo names (e.g. "api", NOT "org/api") the token may
//     touch. Empty → all repositories in the installation.
//   - Permissions: the permission subset (a subset of the installation's own
//     grants). Empty → the installation's full permission set.
type InstallationTokenOptions struct {
	Repositories []string
	Permissions  map[string]string
}

// Installation is what GET /app/installations/{id} tells us about a live
// installation.
type Installation struct {
	Login   string
	HTMLURL string
	// Permissions is the grant the ORG actually approved, which can be
	// narrower than what the App requests (a permission added to an App after
	// installation stays pending until an owner approves it). Carrying it lets
	// the mint ask only for what exists, and lets the health probe name a
	// missing grant BEFORE a run spends hours discovering it at push time.
	Permissions map[string]string
}

// InstallationInfo returns the installation's account login, its GitHub
// settings page URL (html_url — the only place where the installation's repo
// scope and permission grants can be widened), and its approved permissions.
// App-JWT-authenticated: GET /app/installations/{id}.
func InstallationInfo(ctx context.Context, httpClient *http.Client, apiBase string, cfg AppConfig, installationID int64, now time.Time) (info Installation, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	jwt, err := signAppJWT(cfg.AppID, cfg.PrivateKeyPEM, now)
	if err != nil {
		return Installation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/app/installations/"+strconv.FormatInt(installationID, 10), nil)
	if err != nil {
		return Installation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Installation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Installation{}, forge.ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return Installation{}, statusErr("GET /app/installations/{id}", resp.StatusCode)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Installation{}, err
	}
	return Installation{Login: out.Account.Login, HTMLURL: out.HTMLURL, Permissions: out.Permissions}, nil
}

// MintInstallationToken trades the App JWT for a short-lived (≈1h)
// installation access token. apiBase is the REST API base (APIBaseFor). opts
// may be nil for an unconstrained (whole-installation) token, or narrow it to
// specific repositories + a permission subset (least-privilege).
func MintInstallationToken(ctx context.Context, httpClient *http.Client, apiBase string, cfg AppConfig, installationID int64, now time.Time, opts *InstallationTokenOptions) (string, time.Time, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	jwt, err := signAppJWT(cfg.AppID, cfg.PrivateKeyPEM, now)
	if err != nil {
		return "", time.Time{}, err
	}
	var body io.Reader
	if opts != nil {
		payload := map[string]any{}
		if len(opts.Repositories) > 0 {
			payload["repositories"] = opts.Repositories
		}
		if len(opts.Permissions) > 0 {
			payload["permissions"] = opts.Permissions
		}
		if len(payload) > 0 {
			raw, err := json.Marshal(payload)
			if err != nil {
				return "", time.Time{}, err
			}
			body = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", body)
	if err != nil {
		return "", time.Time{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", time.Time{}, forge.ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		// GitHub's 4xx body always names the cause (e.g. a 422 "there is at
		// least one repository that does not exist or is not accessible to the
		// … installation", or an ungranted permission). Surfacing it turns an
		// opaque "HTTP 422" into an actionable message instead of masking the
		// root cause (erreurs-explicites).
		err := mintTokenErr(resp)
		// A 422 whose body reports the requested permissions are NOT GRANTED is
		// a permanent config mismatch (the install was approved with a narrower
		// permission set than iterion now requests). Classify it as the terminal
		// forge.ErrPermissionsNotGranted so the refresh worker marks the
		// connection degraded and stops re-minting it every tick, while keeping
		// GitHub's own actionable message in the wrapped error.
		if resp.StatusCode == http.StatusUnprocessableEntity && isPermissionsNotGranted(err) {
			return "", time.Time{}, fmt.Errorf("%w: %w", forge.ErrPermissionsNotGranted, err)
		}
		return "", time.Time{}, err
	}
	var out struct {
		Token     string            `json:"token"`
		ExpiresAt string            `json:"expires_at"`
		Perms     map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	if exp.IsZero() {
		exp = now.Add(time.Hour)
	}
	return out.Token, exp, nil
}

// lastMintedPermissions remembers what the most recent RUNTIME token for an
// installation actually CARRIED, keyed by installation id.
//
// It exists because checking the installation's grant is necessary but NOT
// sufficient, and mistaking one for the other cost a full run: the
// installation had `workflows`, the minted token did not (it was pinned to the
// baseline), and a pre-flight that read the grant reported all-clear while the
// push was refused an hour later. The token is the thing that acts, so the
// token is the thing to inspect.
//
// Only the RUNTIME token is recorded — the one sealed into a run and used by
// the bot. AppClient.rest mints a separate MANAGEMENT token, deliberately
// pinned to the baseline for iterion's own API calls; recording that one would
// make the pre-flight report a narrow token for a run that will get a wide one,
// which is the same wrong-layer mistake in the opposite direction.
var lastMintedPermissions sync.Map // installationID(int64) → map[string]string

// RecordRuntimePermissions notes what a runtime token carried. Called by the
// mint paths that produce the token a RUN uses.
func RecordRuntimePermissions(installationID int64, perms map[string]string) {
	if len(perms) > 0 {
		lastMintedPermissions.Store(installationID, perms)
	}
}

// LastMintedPermissions returns the permissions carried by the most recent
// runtime token minted for an installation in this process, and whether one is
// known.
func LastMintedPermissions(installationID int64) (map[string]string, bool) {
	v, ok := lastMintedPermissions.Load(installationID)
	if !ok {
		return nil, false
	}
	perms, _ := v.(map[string]string)
	return perms, len(perms) > 0
}

// mintTokenErr wraps the base status error with GitHub's own explanation from
// the response body (message + first field error), so a 422 says WHY. The body
// is a short GitHub error JSON; a decode failure falls back to the bare status
// error. Never includes any token (this is the pre-auth mint call).
func mintTokenErr(resp *http.Response) error {
	base := statusErr("mint installation token", resp.StatusCode)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(raw) == 0 {
		return base
	}
	var ghErr struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &ghErr) != nil || ghErr.Message == "" {
		return base
	}
	detail := ghErr.Message
	if len(ghErr.Errors) > 0 {
		e := ghErr.Errors[0]
		if msg := strings.TrimSpace(e.Message); msg != "" {
			detail += ": " + msg
		} else if e.Field != "" || e.Code != "" {
			detail += " (" + strings.TrimSpace(e.Field+" "+e.Code) + ")"
		}
	}
	return fmt.Errorf("%w: %s", base, detail)
}

// isPermissionsNotGranted reports whether a mint error's message matches
// GitHub's "The permissions requested are not granted to this installation"
// 422 body — the permanent permission-mismatch signal (distinct from the
// "repository does not exist" 422). Matched on the already-surfaced message so
// it survives mintTokenErr's message/errors[] extraction.
func isPermissionsNotGranted(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permissions") && strings.Contains(msg, "not granted")
}

// AppClient is a forge.Admin for one GitHub-App installation. It mints +
// caches the installation token (refreshing ≈60s before expiry) and
// delegates the actual REST calls to an AdminClient. Repo listing + identity
// differ from a user token (an installation token can't read /user), so
// those are overridden.
type AppClient struct {
	HTTP           *http.Client
	WebBaseURL     string
	Cfg            AppConfig
	InstallationID int64
	Now            func() time.Time

	mu    sync.Mutex
	token string
	exp   time.Time
	// denied names the permissions the cached token LACKS relative to the
	// full request: rest() re-mints without the optional ones an
	// installation withholds, so the token is healthy for every other call
	// and only a caller that needs one of these would fail — at the write,
	// unless it asked PreflightFor first.
	denied map[string]bool
	// scoped caches the tokens minted for a grant OTHER than the runtime
	// baseline — the board profile, the issue profiles — keyed by the
	// permission set so one call family's grant can never be handed to a
	// differently-scoped call.
	scoped map[string]scopedToken
}

// scopedToken is one cached installation token and when it stops being usable.
type scopedToken struct {
	token string
	exp   time.Time
}

// scopedTokenLeeway is how much of a token's life is left unused, so a long
// pass started just under the wire cannot have its token die mid-way. GitHub
// issues installation tokens for ~1h, so reuse covers many passes.
const scopedTokenLeeway = 5 * time.Minute

// PermissionStatuses is the OPTIONAL commit-status permission the management
// token asks for on top of the runtime baseline — what the merge gate posts
// its verdict with — and the one permission rest() re-mints without when an
// installation withholds it.
const PermissionStatuses = "statuses"

func (a *AppClient) clock() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

func (a *AppClient) apiBase() string { return APIBaseFor(a.WebBaseURL) }

// rest returns an AdminClient backed by a fresh installation token.
func (a *AppClient) rest(ctx context.Context) (*AdminClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == "" || a.clock().After(a.exp.Add(-60*time.Second)) {
		// Least-privilege: pin the management token to iterion's minimal
		// permission set (webhook + metadata + code + PR), never the
		// installation's full grant — plus the OPTIONAL statuses:write the
		// merge gate needs to post its revi/review commit status.
		perms := map[string]string{PermissionStatuses: "write"}
		for k, v := range RuntimeInstallationPermissions() {
			perms[k] = v
		}
		tok, exp, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
			&InstallationTokenOptions{Permissions: perms})
		var denied map[string]bool
		if errors.Is(err, forge.ErrPermissionsNotGranted) {
			// An installation created before the merge gate (or one that
			// declined statuses:write) still works — the gate then advises
			// instead of blocking (SetCommitStatus 403s, non-fatal). Retry with
			// the core baseline so every other capability keeps functioning,
			// and remember what this token cannot do.
			tok, exp, err = MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
				&InstallationTokenOptions{Permissions: RuntimeInstallationPermissions()})
			denied = map[string]bool{PermissionStatuses: true}
		}
		if err != nil {
			return nil, err
		}
		a.token, a.exp, a.denied = tok, exp, denied
	}
	return &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: a.token}, nil
}

// scopedREST returns an AdminClient backed by a token minted for exactly perms,
// cached until it nears expiry. It is the ONE mint-and-cache for every call
// family that must not ride the runtime baseline — the board profile and the
// issue profiles alike.
//
// Minting per CALL is what this replaces: every board method went through its
// own mint, so one reconciliation pass cost a token round trip per project
// read, per item page and per reflected card — each an RS256-signed App JWT
// against an endpoint GitHub rate-limits for abuse — repeated on the binding's
// interval forever, and widening the pass duration the sync lease has to cover.
//
// Caching does not widen the leak window it was avoided for: GitHub's minimum
// token life is ~1h whatever the caller does, so a per-call mint bought a
// shorter blast radius only in theory. What DOES matter is the key: a token is
// only ever served back to a call asking for the same permission set, so the
// org-wide board grant cannot ride an ordinary push.
//
// The mint happens under the same lock as rest()'s, which makes concurrent
// callers wait for one mint instead of racing N.
func (a *AppClient) scopedREST(ctx context.Context, perms map[string]string) (*AdminClient, error) {
	key := permissionSetKey(perms)
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.scoped[key]; ok && a.clock().Before(t.exp.Add(-scopedTokenLeeway)) {
		return &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: t.token}, nil
	}
	tok, exp, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
		&InstallationTokenOptions{Permissions: perms})
	if err != nil {
		if errors.Is(err, forge.ErrPermissionsNotGranted) {
			return nil, a.withheldGrant(ctx, perms, err)
		}
		return nil, err
	}
	if a.scoped == nil {
		a.scoped = map[string]scopedToken{}
	}
	a.scoped[key] = scopedToken{token: tok, exp: exp}
	return &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: tok}, nil
}

// withheldGrant turns a scoped mint GitHub refused for want of a grant into
// the typed refusal naming WHICH grant. GitHub's 422 body names none, so the
// installation's live grant is read (one App-JWT probe, on the failure path
// only) and the requested set is resolved against it: what the installation
// lacks — or holds at a lower level than requested — is named, with the
// installation page where an owner approves it. When the probe cannot say
// (it fails, or reports no permissions), the whole requested set is named
// rather than nothing. The mint sentinel stays reachable through the cause,
// so the refresh worker's classification is unchanged.
func (a *AppClient) withheldGrant(ctx context.Context, perms map[string]string, cause error) error {
	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}
	sort.Strings(names)
	var missing []string
	page := ""
	if inst, perr := InstallationInfo(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock()); perr == nil {
		page = inst.HTMLURL
		if len(inst.Permissions) > 0 {
			for _, name := range names {
				if !grantCovers(inst.Permissions, name, perms[name]) {
					missing = append(missing, name+":"+perms[name])
				}
			}
		}
	}
	if len(missing) == 0 {
		for _, name := range names {
			missing = append(missing, name+":"+perms[name])
		}
	}
	remedy := "approve " + strings.Join(missing, ", ") + " on the GitHub App installation"
	if page != "" {
		remedy += " (" + page + ")"
	}
	remedy += " — an org owner reviews the App's pending permission request; an App that does not " +
		"request it yet adds it under its Permissions & events settings first"
	return &forge.PermissionError{
		Provider: forge.ProviderGitHub, Op: "mint installation token",
		Missing: missing, Remedy: remedy, Cause: cause,
	}
}

// grantCovers reports whether an installation's grant serves a requested
// permission at the requested level: a write needs write (or admin), a read
// is served by any level.
func grantCovers(granted map[string]string, name, level string) bool {
	got, ok := granted[name]
	if !ok {
		return false
	}
	if level == "write" {
		return got == "write" || got == "admin"
	}
	return true
}

// permissionSetKey renders a grant set as a stable string, so two calls asking
// for the same permissions share a token and no others do.
func permissionSetKey(perms map[string]string) string {
	names := make([]string, 0, len(perms))
	for name := range perms {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(perms[name])
		b.WriteByte(';')
	}
	return b.String()
}

// PreflightFor mints (or reuses) the installation token and nothing else,
// then reports whether that token carries every permission in need. A caller
// learns BEFORE acting whether the installation can serve: the client is
// lazy — construction never touches the network — so a mint that fails (a
// grant narrower than the requested set, a rotated App key, a suspended
// installation) otherwise surfaces on the first real call, and a permission
// the installation withholds — rest() re-mints without it — surfaces only
// on the one call that needs it, both past the point where a caller holding
// another credential could still switch. A withheld need is an error
// wrapping forge.ErrPermissionsNotGranted that names the permission. The
// token is cached, so a successful preflight costs the calls that follow
// nothing.
func (a *AppClient) PreflightFor(ctx context.Context, need ...string) error {
	if _, err := a.rest(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range need {
		if a.denied[p] {
			return fmt.Errorf("%w: the installation withholds %s (its token is minted without it)", forge.ErrPermissionsNotGranted, p)
		}
	}
	return nil
}

func (a *AppClient) Provider() forge.Provider { return forge.ProviderGitHub }

// WhoAmI returns the App identity — an installation token can't call /user,
// and the bot posts AS the App, so this is the correct "post as" handle.
func (a *AppClient) WhoAmI(context.Context) (forge.Identity, error) {
	slug := a.Cfg.AppSlug
	if slug == "" {
		slug = "github-app"
	}
	return forge.Identity{Login: slug + "[bot]", ID: strconv.FormatInt(a.Cfg.AppID, 10), Kind: forge.AccountKindInstallation, Namespace: slug}, nil
}

// ListRepos lists the installation's repositories (GET
// /installation/repositories) — an installation token's repo set, not the
// user's. The App was installed with webhook-write permission, so every
// listed repo is admin-capable (a missing permission surfaces as a 403 on
// CreateHook, mapped to insufficient_scope).
func (a *AppClient) ListRepos(ctx context.Context, q forge.RepoQuery) ([]forge.RepoSummary, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []struct {
			FullName      string `json:"full_name"`
			Description   string `json:"description"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
			HTMLURL       string `json:"html_url"`
		} `json:"repositories"`
	}
	code, err := c.do(ctx, http.MethodGet, "/installation/repositories?per_page=100", nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET /installation/repositories", code)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	repos := make([]forge.RepoSummary, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		if needle != "" && !strings.Contains(strings.ToLower(r.FullName), needle) {
			continue
		}
		repos = append(repos, forge.RepoSummary{
			FullName: r.FullName, Description: r.Description, Private: r.Private,
			DefaultBranch: r.DefaultBranch, WebURL: r.HTMLURL, CanAdmin: true,
		})
	}
	return repos, nil
}

func (a *AppClient) GetHook(ctx context.Context, repo, deliveryURL string) (*forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetHook(ctx, repo, deliveryURL)
}

func (a *AppClient) CreateHook(ctx context.Context, repo string, spec forge.HookSpec) (forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return forge.HookHandle{}, err
	}
	return c.CreateHook(ctx, repo, spec)
}

func (a *AppClient) UpdateHook(ctx context.Context, repo, hookID string, spec forge.HookSpec) (forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return forge.HookHandle{}, err
	}
	return c.UpdateHook(ctx, repo, hookID, spec)
}

func (a *AppClient) DeleteHook(ctx context.Context, repo, hookID string) error {
	c, err := a.rest(ctx)
	if err != nil {
		return err
	}
	return c.DeleteHook(ctx, repo, hookID)
}

func (a *AppClient) ListHooks(ctx context.Context, repo string) ([]forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListHooks(ctx, repo)
}

// AppRefresher re-mints the installation token for the connection's managed
// forge_token secret (forge.TokenRefresher). The refreshToken arg is unused
// — a GitHub App re-mints from its private key, not a refresh token.
type AppRefresher struct {
	HTTP *http.Client
	Cfg  AppConfig
	Now  func() time.Time
	// Repos, when set, returns the short repo names (e.g. "api", not
	// "org/api") this connection actually operates on, so the runtime
	// forge_token is scoped to that repo set (least-privilege) instead of the
	// whole installation. A nil slice with a nil error → whole-installation
	// (still minimal permissions). A non-nil error means the set could not be
	// determined; Refresh fails closed rather than minting a broader token.
	// Injected by the server so the refresher stays free of a store dependency.
	Repos func(ctx context.Context, conn forge.Connection) ([]string, error)
}

func (r AppRefresher) Refresh(ctx context.Context, conn forge.Connection, _ string) (forge.RefreshedToken, error) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now()
	}
	// Least-privilege: the runtime forge token carries only iterion's minimal
	// permission set, scoped to the connection's provisioned repos when known.
	opts := &InstallationTokenOptions{Permissions: RuntimePermissionsFor(conn.GrantedPermissions)}
	if r.Repos != nil {
		repos, err := r.Repos(ctx, conn)
		if err != nil {
			// Fail closed: keep the prior (narrower) token by failing the
			// refresh rather than minting a whole-installation token when the
			// provisioned repo set is momentarily unknown.
			return forge.RefreshedToken{}, err
		}
		opts.Repositories = repos
	}
	tok, exp, err := MintInstallationToken(ctx, r.HTTP, APIBaseFor(conn.BaseURL()), r.Cfg, conn.InstallationID, now, opts)
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	RecordRuntimePermissions(conn.InstallationID, opts.Permissions)
	return forge.RefreshedToken{AccessToken: tok, ExpiresAt: exp}, nil
}

var _ forge.Admin = (*AppClient)(nil)
var _ forge.TokenRefresher = AppRefresher{}
