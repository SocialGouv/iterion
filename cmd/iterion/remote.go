package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote groups the commands that authenticate to and drive a REMOTE iterion
// instance over its HTTP API — turning the local CLI into a full client for
// the cloud surface (typed subcommands per domain in the remote_*.go files;
// `remote api` stays as the raw escape hatch).
var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Authenticate to and drive a remote iterion instance over its API",
	Long: "Authenticate to and drive a remote iterion instance over its HTTP API.\n\n" +
		"The full cloud surface is available as typed subcommands (runs, bots,\n" +
		"issues, teams, secrets, webhooks, admin, …); `remote api` is the raw\n" +
		"escape hatch and `remote routes`/`openapi` enumerate the live surface.\n\n" +
		"CI mode: set ITERION_REMOTE_URL + ITERION_REMOTE_TOKEN (and optionally\n" +
		"ITERION_REMOTE_TEAM / ITERION_REMOTE_ORG) to drive an instance with no\n" +
		"stored config. See docs/cloud-cli.md.",
}

var (
	remoteToken     string
	remoteEmail     string
	remotePassword  string
	remoteLoginTeam string
	remoteAPIData   string
)

var remoteLoginCmd = &cobra.Command{
	Use:   "login <url>",
	Short: "Log in to a remote instance (browser by default; --token/--email for headless)",
	Long: "Log in to a remote iterion instance and store a token under ~/.iterion.\n\n" +
		"By default it opens your browser to authorize (you approve in the studio),\n" +
		"and the token is pinned to the team active in the studio at that moment.\n" +
		"For headless use: --token <iap_…> (a personal access token), or --email <e>\n" +
		"--password <p> (mints a CLI token for you).\n\n" +
		"--team <id> pins the minted token to a team, with --email. A token's team\n" +
		"is fixed when it is minted, so this is how the CLI changes team: log in\n" +
		"again. --team is refused with --token (that token's team is already set)\n" +
		"and with the browser flow (the approval pins the studio's active team).",
	Args: cobra.ExactArgs(1),
	RunE: runRemoteLogin,
}

