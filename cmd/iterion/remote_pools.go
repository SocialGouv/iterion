package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// `iterion remote pools` — the key pools' operator surface (ADR-164):
// create pools, push entries (the bulk import sealing a local agent's
// subscription blob), attach/detach a pool at a cascade level, reset an
// entry's parked state, and run one signal-sync pass inline. Scopes:
// yours by default, --org <id>, --platform for the instance pool; the
// writes need the matching role server-side (owner / org admin /
// super-admin).

var (
	remotePoolsOrgFlag      string
	remotePoolsTeamFlag     string
	remotePoolsPlatformFlag bool
	remotePoolsPoolFlag     string
	remotePoolsNameFlag     string
	remotePoolsVisFlag      string
	remotePoolsEntryFlag    string
	remotePoolsSourceFlag   string
	remotePoolsRefFlag      string
	remotePoolsKeyIDFlag    string
	remotePoolsModelFlag    string
	remotePoolsPriorityFlag int
	remotePoolsFileFlag     string
	remotePoolsModeFlag     string
	remotePoolsMarginFlag   float64
	remotePoolsMarginSet    bool
	remotePoolsTriggersFlag string
	remotePoolsHoldFlag     bool
	remotePoolsRunsFlag     int
	remotePoolsUSDFlag      float64
)

var remotePoolsCmd = &cobra.Command{
	Use:   "pools",
	Short: "Manage the key pools (ADR-164)",
	Long: `Manage the key pools: named pools of provider credentials (subscription
forfaits and gateway keys) that a scope attaches over a default, with a
fallback strategy that switches entries when a forfait is exhausted — or
is about to be. An attachment is an explicit opt-in; detaching restores
the inheritance; Enabled=false pauses without deleting.`,
}

func poolsBase(cmd *cobra.Command, c *cli.RemoteClient) string {
	switch {
	case remotePoolsPlatformFlag:
		return "/api/admin/llm/pools"
	case remotePoolsOrgFlag != "":
		org, _ := c.ResolveOrg(cmd.Context(), remotePoolsOrgFlag)
		return "/api/orgs/" + org + "/pools"
	default:
		return "/api/me/pools"
	}
}

func poolsAttachmentBase(cmd *cobra.Command, c *cli.RemoteClient) (string, string, error) {
	switch {
	case remotePoolsPlatformFlag:
		return "/api/admin/llm/pool-attachment", "platform", nil
	case remotePoolsOrgFlag != "":
		org, err := c.ResolveOrg(cmd.Context(), remotePoolsOrgFlag)
		if err != nil {
			return "", "", err
		}
		return "/api/orgs/" + org + "/pool-attachment", "org:" + org, nil
	default:
		team, err := c.ResolveTeam(cmd.Context(), remotePoolsTeamFlag)
		if err != nil {
			return "", "", err
		}
		return "/api/teams/" + team + "/pool-attachment", "team:" + team, nil
	}
}

var remotePoolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the scope's pools",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		var out []poolsCLIView
		if _, err := c.Call(cmd.Context(), "GET", poolsBase(cmd, c), nil, &out); err != nil {
			return err
		}
		p.Header("Key pools")
		if len(out) == 0 {
			p.Line("(none)")
		}
		for _, pool := range out {
			p.KV(pool.ID, fmt.Sprintf("%s owner=%s/%s visibility=%s", poolLabel(pool), pool.OwnerType, pool.OwnerID, pool.Visibility))
			if pool.Name != "" {
				p.KV("  name", pool.Name)
			}
		}
		return nil
	}),
}

func poolLabel(v poolsCLIView) string {
	if !v.Enabled {
		return "disabled"
	}
	return "enabled"
}

var remotePoolsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a pool in the flag-chosen scope",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := map[string]any{"name": remotePoolsNameFlag, "enabled": true}
		if remotePoolsVisFlag != "" {
			body["visibility"] = remotePoolsVisFlag
		}
		var out poolsCLIView
		if _, err := c.Call(cmd.Context(), "POST", poolsBase(cmd, c), body, &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("pool %s created (%s)", out.ID, out.Visibility))
		return nil
	}),
}

