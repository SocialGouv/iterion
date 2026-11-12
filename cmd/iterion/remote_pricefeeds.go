package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/spf13/cobra"
)

// `iterion remote price-feeds` — the gateway price feed's control surface
// (ADR-163 §5): list, replace, clear and MANUALLY SYNC the feeds, the
// last answering what changed per id; declare one feed's table
// (remote_pricefeeds_declare.go) and read a team's effective table
// (remote_pricefeeds_effective.go). Scope is a team by default (the
// switched/active one, or --team), an org with --org, or the platform
// with --platform; the writes need the matching admin role server-side
// (super-admin for the platform).

var (
	remotePriceFeedsPlatformFlag bool
	remotePriceFeedsOrgFlag      string
	remotePriceFeedsTeamFlag     string
	remotePriceFeedsFile         string
)

// priceFeedParamSummaryLimit is the longest param value `list` prints
// whole; a longer one (a declaration's document) prints as its size.
const priceFeedParamSummaryLimit = 120

var remotePriceFeedsCmd = &cobra.Command{
	Use:   "price-feeds",
	Short: "Manage the gateway price feeds (ADR-163)",
	Long: `Manage the gateway price feeds: operator pricing and accepted reasoning
efforts that live in the database (platform, org and team levels, the
more specific level answering per id and per field) and ride the run
bundle, the runner's env tables staying the fallback. Sources are
pluggable adapters — modelsdev, scaleway, litellm, exec (a command
printing the feed-entry JSON on stdout) and declared (the table itself,
stored in the feed's params: see declare). A fetched feed answers while
FRESH; a stale or failing one steps down with a warning, never a
refusal. A declaration never ages.

The feed list wraps each source in a feed config:

    [{"name":"scaleway","type":"scaleway","enabled":true,
      "interval":3600,"timeout":30,
      "params":{"url":"https://models.dev/api.json"}}]

interval and timeout are seconds. --platform addresses the deployment's
own list (super-admin), --org an org's, --team (or the switched team)
a team's.`,
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
			p.KV(f.Feed.Name, fmt.Sprintf("%s %s enabled=%v interval=%ds timeout=%ds", f.Feed.Type, f.Status, f.Feed.Enabled, secs(f.Feed.Interval), secs(f.Feed.Timeout)))
			if len(f.Feed.Params) > 0 {
				p.KV("  params", priceFeedParamsSummary(f.Feed.Params))
			}
			if len(f.RedactedParams) > 0 {
				p.KV("  redacted", strings.Join(f.RedactedParams, ", "))
			}
			if f.DeclaredAt != nil {
				p.KV("  declared", fmt.Sprintf("%s — %d entries", f.DeclaredAt.Format("2006-01-02 15:04:05"), f.Entries))
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

// priceFeedsBase resolves the scoped base path: /api/admin/price-feeds
// with --platform, /api/orgs/{id}/price-feeds with --org,
// /api/teams/{id}/price-feeds otherwise (the switched/active team, or
// --team). The flags are mutually exclusive (addPriceFeedsScopeFlags).
func priceFeedsBase(cmd *cobra.Command, c *cli.RemoteClient) (string, error) {
	if remotePriceFeedsPlatformFlag {
		return "/api/admin/price-feeds", nil
	}
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

// priceEntrySummary renders one entry's every field pricefeed.SpecEqual
// compares — a sync's or a declaration's per-id change reads as a change
// whichever field moved — prices at their full precision, an unset field
// left out.
func priceEntrySummary(s *modelspecs.Spec) string {
	if s == nil {
		return "(absent)"
	}
	parts := []string{"unpriced"}
	if s.InputCostPerM != 0 || s.OutputCostPerM != 0 {
		parts[0] = fmt.Sprintf("$%s/$%s/mtok", priceText(s.InputCostPerM), priceText(s.OutputCostPerM))
	}
	if s.CacheReadCostPerM != 0 {
		parts = append(parts, fmt.Sprintf("cache $%s/mtok", priceText(s.CacheReadCostPerM)))
	}
	if s.ContextWindow != 0 {
		parts = append(parts, fmt.Sprintf("window %d", s.ContextWindow))
	}
	if s.MaxOutputTokens != 0 {
		parts = append(parts, fmt.Sprintf("max output %d", s.MaxOutputTokens))
	}
	for _, flag := range []struct {
		name  string
		value *bool
	}{{"reasoning", s.Reasoning}, {"tool_call", s.ToolCall}, {"temperature", s.Temperature}} {
		if flag.value != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", flag.name, *flag.value))
		}
	}
	if len(s.ReasoningEfforts) > 0 {
		parts = append(parts, "efforts "+strings.Join(s.ReasoningEfforts, ","))
	}
	return strings.Join(parts, " ")
}

// priceText renders a USD-per-mtok price in its shortest exact form.
func priceText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// priceFeedParamsSummary renders a feed's params in key order, a value
// over priceFeedParamSummaryLimit bytes as its size.
func priceFeedParamsSummary(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kv := make([]string, 0, len(keys))
	for _, k := range keys {
		v := params[k]
		if len(v) > priceFeedParamSummaryLimit {
			v = fmt.Sprintf("(%d bytes)", len(v))
		}
		kv = append(kv, k+"="+v)
	}
	return strings.Join(kv, " ")
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
	Feed           pricefeed.FeedConfig `json:"feed"`
	RedactedParams []string             `json:"redacted_params,omitempty"`
	Status         string               `json:"status"`
	Entries        int                  `json:"entries,omitempty"`
	RefreshedAt    *time.Time           `json:"refreshed_at,omitempty"`
	DeclaredAt     *time.Time           `json:"declared_at,omitempty"`
	FreshFor       pricefeed.Duration   `json:"fresh_for,omitempty"`
	LastError      string               `json:"last_error,omitempty"`
}

type priceFeedsCLISync struct {
	Results []pricefeed.SyncOutcome `json:"results"`
}

// addPriceFeedsScopeFlags gives cmd the three scope flags priceFeedsBase
// reads — one scope per command: two of them refuse before any request.
func addPriceFeedsScopeFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&remotePriceFeedsPlatformFlag, "platform", false, "The platform's own feeds (super-admin)")
	cmd.Flags().StringVar(&remotePriceFeedsOrgFlag, "org", "", "Org id (org-level feeds)")
	cmd.Flags().StringVar(&remotePriceFeedsTeamFlag, "team", "", "Team id (default: switched/active team)")
	cmd.MarkFlagsMutuallyExclusive("platform", "org", "team")
}

func init() {
	remotePriceFeedsSetCmd.Flags().StringVar(&remotePriceFeedsFile, "file", "", "Path to the feeds JSON list (or - for stdin)")
	remotePriceFeedsCmd.AddCommand(remotePriceFeedsListCmd, remotePriceFeedsSetCmd, remotePriceFeedsDeleteCmd, remotePriceFeedsSyncCmd)
	for _, sub := range []*cobra.Command{remotePriceFeedsListCmd, remotePriceFeedsSetCmd, remotePriceFeedsDeleteCmd, remotePriceFeedsSyncCmd} {
		addPriceFeedsScopeFlags(sub)
	}
	remoteCmd.AddCommand(remotePriceFeedsCmd)
}