func runRemoteLogin(cmd *cobra.Command, args []string) error {
	base := strings.TrimRight(args[0], "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	cfg := cli.RemoteConfig{BaseURL: base}

	token := remoteToken
	// selfMinted marks a token this command made exist: only such a token has
	// a server-side row to orphan when the login refuses it, and only it is
	// known to belong to the instance at base.
	selfMinted := false
	team := strings.TrimSpace(remoteLoginTeam)
	if token == "" {
		token = os.Getenv("ITERION_TOKEN")
	}
	switch {
	case token != "":
		// A token already exists, and its team was fixed when it was minted:
		// nothing here can move it, and pretending otherwise would store a
		// credential whose scope is not the one asked for.
		if team != "" {
			return fmt.Errorf("--team cannot be used with --token: a token's team is fixed when it is minted.\n"+
				"Pin a new one instead: `iterion remote login %s --email <you> --team %s`,\n"+
				"or mint it in the studio (Account → Access tokens) and log in with that token", base, team)
		}
		// direct PAT, validated below
	case remoteEmail != "":
		pw := remotePassword
		if pw == "" {
			pw = os.Getenv("ITERION_PASSWORD")
		}
		if pw == "" {
			return fmt.Errorf("--email requires --password or ITERION_PASSWORD")
		}
		// The team the token is PINNED to is what gets persisted: `--team` takes
		// a slug as well as a UUID, and the scope every later command sends is
		// the one the mint pinned, not the argument as typed.
		minted, pinned, err := cli.NewRemoteClientFor(cfg).LoginWithPassword(cmd.Context(), remoteEmail, pw, cliTokenName(), team)
		if err != nil {
			return err
		}
		token, cfg.Email, selfMinted = minted, remoteEmail, true
		cfg.TeamID = pinned
	default:
		if team != "" {
			return fmt.Errorf("--team is not supported by the browser login: the approval pins the token to the team active in the studio.\n"+
				"Switch to team %s in the studio and run `iterion remote login %s`,\n"+
				"or log in headless: `iterion remote login %s --email <you> --team %s`", team, base, base, team)
		}
		minted, err := loginViaBrowser(cmd.Context(), base)
		if err != nil {
			return err
		}
		token, selfMinted = minted, true
	}

	cfg.Token = token
	// refuseMinted is the exit for every failure PAST a mint this command
	// made: a token nothing stores is a credential nobody can use, and for
	// the browser approval it belongs to an account the CLI holds nothing
	// else for — only the token itself can revoke it. A token the operator
	// supplied has no row this command created, so it leaves untouched.
	refuseMinted := func(reason error) error {
		if !selfMinted {
			return reason
		}
		return cli.RefuseSelfMintedToken(cmd.Context(), cli.NewRemoteClientFor(cfg), token, reason)
	}
	answer, err := probeIdentity(cmd.Context(), base, token)
	if err != nil {
		return refuseMinted(fmt.Errorf("reach %s: %w", base, err))
	}
	code := answer.code
	if code == 401 || code == 403 {
		return refuseMinted(fmt.Errorf("the token was rejected by %s (HTTP %d)", base, code))
	}
	me, idErr := answer.identity(base)
	// A token this command minted is known-good — the instance at base just
	// issued it — so an identity it cannot read right now is its failure and
	// not a verdict on the token, which would otherwise be thrown away with
	// its row left behind on the server.
	//
	// A token the operator supplied (--token / ITERION_TOKEN) has no such row
	// to orphan, and this probe is the only check the URL they typed ever
	// gets: only an iterion identity — a 2xx whose JSON names the account —
	// accepts it, so a typo'd host's 404 page, a proxy's 502, a static host
	// answering its index page for every path or an unrelated API never
	// becomes the credential every later `iterion remote` command sends there.
	if idErr != nil && !selfMinted {
		return tokenLoginRefusal(base, code, idErr)
	}
	if err := cli.SaveRemoteConfig(cfg); err != nil {
		return refuseMinted(fmt.Errorf("save the CLI credentials: %w", err))
	}
	p, _ := cli.RemoteConfigPath()
	out := cli.NewPrinter(cli.OutputHuman)
	if idErr != nil {
		if asIdentityRedirect(idErr) != nil {
			// Not "right now": a redirect is where the API is served, and the
			// next attempt meets the same one.
			out.Line("Logged in to %s; the account could not be described: %v", base, idErr)
		} else {
			out.Line("Logged in to %s; the server could not describe the account right now (%v)", base, idErr)
		}
		out.Line("(token saved to %s)", p)
		return nil
	}
	account := me.User.Email
	if account == "" {
		account = me.User.ID
	}
	out.Line("Logged in to %s as %s", base, account)
	out.Line("(token saved to %s)", p)
	return nil
}

// identityProbeTimeout bounds the login's identity probe, as the remote
// client bounds every request.
const identityProbeTimeout = 120 * time.Second

// identityAnswer is what GET /api/auth/me answered the login's probe.
type identityAnswer struct {
	code     int
	body     []byte
	location string
}

// probeIdentity asks base who token belongs to (GET /api/auth/me). It follows
// no redirect: a 3xx — an SSO proxy sending to its sign-in page, a host that
// moved — is the probe's answer, and the token is never sent on to the
// redirect's target.
func probeIdentity(ctx context.Context, base, token string) (identityAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/auth/me", nil)
	if err != nil {
		return identityAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{
		Timeout:       identityProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(req)
	if err != nil {
		return identityAnswer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return identityAnswer{}, fmt.Errorf("read GET /api/auth/me: %w", err)
	}
	return identityAnswer{code: resp.StatusCode, body: body, location: resp.Header.Get("Location")}, nil
}

// identity reads the answer as an iterion identity: a 2xx whose JSON body
// names an account by id. Anything else is the error that says what came
// back instead.
func (a identityAnswer) identity(base string) (cli.RemoteMe, error) {
	var me cli.RemoteMe
	switch {
	case a.code/100 == 3:
		return me, &identityRedirect{code: a.code, base: base, location: a.location}
	case a.code/100 != 2:
		return me, &cli.APIError{Status: a.code, Method: "GET", Path: "/api/auth/me", Body: string(a.body)}
	}
	if err := json.Unmarshal(a.body, &me); err != nil {
		return me, fmt.Errorf("HTTP %d GET /api/auth/me answered no iterion identity (%v)", a.code, err)
	}
	if me.User.ID == "" {
		return me, fmt.Errorf("HTTP %d GET /api/auth/me answered no iterion identity (no user id)", a.code)
	}
	return me, nil
}

// identityRedirect is the answer of a probe whose GET /api/auth/me was
// redirected. An iterion instance answers that route itself, so the redirect
// is a front in the way: the URL the operator typed is not the one that
// serves the API. The message names the target's own base URL, because a
// login pinned to THAT is the one every later command can use — the probe
// follows nothing and the token is not sent on.
//
// A target on the same base (a trailing-slash normaliser, a front rewriting
// the path) names no other instance to try, so it is reported as what it is.
type identityRedirect struct {
	code     int
	base     string
	location string
}

func (e *identityRedirect) Error() string {
	if origin := redirectOrigin(e.base, e.location); origin != "" {
		return fmt.Sprintf("HTTP %d GET /api/auth/me redirects to %q: if that is this iterion instance, log in to %s instead (the token was not sent there)", e.code, e.location, origin)
	}
	return fmt.Sprintf("HTTP %d GET /api/auth/me redirects to %q, not an iterion API answer (the token was not sent there)", e.code, e.location)
}

// asIdentityRedirect extracts an *identityRedirect from err, or nil.
func asIdentityRedirect(err error) *identityRedirect {
	var ir *identityRedirect
	if errors.As(err, &ir) {
		return ir
	}
	return nil
}

// redirectOrigin is the scheme://host of a redirect's target, resolved
// against base, when it names an instance OTHER than base. Empty when the
// target is on base itself (nothing else to log in to) or unparseable.
func redirectOrigin(base, location string) string {
	if location == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	t, err := b.Parse(location)
	if err != nil || t.Scheme == "" || t.Host == "" {
		return ""
	}
	origin := t.Scheme + "://" + t.Host
	if origin == b.Scheme+"://"+b.Host {
		return ""
	}
	return origin
}

// tokenLoginRefusal is the error of a login with a token the operator
// supplied that the probe did not accept. It says that nothing was saved,
// and — for a server error, which a later attempt may not meet — to retry.
func tokenLoginRefusal(base string, code int, idErr error) error {
	if code/100 == 5 {
		return fmt.Errorf("%s: %w — nothing was saved; retry when the server answers", base, idErr)
	}
	return fmt.Errorf("%s: %w — nothing was saved", base, idErr)
}

// loginViaBrowser runs the loopback browser flow: start a local server, open the
// instance's /cli-auth page (where the signed-in user approves and a token is
// minted), and wait for it to redirect back to the loopback with the token.
func loginViaBrowser(ctx context.Context, base string) (string, error) {
	res, err := approveViaBrowser(ctx, base, "")
	return res.token, err
}

// browserApprovalResult is what the /cli-auth page handed the loopback
// listener: the minted token and, for a team-pinned approval, the team the
// page says it pinned it to and the org the server states for that team
// (both empty from an instance whose page does not know ?team=).
type browserApprovalResult struct {
	token  string
	teamID string
	orgID  string
}

// approveViaBrowser is the browser-approval flow of `remote login` and —
// with team non-empty — of `remote teams switch` (#2674): the /cli-auth page
// is opened with ?team=<id>, the approving LIVE SESSION mints the team-pinned
// token (a stored PAT mints nothing since #1668), and the loopback callback
// hands the token back together with the pin the page claims.
//
// It judges NOTHING about that pin: by the time the callback arrives the
// token exists on the server, and only the caller holding it can revoke the
// one it refuses (cli.RemoteTeamsSwitch). A refusal here would abandon a
// live token nobody can see.
func approveViaBrowser(ctx context.Context, base, team string) (browserApprovalResult, error) {
	state := randHex(16)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return browserApprovalResult{}, fmt.Errorf("start local listener: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	resCh := make(chan browserApprovalResult, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch on callback")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := q.Get("error"); e != "" {
			fmt.Fprint(w, htmlMsg("Authorization cancelled — you can close this tab."))
			errCh <- fmt.Errorf("authorization cancelled")
			return
		}
		tok := q.Get("token")
		if tok == "" {
			fmt.Fprint(w, htmlMsg("No token received — you can close this tab."))
			errCh <- fmt.Errorf("callback returned no token")
			return
		}
		fmt.Fprint(w, htmlMsg("iterion CLI authorized ✓ — return to your terminal."))
		resCh <- browserApprovalResult{token: tok, teamID: q.Get("team_id"), orgID: q.Get("org_id")}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	authURL := base + "/cli-auth?redirect_uri=" + url.QueryEscape(redirectURI) + "&state=" + state
	if team != "" {
		authURL += "&team=" + url.QueryEscape(team)
	}
	fmt.Println("Opening your browser to authorize the iterion CLI…")
	fmt.Println("If it doesn't open, visit:\n  " + authURL)
	_ = browserOpener(authURL)

	select {
	case res := <-resCh:
		return res, nil
	case err := <-errCh:
		return browserApprovalResult{}, err
	case <-ctx.Done():
		return browserApprovalResult{}, ctx.Err()
	case <-time.After(5 * time.Minute):
		return browserApprovalResult{}, fmt.Errorf("timed out waiting for browser authorization")
	}
}

// cliTeamApproval is the pkg/cli TeamApproval seam `remote teams switch`
// runs through: the browser approval of the instance the client points at.
func cliTeamApproval(c *cli.RemoteClient) cli.TeamApproval {
	return func(ctx context.Context, teamID string) (cli.TeamApprovalResult, error) {
		res, err := approveViaBrowser(ctx, c.BaseURL(), teamID)
		return cli.TeamApprovalResult{Token: res.token, TeamID: res.teamID, OrgID: res.orgID}, err
	}
}

var remoteLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Forget the stored remote credentials",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := cli.ClearRemoteConfig()
		if err != nil {
			return err
		}
		fmt.Printf("Logged out (%s removed).\n", p)
		return nil
	},
}

var remoteStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the logged-in instance + account",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := cli.NewRemoteClient()
		if err != nil {
			return err
		}
		code, body, err := client.API(cmd.Context(), "GET", "/api/auth/me", nil)
		if err != nil {
			return err
		}
		if code/100 != 2 {
			return &cli.APIError{Status: code, Method: "GET", Path: "/api/auth/me", Body: string(body)}
		}
		var me struct {
			User struct {
				Email        string `json:"email"`
				IsSuperAdmin bool   `json:"is_super_admin"`
			} `json:"user"`
		}
		_ = json.Unmarshal(body, &me)
		role := ""
		if me.User.IsSuperAdmin {
			role = " (super-admin)"
		}
		out := cli.NewPrinter(cli.OutputHuman)
		out.Line("Instance: %s", client.BaseURL())
		out.Line("Account:  %s%s", me.User.Email, role)
		return nil
	},
}