var remotePoolsEntryListCmd = &cobra.Command{
	Use:   "entries",
	Short: "List a pool's entries with their derived status",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remotePoolsPoolFlag == "" {
			return fmt.Errorf("--pool is required")
		}
		var out []poolsEntryCLIView
		if _, err := c.Call(cmd.Context(), "GET", "/api/pools/"+remotePoolsPoolFlag+"/entries", nil, &out); err != nil {
			return err
		}
		p.Header(fmt.Sprintf("Entries of %s", remotePoolsPoolFlag))
		for _, e := range out {
			p.KV(e.ID, fmt.Sprintf("%s/%s %s prio=%d", e.Source, e.Ref, e.Status, e.Priority))
			if e.Model != "" {
				p.KV("  model", e.Model)
			}
			if e.CooldownUntil != nil {
				p.KV("  cooldown", e.CooldownUntil.Format("2006-01-02 15:04:05"))
			}
			if e.HealthDetail != "" {
				p.KV("  detail", e.HealthDetail)
			}
		}
		return nil
	}),
}

var remotePoolsEntryPutCmd = &cobra.Command{
	Use:   "entry",
	Short: "Put one entry (a pledge of a credential the pool's lender holds)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remotePoolsPoolFlag == "" || remotePoolsSourceFlag == "" || remotePoolsRefFlag == "" {
			return fmt.Errorf("--pool, --source and --ref are required")
		}
		body := map[string]any{"source": remotePoolsSourceFlag, "ref": remotePoolsRefFlag}
		if remotePoolsKeyIDFlag != "" {
			body["key_id"] = remotePoolsKeyIDFlag
		}
		if remotePoolsModelFlag != "" {
			body["model"] = remotePoolsModelFlag
		}
		if remotePoolsPriorityFlag != 0 {
			body["priority"] = remotePoolsPriorityFlag
		}
		var out poolsEntryCLIView
		if _, err := c.Call(cmd.Context(), "PUT", "/api/pools/"+remotePoolsPoolFlag+"/entries", body, &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("entry %s (%s/%s %s)", out.ID, out.Source, out.Ref, out.Status))
		return nil
	}),
}

var remotePoolsImportCmd = &cobra.Command{
	Use:   "import --pool <id> --kind <claude_code|codex> --file <credentials.json>",
	Short: "Bulk-push a local agent's subscription blob as a pool entry",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remotePoolsPoolFlag == "" || remotePoolsRefFlag == "" || remotePoolsFileFlag == "" {
			return fmt.Errorf("--pool, --kind and --file are required")
		}
		blob, err := os.ReadFile(remotePoolsFileFlag)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		body := map[string]any{"source": remotePoolsRefFlag, "credentials_json": string(blob)}
		if remotePoolsModelFlag != "" {
			body["model"] = remotePoolsModelFlag
		}
		if remotePoolsPriorityFlag != 0 {
			body["priority"] = remotePoolsPriorityFlag
		}
		var out poolsEntryCLIView
		if _, err := c.Call(cmd.Context(), "POST", "/api/pools/"+remotePoolsPoolFlag+"/entries/import", body, &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("imported %s (%s/%s %s)", out.ID, out.Source, out.Ref, out.Status))
		return nil
	}),
}

var remotePoolsResetCmd = &cobra.Command{
	Use:   "reset --pool <id> --entry <pledge-id>",
	Short: "Clear an entry's cooldown and forget its stale readings",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remotePoolsPoolFlag == "" || remotePoolsEntryFlag == "" {
			return fmt.Errorf("--pool and --entry are required")
		}
		var out poolsEntryCLIView
		if _, err := c.Call(cmd.Context(), "POST", "/api/pools/"+remotePoolsPoolFlag+"/entries/"+remotePoolsEntryFlag+"/reset", map[string]any{}, &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("entry %s reset (%s)", out.ID, out.Status))
		return nil
	}),
}

