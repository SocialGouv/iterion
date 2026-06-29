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
// instance over its HTTP API — turning the local CLI into a thin client (and
// the mechanism a future OpenAPI-generated command set would build on).
var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Authenticate to and drive a remote iterion instance over its API",
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
		out := resp
		var pretty bytes.Buffer
		if json.Indent(&pretty, resp, "", "  ") == nil && pretty.Len() > 0 {
			out = pretty.Bytes()
		}
		fmt.Fprintln(os.Stdout, string(out))
		if code/100 != 2 {
			return fmt.Errorf("HTTP %d", code)
		}
		return nil
	},
}

func init() {
	remoteLoginCmd.Flags().StringVar(&remoteToken, "token", "", "Personal access token (iap_…) — or set ITERION_TOKEN")
	remoteLoginCmd.Flags().StringVar(&remoteEmail, "email", "", "Email for headless password login (mints a CLI token)")
	remoteLoginCmd.Flags().StringVar(&remotePassword, "password", "", "Password (or ITERION_PASSWORD); used with --email")
	remoteAPICmd.Flags().StringVar(&remoteAPIData, "data", "", "Request body JSON (literal, or @file)")
	remoteCmd.AddCommand(remoteLoginCmd, remoteLogoutCmd, remoteStatusCmd, remoteAPICmd)
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
