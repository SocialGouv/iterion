package main

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote admin / sso / plugins — platform administration. Admin
// endpoints are super-admin-gated server-side; the CLI does no
// client-side role check (a 403 surfaces as-is).

var remoteAdminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Platform administration (super-admin)",
}

var remoteAdminData string

var remoteAdminOrgsCmd = &cobra.Command{
	Use:   "orgs [get|create|update|delete|restore|status|teams|usage] [id] [status]",
	Short: "Org console: list (default) or act on one org",
	Args:  cobra.MaximumNArgs(3),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs")
		}
		action := args[0]
		needID := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin orgs %s <org-id>", action)
			}
			return args[1], nil
		}
		switch action {
		case "get":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id)
		case "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/admin/orgs", remoteAdminData, "org JSON")
		case "update":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/orgs/"+id, remoteAdminData, "patch JSON")
		case "delete":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/orgs/"+id, nil)
		case "restore":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/orgs/"+id+"/restore", nil)
		case "status":
			if len(args) != 3 {
				return fmt.Errorf("usage: admin orgs status <org-id> <status>")
			}
			body := []byte(fmt.Sprintf(`{"status":%q}`, args[2]))
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/orgs/"+args[1]+"/status", body)
		case "teams":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id+"/teams")
		case "usage":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/orgs/"+id+"/usage")
		default:
			return fmt.Errorf("unknown orgs action %q", action)
		}
	}),
}

var remoteAdminUsersCmd = &cobra.Command{
	Use:   "users [update <user-id>]",
	Short: "List platform users, or update one (--data)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/users")
		}
		if args[0] != "update" || len(args) != 2 {
			return fmt.Errorf("usage: admin users [update <user-id> --data @f]")
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/users/"+args[1], remoteAdminData, "patch JSON")
	}),
}

var remoteAdminDLQCmd = &cobra.Command{
	Use:   "dlq [show|replay|delete <seq>]",
	Short: "Queue dead-letter entries: list (default) or act on one",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/dlq")
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: admin dlq [show|replay|delete <seq>]")
		}
		switch args[0] {
		case "show":
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/dlq/"+args[1])
		case "replay":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/dlq/"+args[1]+"/replay", nil)
		case "delete":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/dlq/"+args[1], nil)
		default:
			return fmt.Errorf("unknown dlq action %q (want show|replay|delete)", args[0])
		}
	}),
}

// --- platform LLM credentials (DB-backed env fallback) ---

var (
	remoteLLMFromEnv  string
	remoteLLMFromFile string
	remoteLLMProvider string
	remoteLLMName     string
	remoteLLMDefault  bool
	remoteLLMKeyData  string
)

var remoteAdminLLMCmd = &cobra.Command{
	Use:   "llm",
	Short: "Platform LLM credentials — rotate the deployment's provider keys/forfait without a redeploy",
}

var remoteAdminLLMKeysCmd = &cobra.Command{
	Use:   "api-keys [create|rotate|update|delete] [key-id]",
	Short: "Platform provider API keys: list (default) or act on one (values from --from-env/--from-file/stdin)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/llm/api-keys")
		}
		action := args[0]
		needID := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin llm api-keys %s <key-id>", action)
			}
			return args[1], nil
		}
		switch action {
		case "create":
			if len(args) != 1 {
				return fmt.Errorf("usage: admin llm api-keys create --provider <p> --name <n> (value on --from-env/--from-file/stdin); %q is not a positional argument", args[1])
			}
			if remoteLLMProvider == "" || remoteLLMName == "" {
				return fmt.Errorf("--provider and --name are required")
			}
			value, err := cli.ReadSecretValue(remoteLLMFromEnv, remoteLLMFromFile, true)
			if err != nil {
				return err
			}
			return cli.RemoteAPIKeysCreate(cmd.Context(), c, p, "platform", "", remoteLLMProvider, remoteLLMName, value, remoteLLMDefault)
		case "rotate":
			id, err := needID()
			if err != nil {
				return err
			}
			value, err := cli.ReadSecretValue(remoteLLMFromEnv, remoteLLMFromFile, true)
			if err != nil {
				return err
			}
			return cli.RemoteAPIKeysRotate(cmd.Context(), c, p, "platform", "", id, value)
		case "update":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", "/api/admin/llm/api-keys/"+id, remoteLLMKeyData, "patch JSON")
		case "delete":
			id, err := needID()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/llm/api-keys/"+id, nil)
		default:
			return fmt.Errorf("unknown api-keys action %q (want create|rotate|update|delete)", action)
		}
	}),
}