// poolsAttachmentBody builds the CAS-less PUT body from the flags.
func poolsAttachmentBody() map[string]any {
	body := map[string]any{"pool_id": remotePoolsPoolFlag, "enabled": true}
	if remotePoolsModeFlag != "" {
		body["mode"] = remotePoolsModeFlag
	}
	if remotePoolsMarginSet {
		body["margin_pct"] = remotePoolsMarginFlag
	}
	if remotePoolsTriggersFlag != "" {
		body["triggers"] = strings.Split(remotePoolsTriggersFlag, ",")
	}
	if remotePoolsHoldFlag {
		body["reset_hold"] = true
	}
	if remotePoolsRunsFlag > 0 {
		body["max_runs_per_day"] = remotePoolsRunsFlag
	}
	if remotePoolsUSDFlag > 0 {
		body["max_usd_per_day"] = remotePoolsUSDFlag
	}
	return body
}

var remotePoolsAttachCmd = &cobra.Command{
	Use:   "attach --pool <id>",
	Short: "Attach a pool at the flag-chosen level (org / team / instance)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, scope, err := poolsAttachmentBase(cmd, c)
		if err != nil {
			return err
		}
		if remotePoolsPoolFlag == "" {
			return fmt.Errorf("--pool is required")
		}
		var out poolsAttachmentCLIView
		if _, err := c.Call(cmd.Context(), "PUT", base, poolsAttachmentBody(), &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("attached pool %s at %s (mode=%s margin=%v)", out.PoolID, scope, out.Strategy.Mode, strategyMarginOf(out.Strategy)))
		return nil
	}),
}

var remotePoolsDetachCmd = &cobra.Command{
	Use:   "detach",
	Short: "Detach the level's attachment (the level inherits again)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, scope, err := poolsAttachmentBase(cmd, c)
		if err != nil {
			return err
		}
		if _, err := c.Call(cmd.Context(), "DELETE", base, map[string]any{}, nil); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("detached %s — the level inherits", scope))
		return nil
	}),
}

var remotePoolsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Run one signal-sync pass inline (the onboarding proof)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		var out map[string]string
		if _, err := c.Call(cmd.Context(), "POST", "/api/admin/llm/pools/sync", map[string]any{}, &out); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("sync: %s (%s)", out["status"], out["verdict"]))
		return nil
	}),
}

type poolsCLIView struct {
	ID         string `json:"id"`
	OrgID      string `json:"org_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Enabled    bool   `json:"enabled"`
	OwnerType  string `json:"owner_type"`
	OwnerID    string `json:"owner_id,omitempty"`
	Visibility string `json:"visibility"`
}

type poolsEntryCLIView struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	Ref           string     `json:"ref"`
	Model         string     `json:"model,omitempty"`
	Priority      int        `json:"priority,omitempty"`
	Status        string     `json:"status"`
	Health        string     `json:"health"`
	HealthDetail  string     `json:"health_detail,omitempty"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
}

type poolsAttachmentCLIView struct {
	ScopeID  string            `json:"scope_id"`
	PoolID   string            `json:"pool_id"`
	Enabled  bool              `json:"enabled"`
	Strategy poolsStrategyView `json:"strategy"`
}

type poolsStrategyView struct {
	Mode      string   `json:"mode,omitempty"`
	MarginPct *float64 `json:"margin_pct,omitempty"`
	Triggers  []string `json:"triggers"`
}

func strategyMarginOf(s poolsStrategyView) float64 {
	if s.MarginPct == nil {
		return 85
	}
	return *s.MarginPct
}

