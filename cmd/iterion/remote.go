package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	remoteToken    string
	remoteEmail    string
	remotePassword string
	remoteAPIData  string
)

var remoteLoginCmd = &cobra.Command{
	Use:   "login <url>",
	Short: "Log in to a remote instance (browser by default; --token/--email for headless)",
	Long: "Log in to a remote iterion instance and store a token under ~/.iterion.\n\n" +
		"By default it opens your browser to authorize (you approve in the studio).\n" +
		"For headless use: --token <iap_…> (a personal access token), or --email <e>\n" +
		"--password <p> (mints a CLI token for you).",
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
	if token == "" {
		token = os.Getenv("ITERION_TOKEN")
	}
	switch {
	case token != "":
		// direct PAT, validated below
	case remoteEmail != "":
		pw := remotePassword
		if pw == "" {
			pw = os.Getenv("ITERION_PASSWORD")
		}
		if pw == "" {
			return fmt.Errorf("--email requires --password or ITERION_PASSWORD")
		}
		minted, err := cli.NewRemoteClientFor(cfg).LoginWithPassword(cmd.Context(), remoteEmail, pw, cliTokenName())
		if err != nil {
			return err
		}
		token, cfg.Email = minted, remoteEmail
	default:
		minted, err := loginViaBrowser(cmd.Context(), base)
		if err != nil {
			return err
		}
		token = minted
	}

	cfg.Token = token
	client := cli.NewRemoteClientFor(cfg)
	code, body, err := client.API(cmd.Context(), "GET", "/api/auth/me", nil)
	if err != nil {
		return fmt.Errorf("reach %s: %w", base, err)
	}
	if code == 401 || code == 403 {
		return fmt.Errorf("the token was rejected by %s (HTTP %d)", base, code)
	}
	if code/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", base, code)
	}
	if err := cli.SaveRemoteConfig(cfg); err != nil {
		return err
	}
	var me struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	_ = json.Unmarshal(body, &me)
	p, _ := cli.RemoteConfigPath()
	fmt.Printf("Logged in to %s as %s\n(token saved to %s)\n", base, me.User.Email, p)
	return nil
}

// loginViaBrowser runs the loopback browser flow: start a local server, open the
// instance's /cli-auth page (where the signed-in user approves and a token is
// minted), and wait for it to redirect back to the loopback with the token.
func loginViaBrowser(ctx context.Context, base string) (string, error) {
	state := randHex(16)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("start local listener: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	tokenCh := make(chan string, 1)
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
		tokenCh <- tok
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	authURL := base + "/cli-auth?redirect_uri=" + url.QueryEscape(redirectURI) + "&state=" + state
	fmt.Println("Opening your browser to authorize the iterion CLI…")
	fmt.Println("If it doesn't open, visit:\n  " + authURL)
	_ = openBrowser(authURL)

	select {
	case tok := <-tokenCh:
		return tok, nil
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return "", fmt.Errorf("timed out waiting for browser authorization")
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
			return fmt.Errorf("HTTP %d", code)
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
		fmt.Printf("Instance: %s\nAccount:  %s%s\n", client.BaseURL(), me.User.Email, role)
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
		var body []byte
		if remoteAPIData != "" {
			if strings.HasPrefix(remoteAPIData, "@") {
				b, err := os.ReadFile(remoteAPIData[1:])
				if err != nil {
					return err
				}
				body = b
			} else {
				body = []byte(remoteAPIData)
			}
		}
		code, resp, err := client.API(cmd.Context(), strings.ToUpper(args[0]), args[1], body)
		if err != nil {
			return err
		}
		return printJSONResponse(code, resp)
	},
}

// printJSONResponse pretty-prints a JSON response body (falling back to the raw
// bytes when it isn't valid JSON) and maps a non-2xx status to an error. Shared
// by `remote api` and `remote openapi`.
func printJSONResponse(code int, body []byte) error {
	out := body
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil && pretty.Len() > 0 {
		out = pretty.Bytes()
	}
	fmt.Fprintln(os.Stdout, string(out))
	if code/100 != 2 {
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
}

var remoteOpenAPICmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the instance's live OpenAPI 3 spec (GET /api/openapi.json)",
	Long: "Fetch the instance's auto-generated OpenAPI 3 document — the single\n" +
		"source of truth for its API surface, generated from the live routing\n" +
		"table (zero drift). Pipe it to a generator to build a typed client.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return remoteGetJSON(cmd.Context(), "/api/openapi.json")
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
		code, body, err := client.API(cmd.Context(), "GET", "/api/routes", nil)
		if err != nil {
			return err
		}
		if code/100 != 2 {
			return fmt.Errorf("HTTP %d", code)
		}
		var rr struct {
			Routes []struct {
				Method  string `json:"method"`
				Pattern string `json:"pattern"`
			} `json:"routes"`
		}
		if err := json.Unmarshal(body, &rr); err != nil {
			return err
		}
		for _, r := range rr.Routes {
			m := r.Method
			if m == "" {
				m = "ANY"
			}
			fmt.Printf("%-6s %s\n", m, r.Pattern)
		}
		return nil
	},
}

// remoteGetJSON fetches a path and pretty-prints the JSON response.
func remoteGetJSON(ctx context.Context, path string) error {
	client, err := cli.NewRemoteClient()
	if err != nil {
		return err
	}
	code, body, err := client.API(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	return printJSONResponse(code, body)
}

func init() {
	remoteLoginCmd.Flags().StringVar(&remoteToken, "token", "", "Personal access token (iap_…) — or set ITERION_TOKEN")
	remoteLoginCmd.Flags().StringVar(&remoteEmail, "email", "", "Email for headless password login (mints a CLI token)")
	remoteLoginCmd.Flags().StringVar(&remotePassword, "password", "", "Password (or ITERION_PASSWORD); used with --email")
	remoteAPICmd.Flags().StringVar(&remoteAPIData, "data", "", "Request body JSON (literal, or @file)")
	remoteCmd.AddCommand(remoteLoginCmd, remoteLogoutCmd, remoteStatusCmd, remoteAPICmd, remoteOpenAPICmd, remoteRoutesCmd)
	rootCmd.AddCommand(remoteCmd)
}

func htmlMsg(msg string) string {
	return "<!doctype html><meta charset=utf-8><body style=\"font-family:system-ui;padding:3rem;text-align:center\"><h2>" + msg + "</h2>"
}

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