var remoteAdminLLMOAuthCmd = &cobra.Command{
	Use:   "oauth [set|connect|refresh|delete] [kind]",
	Short: "Platform OAuth-forfait: list connections (default) or act on one kind (claude_code|codex)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/llm/oauth/connections")
		}
		action := args[0]
		needKind := func() (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin llm oauth %s <kind>", action)
			}
			return args[1], nil
		}
		switch action {
		case "set":
			// The forfait blob (claude_code credentials.json / codex auth.json).
			kind, err := needKind()
			if err != nil {
				return err
			}
			blob, err := cli.ReadSecretBlob(remoteLLMFromEnv, remoteLLMFromFile)
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/llm/oauth/"+kind+"/credentials", blob)
		case "connect":
			// Browser code flow; only claude_code supports it, so it defaults.
			kind := "claude_code"
			if len(args) == 2 {
				kind = args[1]
			}
			return cli.RemoteAdminLLMOAuthConnect(cmd.Context(), c, p, kind)
		case "refresh":
			kind, err := needKind()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/llm/oauth/"+kind+"/refresh", nil)
		case "delete":
			kind, err := needKind()
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/llm/oauth/"+kind, nil)
		default:
			return fmt.Errorf("unknown oauth action %q (want set|connect|refresh|delete)", action)
		}
	}),
}

// --- platform bot overrides (DB-backed catalog) ---

var (
	remoteAdminBotSlug string
	remoteAdminBotOut  string
)

var remoteAdminBotsCmd = &cobra.Command{
	Use:   "bots [show|push|pull|rm|fork] [slug|dir]",
	Short: "Platform bot overrides: iterate on any bot (incl. natives) without an image rollout",
	Long: `Platform bot overrides — the DB-backed form of the baked bot catalog.
A pushed bundle overrides the same-slug baked bot for EVERY tenant and
every launch surface (studio, webhooks, schedules, board, triggers) from
the next launch; deleting it reverts to the baked catalog.

  iterion remote admin bots                       # list overrides (slug, version, digest)
  iterion remote admin bots push bots/review-pr   # push a local bundle dir
  iterion remote admin bots show review-pr        # stored files + metadata
  iterion remote admin bots pull review-pr --out /tmp/review-pr
  iterion remote admin bots fork review-pr        # seed the override from the baked bundle
  iterion remote admin bots rm review-pr          # revert to the baked catalog

An override is deployment-trusted code (the same trust as the image);
every mutation lands on the platform audit log with a content digest.`,
	Args: cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/bots")
		}
		action := args[0]
		needArg := func(what string) (string, error) {
			if len(args) < 2 {
				return "", fmt.Errorf("usage: admin bots %s <%s>", action, what)
			}
			return args[1], nil
		}
		switch action {
		case "push":
			dir, err := needArg("bundle-dir")
			if err != nil {
				return err
			}
			return cli.RemoteAdminBotsPush(cmd.Context(), c, p, dir, remoteAdminBotSlug)
		case "show":
			slug, err := needArg("slug")
			if err != nil {
				return err
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/bots/"+slug)
		case "pull":
			slug, err := needArg("slug")
			if err != nil {
				return err
			}
			return cli.RemoteAdminBotsPull(cmd.Context(), c, p, slug, remoteAdminBotOut)
		case "rm":
			slug, err := needArg("slug")
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/admin/bots/"+slug, nil)
		case "fork":
			slug, err := needArg("slug")
			if err != nil {
				return err
			}
			body := []byte(fmt.Sprintf(`{"from":%q}`, slug))
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/admin/bots/"+slug+"/fork", body)
		default:
			return fmt.Errorf("unknown bots action %q (want push|show|pull|rm|fork)", action)
		}
	}),
}