func init() {
	remotePoolsCreateCmd.Flags().StringVar(&remotePoolsNameFlag, "name", "", "Pool label")
	remotePoolsCreateCmd.Flags().StringVar(&remotePoolsVisFlag, "visibility", "", "private (default for your own) or audience")
	remotePoolsEntryPutCmd.Flags().StringVar(&remotePoolsSourceFlag, "source", "", "oauth | api_key")
	remotePoolsEntryPutCmd.Flags().StringVar(&remotePoolsRefFlag, "ref", "", "the kind (claude_code, codex) or provider (anthropic, openai_compatible...)")
	remotePoolsEntryPutCmd.Flags().StringVar(&remotePoolsKeyIDFlag, "key-id", "", "the lent key's id (api_key entries)")
	remotePoolsEntryPutCmd.Flags().StringVar(&remotePoolsModelFlag, "model", "", "the entry's verbatim model id (REQUIRED for gateway entries)")
	remotePoolsEntryPutCmd.Flags().IntVar(&remotePoolsPriorityFlag, "priority", 0, "priority mode order (lower first; forfaits before metered keys at equal priority)")
	remotePoolsImportCmd.Flags().StringVar(&remotePoolsRefFlag, "kind", "", "claude_code | codex")
	remotePoolsImportCmd.Flags().StringVar(&remotePoolsFileFlag, "file", "", "Path to the credentials blob (e.g. ~/.claude/.credentials.json)")
	remotePoolsImportCmd.Flags().StringVar(&remotePoolsModelFlag, "model", "", "optional model override")
	remotePoolsImportCmd.Flags().IntVar(&remotePoolsPriorityFlag, "priority", 0, "optional priority")
	remotePoolsResetCmd.Flags().StringVar(&remotePoolsEntryFlag, "entry", "", "The entry's pledge id")
	remotePoolsAttachCmd.Flags().StringVar(&remotePoolsModeFlag, "mode", "priority", "priority | least_consumed")
	remotePoolsAttachCmd.Flags().Float64Var(&remotePoolsMarginFlag, "margin", 85, "Pre-exhaustion threshold in percent")
	remotePoolsAttachCmd.Flags().BoolVar(&remotePoolsMarginSet, "margin-set", false, "Carry the --margin value (omit to keep the server default)")
	remotePoolsAttachCmd.Flags().StringVar(&remotePoolsTriggersFlag, "triggers", "", "Comma-separated trigger subset (default usage_window)")
	remotePoolsAttachCmd.Flags().BoolVar(&remotePoolsHoldFlag, "reset-hold", false, "Stay switched until a manual reset")
	remotePoolsAttachCmd.Flags().IntVar(&remotePoolsRunsFlag, "max-runs-per-day", 0, "Borrower draw cap (0 = uncapped)")
	remotePoolsAttachCmd.Flags().Float64Var(&remotePoolsUSDFlag, "max-usd-per-day", 0, "Borrower draw cap (0 = uncapped)")
	for _, sub := range []*cobra.Command{remotePoolsListCmd, remotePoolsCreateCmd, remotePoolsAttachCmd, remotePoolsDetachCmd} {
		sub.Flags().StringVar(&remotePoolsOrgFlag, "org", "", "Org id (org-level scope)")
		sub.Flags().StringVar(&remotePoolsTeamFlag, "team", "", "Team id (default: switched/active team)")
		sub.Flags().BoolVar(&remotePoolsPlatformFlag, "platform", false, "Instance-level scope (super-admin)")
	}
	for _, sub := range []*cobra.Command{remotePoolsEntryListCmd, remotePoolsEntryPutCmd, remotePoolsImportCmd, remotePoolsResetCmd, remotePoolsAttachCmd, remotePoolsDetachCmd} {
		sub.Flags().StringVar(&remotePoolsPoolFlag, "pool", "", "The pool's id")
	}
	remotePoolsCmd.AddCommand(remotePoolsListCmd, remotePoolsCreateCmd, remotePoolsEntryListCmd, remotePoolsEntryPutCmd, remotePoolsImportCmd, remotePoolsResetCmd, remotePoolsAttachCmd, remotePoolsDetachCmd, remotePoolsSyncCmd)
	remoteCmd.AddCommand(remotePoolsCmd)
}
