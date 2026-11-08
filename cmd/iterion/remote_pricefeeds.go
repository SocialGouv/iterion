package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/spf13/cobra"
)

// `iterion remote price-feeds` — the gateway price feed's tenant surface
// (ADR-163 §5): list, replace, clear and MANUALLY SYNC the feeds, the
// last answering what changed per id. Scope is a team by default (the
// switched/active one, or --team) or an org with --org; the writes need
// the matching admin role server-side.

var (
	remotePriceFeedsOrgFlag  string
	remotePriceFeedsTeamFlag string
	remotePriceFeedsFile     string
)

var remotePriceFeedsCmd = &cobra.Command{
	Use:   "price-feeds",
	Short: "Manage the gateway price feeds (ADR-163)",
	Long: `Manage the gateway price feeds: auto-synced operator pricing that lives
in the database (org/team level) and rides the run bundle, the operator
table staying the floor. Sources are pluggable adapters — modelsdev,
scaleway, litellm, exec (a command printing the operator-table JSON on
stdout). A feed answers before the operator table while FRESH; a stale
or failing one steps down with a warning, never a refusal.

The feed list is the operator-table entry JSON shape per id, wrapped in
a feed config:

    [{"name":"scaleway","type":"scaleway","enabled":true,
      "interval":3600,"timeout":30,
      "params":{"url":"https://models.dev/api.json"}}]

interval and timeout are seconds.`,
}

var remotePriceFeedsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the scope's feeds with their live state",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := priceFeedsBase(cmd, c)
		if err != nil {
			return err
		}
		var view priceFeedsCLIView
		if _, err := c.Call(cmd.Context(), "GET", base, nil, &view); err != nil {
			return err
		}
		p.Header(fmt.Sprintf("Price feeds — %s (%s)", view.Scope, view.Origin))
		if len(view.Feeds) == 0 {
			p.Line("(none)")
		}
		for _, f := range view.Feeds {
			p.KV(f.Feed.Name, fmt.Sprintf("%s enabled=%v interval=%ds timeout=%ds", f.Status, f.Feed.Enabled, secs(f.Feed.Interval), secs(f.Feed.Timeout)))
			if len(f.Feed.Params) > 0 {
				var kv []string
				for k, v := range f.Feed.Params {
					kv = append(kv, k+"="+v)
				}
				p.KV("  params", strings.Join(kv, " "))
			}
			if f.RefreshedAt != nil {
				p.KV("  refreshed", fmt.Sprintf("%s — %d entries, fresh %ds", f.RefreshedAt.Format("2006-01-02 15:04:05"), f.Entries, secs(f.FreshFor)))
			}
			if f.LastError != "" {
				p.KV("  last error", f.LastError)
			}
		}
		p.Blank()
		p.Line(fmt.Sprintf("source types: %s", strings.Join(view.SourceTypes, ", ")))
		return nil
	}),
}

var remotePriceFeedsSetCmd = &cobra.Command{
	Use:   "set --file <feeds.json>",
	Short: "Replace the scope's feed list (JSON file, or - for stdin)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := priceFeedsBase(cmd, c)
		if err != nil {
			return err
		}
		var body []byte
		switch remotePriceFeedsFile {
		case "", "-":
			body, err = io.ReadAll(os.Stdin)
		default:
			body, err = os.ReadFile(remotePriceFeedsFile)
		}
		if err != nil {
			return fmt.Errorf("price-feeds set: %w", err)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return fmt.Errorf("price-feeds set: no feed list read")
		}
		payload, err := json.Marshal(map[string]json.RawMessage{"feeds": json.RawMessage(body)})
		if err != nil {
			return fmt.Errorf("price-feeds set: %w", err)
		}
		var view priceFeedsCLIView
		if _, err := c.Call(cmd.Context(), "PUT", base, json.RawMessage(payload), &view); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("price feeds: %d stored (scope %s)", len(view.Feeds), view.Scope))
		return nil
	}),
}

var remotePriceFeedsDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Clear the scope's feed record",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := priceFeedsBase(cmd, c)
		if err != nil {
			return err
		}
		var view priceFeedsCLIView
		if _, err := c.Call(cmd.Context(), "PUT", base, json.RawMessage(`{"feeds":null}`), &view); err != nil {
			return err
		}
		p.Line(fmt.Sprintf("price feeds: cleared (scope %s)", view.Scope))
		return nil
	}),
}