var remoteAPICmd = &cobra.Command{
	Use:   "api <METHOD> <path>",
	Short: "Authenticated request to the instance API (e.g. `api GET /api/admin/orgs`)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := cli.NewRemoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteAPIData)
		if err != nil {
			return err
		}
		return cli.RemoteAPIPrint(cmd.Context(), client, newPrinter(), strings.ToUpper(args[0]), args[1], body)
	},
}

var remoteOpenAPICmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the instance's live OpenAPI 3 spec (GET /api/openapi.json)",
	Long: "Fetch the instance's auto-generated OpenAPI 3 document — the single\n" +
		"source of truth for its API surface, generated from the live routing\n" +
		"table (zero drift). Pipe it to a generator to build a typed client.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := cli.NewRemoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), client, newPrinter(), "/api/openapi.json")
	},
}

var remoteRoutesCmd = &cobra.Command{
	Use:   "routes",
	Short: "List the instance's API routes (method + path)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := cli.NewRemoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteRoutesList(cmd.Context(), client, newPrinter())
	},
}

func init() {
	remoteLoginCmd.Flags().StringVar(&remoteToken, "token", "", "Personal access token (iap_…) — or set ITERION_TOKEN")
	remoteLoginCmd.Flags().StringVar(&remoteEmail, "email", "", "Email for headless password login (mints a CLI token)")
	remoteLoginCmd.Flags().StringVar(&remotePassword, "password", "", "Password (or ITERION_PASSWORD); used with --email")
	remoteLoginCmd.Flags().StringVar(&remoteLoginTeam, "team", "", "Pin the minted token to this team id (with --email)")
	remoteAPICmd.Flags().StringVar(&remoteAPIData, "data", "", "Request body JSON (literal, @file, or @- for stdin)")
	remoteCmd.AddCommand(remoteLoginCmd, remoteLogoutCmd, remoteStatusCmd, remoteAPICmd, remoteOpenAPICmd, remoteRoutesCmd)
	rootCmd.AddCommand(remoteCmd)
}

// htmlMsg renders the one-line page the loopback listener of the browser
// approval serves. msg is escaped at the sink, so a sentence that ever
// carries a value the instance chose cannot run as markup on
// http://127.0.0.1:<port>.
func htmlMsg(msg string) string {
	return "<!doctype html><meta charset=utf-8><body style=\"font-family:system-ui;padding:3rem;text-align:center\"><h2>" + html.EscapeString(msg) + "</h2>"
}

// browserOpener opens the approval URL in the system browser. A var so a
// test drives the loopback itself instead of spawning xdg-open.
var browserOpener = openBrowser

func openBrowser(u string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, u)...).Start()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cliTokenName() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "cli"
	}
	return "iterion-cli (" + h + ")"
}