// --- platform runtime settings: usage caps (DB-backed env fallback) ---

var (
	remoteCapsFiveHour  int
	remoteCapsWeek      int
	remoteCapsClearFive bool
	remoteCapsClearWeek bool
)

var remoteAdminCapsCmd = &cobra.Command{
	Use:   "caps [set]",
	Short: "Platform usage caps: show the effective values (default) or retune them without a restart",
	Long: `Platform usage caps — the runtime-settings form of
ITERION_USAGE_CAP_5H_PCT / ITERION_USAGE_CAP_WEEK_PCT.

Without arguments, prints the stored record, the env defaults, the
EFFECTIVE policy and its source. "set" writes overrides (merge
semantics: a window you don't name keeps its current state):

  iterion remote admin caps set --five-hour 80 --week 70
  iterion remote admin caps set --clear-week        # back to the env default

Changes propagate to every server and runner replica within the
advertised propagation bound (no restart). Enforcement postures
(soft/hard modes) and the ITERION_USAGE_CAP kill switch stay env-only.`,
	Args: cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		const path = "/api/admin/settings/usage-caps"
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, path)
		}
		if args[0] != "set" {
			return fmt.Errorf("unknown caps action %q (want set)", args[0])
		}
		fields := map[string]string{}
		if cmd.Flags().Changed("five-hour") {
			fields["five_hour_pct"] = fmt.Sprintf("%d", remoteCapsFiveHour)
		}
		if remoteCapsClearFive {
			fields["five_hour_pct"] = "null"
		}
		if cmd.Flags().Changed("week") {
			fields["week_pct"] = fmt.Sprintf("%d", remoteCapsWeek)
		}
		if remoteCapsClearWeek {
			fields["week_pct"] = "null"
		}
		if len(fields) == 0 {
			return fmt.Errorf("usage: admin caps set --five-hour <0-100> and/or --week <0-100> (or --clear-five-hour / --clear-week)")
		}
		body := "{"
		for k, v := range fields {
			if len(body) > 1 {
				body += ","
			}
			body += fmt.Sprintf("%q:%s", k, v)
		}
		body += "}"
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", path, []byte(body))
	}),
}

// --- platform settings: webhook role bots + sandbox image ---

var (
	remoteRoleFlags  = map[string]*string{}
	remoteRoleClears = map[string]*bool{}
)

var remoteAdminRolesCmd = &cobra.Command{
	Use:   "roles [set]",
	Short: "Webhook role→bot bindings: show the effective set (default) or re-point a role without a rollout",
	Long: `Platform bot roles — the runtime-settings form of the hardcoded
webhook role constants (reviewer/revi_converse/brancher/implementer).

  iterion remote admin roles                       # stored + effective + origin
  iterion remote admin roles set --reviewer my-reviewer
  iterion remote admin roles set --clear-reviewer  # back to the built-in default

Changes propagate to every replica within the resolver TTL (no restart).`,
	Args: cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		const path = "/api/admin/settings/bot-roles"
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, path)
		}
		if args[0] != "set" {
			return fmt.Errorf("unknown roles action %q (want set)", args[0])
		}
		fields := map[string]string{}
		for name, v := range remoteRoleFlags {
			if cmd.Flags().Changed(strings.ReplaceAll(name, "_", "-")) {
				fields[name] = fmt.Sprintf("%q", *v)
			}
		}
		for name, cleared := range remoteRoleClears {
			if *cleared {
				fields[name] = "null"
			}
		}
		if len(fields) == 0 {
			return fmt.Errorf("usage: admin roles set --reviewer|--revi-converse|--brancher|--implementer <bot-id> (or the matching --clear-* flag)")
		}
		parts := make([]string, 0, len(fields))
		for k, v := range fields {
			parts = append(parts, fmt.Sprintf("%q:%s", k, v))
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", path, []byte("{"+strings.Join(parts, ",")+"}"))
	}),
}