var remotePriceFeedsSyncCmd = &cobra.Command{
	Use:   "sync [feed]",
	Short: "Force a refresh of one feed (or every enabled feed), reporting what changed",
	Args:  cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := priceFeedsBase(cmd, c)
		if err != nil {
			return err
		}
		payload := json.RawMessage(`{}`)
		if len(args) == 1 {
			raw, err := json.Marshal(map[string]string{"name": args[0]})
			if err != nil {
				return err
			}
			payload = raw
		}
		var out priceFeedsCLISync
		if _, err := c.Call(cmd.Context(), "POST", base+"/sync", payload, &out); err != nil {
			return err
		}
		for _, r := range out.Results {
			p.Header(fmt.Sprintf("feed %s — %s", r.Feed, r.Status))
			if r.Error != "" {
				p.KV("error", r.Error)
			}
			if !r.RefreshedAt.IsZero() {
				p.KV("refreshed", r.RefreshedAt.Format("2006-01-02 15:04:05"))
				p.KV("fresh for", fmt.Sprintf("%ds", secs(r.FreshFor)))
			}
			if len(r.Changes) == 0 {
				p.Line("  no entry changed")
				continue
			}
			for _, ch := range r.Changes {
				p.KV("  "+ch.ID, fmt.Sprintf("%s → %s", priceEntrySummary(ch.Old), priceEntrySummary(ch.New)))
			}
		}
		return nil
	}),
}

// priceFeedsBase resolves the scoped base path: /api/orgs/{id} with
// --org, /api/teams/{id} otherwise (the switched/active team, or --team).
func priceFeedsBase(cmd *cobra.Command, c *cli.RemoteClient) (string, error) {
	if remotePriceFeedsOrgFlag != "" {
		org, err := c.ResolveOrg(cmd.Context(), remotePriceFeedsOrgFlag)
		if err != nil {
			return "", err
		}
		return "/api/orgs/" + org + "/price-feeds", nil
	}
	team, err := c.ResolveTeam(cmd.Context(), remotePriceFeedsTeamFlag)
	if err != nil {
		return "", err
	}
	return "/api/teams/" + team + "/price-feeds", nil
}

func secs(d pricefeed.Duration) int64 {
	return int64(time.Duration(d) / time.Second)
}

func priceEntrySummary(s *modelspecs.Spec) string {
	if s == nil {
		return "(absent)"
	}
	return fmt.Sprintf("$%.4f/$%.4f/mtok", s.InputCostPerM, s.OutputCostPerM)
}

// priceFeedsCLIView mirrors the server's view for the CLI's decode.
type priceFeedsCLIView struct {
	Scope       string             `json:"scope"`
	Origin      string             `json:"origin"`
	Feeds       []priceFeedCLIView `json:"feeds"`
	UpdatedAt   *time.Time         `json:"updated_at,omitempty"`
	UpdatedBy   string             `json:"updated_by,omitempty"`
	SourceTypes []string           `json:"source_types"`
}

type priceFeedCLIView struct {
	Feed        pricefeed.FeedConfig `json:"feed"`
	Status      string               `json:"status"`
	Entries     int                  `json:"entries,omitempty"`
	RefreshedAt *time.Time           `json:"refreshed_at,omitempty"`
	FreshFor    pricefeed.Duration   `json:"fresh_for,omitempty"`
	LastError   string               `json:"last_error,omitempty"`
}

type priceFeedsCLISync struct {
	Results []pricefeed.SyncOutcome `json:"results"`
}

func init() {
	remotePriceFeedsSetCmd.Flags().StringVar(&remotePriceFeedsFile, "file", "", "Path to the feeds JSON list (or - for stdin)")
	remotePriceFeedsCmd.AddCommand(remotePriceFeedsListCmd, remotePriceFeedsSetCmd, remotePriceFeedsDeleteCmd, remotePriceFeedsSyncCmd)
	for _, sub := range []*cobra.Command{remotePriceFeedsListCmd, remotePriceFeedsSetCmd, remotePriceFeedsDeleteCmd, remotePriceFeedsSyncCmd} {
		sub.Flags().StringVar(&remotePriceFeedsOrgFlag, "org", "", "Org id (org-level feeds)")
		sub.Flags().StringVar(&remotePriceFeedsTeamFlag, "team", "", "Team id (default: switched/active team)")
	}
	remoteCmd.AddCommand(remotePriceFeedsCmd)
}