var (
	remoteSandboxImage      string
	remoteSandboxClearImage bool
)

var remoteAdminSandboxCmd = &cobra.Command{
	Use:   "sandbox [set]",
	Short: "Sandbox runtime settings: show (default) or retune the `sandbox: auto` fallback image without a rollout",
	Long: `Platform sandbox settings — the runtime-settings form of
ITERION_SANDBOX_DEFAULT_IMAGE. The effective image is resolved at PUBLISH
time and pinned on each queued run, so a redelivery reruns in the same
environment. Prefer an @sha256 digest ref in cloud.

  iterion remote admin sandbox
  iterion remote admin sandbox set --default-image ghcr.io/…/iterion-sandbox-slim@sha256:…
  iterion remote admin sandbox set --clear-default-image`,
	Args: cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		const path = "/api/admin/settings/sandbox"
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, path)
		}
		if args[0] != "set" {
			return fmt.Errorf("unknown sandbox action %q (want set)", args[0])
		}
		switch {
		case remoteSandboxClearImage:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", path, []byte(`{"default_image":null}`))
		case cmd.Flags().Changed("default-image"):
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", path, []byte(fmt.Sprintf(`{"default_image":%q}`, remoteSandboxImage)))
		default:
			return fmt.Errorf("usage: admin sandbox set --default-image <ref> (or --clear-default-image)")
		}
	}),
}

// --- org SSO ---

var remoteSSOData string

var remoteSSOCmd = &cobra.Command{
	Use:   "sso",
	Short: "Per-org SSO providers and domains",
}

var remoteSSOProvidersCmd = &cobra.Command{
	Use:   "providers [create|update|delete|test <provider-id>]",
	Short: "Org SSO providers",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/sso/providers"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteSSOData, "provider JSON")
		case args[0] == "update" && len(args) == 2:
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base+"/"+args[1], remoteSSOData, "patch JSON")
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		case args[0] == "test" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/test", nil)
		default:
			return fmt.Errorf("usage: providers [create|update|delete|test <provider-id>]")
		}
	}),
}

var remoteSSODomainsCmd = &cobra.Command{
	Use:   "domains [create|verify <domain-id>|delete <domain-id>]",
	Short: "Org SSO domains",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		org, err := c.ResolveOrg(cmd.Context(), remoteOrgFlag)
		if err != nil {
			return err
		}
		base := "/api/orgs/" + org + "/sso/domains"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteSSOData, "domain JSON")
		case args[0] == "verify" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/verify", nil)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: domains [create|verify <id>|delete <id>]")
		}
	}),
}

// --- plugins ---

var remotePluginsData string

var remotePluginsCmd = &cobra.Command{
	Use:   "plugins [enable|disable|install|uninstall|config <name>]",
	Short: "Plugin management on the remote instance",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/plugins")
		}
		action := args[0]
		switch action {
		case "install":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", "/api/v1/plugins/install", remotePluginsData, "install JSON")
		case "enable", "disable":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins %s <name>", action)
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/plugins/"+args[1]+"/"+action, nil)
		case "uninstall":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins uninstall <name>")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/v1/plugins/"+args[1], nil)
		case "config":
			if len(args) != 2 {
				return fmt.Errorf("usage: plugins config <name> --data @f")
			}
			return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/v1/plugins/"+args[1]+"/config", remotePluginsData, "config JSON")
		default:
			return fmt.Errorf("unknown plugins action %q", action)
		}
	}),
}

func init() {
	remoteAdminOrgsCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")
	remoteAdminUsersCmd.Flags().StringVar(&remoteAdminData, "data", "", "Request body JSON (literal or @file)")

	for _, c := range []*cobra.Command{remoteAdminLLMKeysCmd, remoteAdminLLMOAuthCmd} {
		c.Flags().StringVar(&remoteLLMFromEnv, "from-env", "", "Read the secret value from this environment variable")
		c.Flags().StringVar(&remoteLLMFromFile, "from-file", "", "Read the secret value from this file")
	}
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMProvider, "provider", "", "LLM provider for create (anthropic|openai|…)")
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMName, "name", "", "Key display name for create")
	remoteAdminLLMKeysCmd.Flags().BoolVar(&remoteLLMDefault, "default", false, "Make the created key the provider's default")
	remoteAdminLLMKeysCmd.Flags().StringVar(&remoteLLMKeyData, "data", "", "Patch JSON for update (literal or @file)")
	remoteAdminLLMCmd.AddCommand(remoteAdminLLMKeysCmd, remoteAdminLLMOAuthCmd)

	remoteAdminCapsCmd.Flags().IntVar(&remoteCapsFiveHour, "five-hour", 0, "Five-hour window cap percentage (0–100; 0 = no cap)")
	remoteAdminCapsCmd.Flags().IntVar(&remoteCapsWeek, "week", 0, "Weekly window cap percentage (0–100; 0 = no cap)")
	remoteAdminCapsCmd.Flags().BoolVar(&remoteCapsClearFive, "clear-five-hour", false, "Clear the five-hour override (fall back to the env default)")
	remoteAdminCapsCmd.Flags().BoolVar(&remoteCapsClearWeek, "clear-week", false, "Clear the weekly override (fall back to the env default)")

	remoteAdminBotsCmd.Flags().StringVar(&remoteAdminBotSlug, "slug", "", "Override slug (push; default: bundle dir basename)")
	remoteAdminBotsCmd.Flags().StringVar(&remoteAdminBotOut, "out", "", "Output directory (pull; default: ./<slug>)")

	for _, role := range []string{"reviewer", "revi_converse", "brancher", "implementer"} {
		flag := strings.ReplaceAll(role, "_", "-")
		remoteRoleFlags[role] = remoteAdminRolesCmd.Flags().String(flag, "", "Bot id for the "+role+" role")
		remoteRoleClears[role] = remoteAdminRolesCmd.Flags().Bool("clear-"+flag, false, "Clear the "+role+" override (fall back to the built-in default)")
	}
	remoteAdminSandboxCmd.Flags().StringVar(&remoteSandboxImage, "default-image", "", "`sandbox: auto` fallback image ref (prefer an @sha256 digest)")
	remoteAdminSandboxCmd.Flags().BoolVar(&remoteSandboxClearImage, "clear-default-image", false, "Clear the override (fall back to the env default / built-in)")

	remoteAdminCmd.AddCommand(remoteAdminOrgsCmd, remoteAdminUsersCmd, remoteAdminDLQCmd, remoteAdminLLMCmd, remoteAdminCapsCmd, remoteAdminBotsCmd, remoteAdminRolesCmd, remoteAdminSandboxCmd)

	remoteSSOProvidersCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteSSOData, "data", "", "Request body JSON (literal or @file)")
	remoteSSOProvidersCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSODomainsCmd.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	remoteSSOCmd.AddCommand(remoteSSOProvidersCmd, remoteSSODomainsCmd)

	remotePluginsCmd.Flags().StringVar(&remotePluginsData, "data", "", "Request body JSON (literal or @file)")

	remoteCmd.AddCommand(remoteAdminCmd, remoteSSOCmd, remotePluginsCmd)
}
