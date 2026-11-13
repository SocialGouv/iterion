package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/spf13/cobra"
)

// `iterion remote price-feeds effective` — a team's effective price feed
// (GET /api/teams/{id}/price-feeds/effective): the table a run launched
// now for the team seals, per gateway id the answering entry and efforts,
// each with its feed and level, and every feed that states the id.

var remotePriceFeedsEffectiveModels []string

var remotePriceFeedsEffectiveCmd = &cobra.Command{
	Use:   "effective [--team ID] [--model ID ...]",
	Short: "Show the price-feed table a team's run reads, each answer with its source",
	Long: `Show the team's effective price feed: the table a run launched now for
the team carries — the platform, org and team levels folded, the more
specific level answering per id — and, per gateway id, the entry
(prices, window, output cap, flags, whole from one feed) and the accepted
reasoning efforts a run reads, each named with its feed and level, then
every feed that states the id, and the fold's warnings: one names a price
entry that leaves unstated a field of the entry it shadows.

--model narrows to the named ids (repeatable): the table a run naming them
carries; without it, every id a level states, each answering as a run
naming it reads it. When that listing's table exceeds the run channel's
byte budget, no run carries it whole — a run carries the prices of the
gateway ids it names, then of the ids with accepted efforts and of the
declared ids as the budget allows, and every id's efforts while they fit —
and effective says so: --model shows one, and "only a run naming it" marks
an entry or efforts a run naming no gateway id (a model set by an env
dial) does not carry. A level
the server could not read reads unread — a run launched now is refused
until it reads; the server log names the cause.
The runner's own env tables (OPENAI_COMPATIBLE_MODELS,
OPENAI_COMPATIBLE_REASONING_EFFORTS) answer what no feed states; the server
does not see them. --json prints the server's answer as sent.`,
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		team, err := c.ResolveTeam(cmd.Context(), remotePriceFeedsTeamFlag)
		if err != nil {
			return err
		}
		path := "/api/teams/" + url.PathEscape(team) + "/price-feeds/effective"
		if len(remotePriceFeedsEffectiveModels) > 0 {
			path += "?" + url.Values{"model": remotePriceFeedsEffectiveModels}.Encode()
		}
		var view priceFeedEffectiveCLIView
		raw, err := c.Call(cmd.Context(), "GET", path, nil, &view)
		if err != nil {
			return err
		}
		if p.Format == cli.OutputJSON {
			p.Raw(raw)
			return nil
		}
		printPriceFeedEffective(p, view)
		return nil
	}),
}

// priceFeedEffectiveCLIView mirrors the server's effective view for the
// CLI's decode.
type priceFeedEffectiveCLIView struct {
	TeamID            string                        `json:"team_id"`
	OrgID             string                        `json:"org_id,omitempty"`
	At                time.Time                     `json:"at"`
	Scopes            []priceFeedEffectiveCLIScope  `json:"scopes"`
	Models            []priceFeedEffectiveCLIModel  `json:"models"`
	Table             *pricefeed.Table              `json:"table"`
	OverBudget        *priceFeedEffectiveOverBudget `json:"over_budget,omitempty"`
	Warnings          []string                      `json:"warnings,omitempty"`
	RunnerEnvFallback string                        `json:"runner_env_fallback"`
}

// priceFeedEffectiveOverBudget is the listing's folded table size against
// the run channel's byte budget.
type priceFeedEffectiveOverBudget struct {
	Bytes  int `json:"bytes"`
	Budget int `json:"budget"`
}

type priceFeedEffectiveCLIScope struct {
	Scope  string   `json:"scope"`
	ID     string   `json:"id,omitempty"`
	Feeds  []string `json:"feeds,omitempty"`
	Unread string   `json:"unread,omitempty"`
}

type priceFeedEffectiveCLIModel struct {
	ID               string                             `json:"id"`
	Entry            *pricefeed.Entry                   `json:"entry,omitempty"`
	Efforts          *pricefeed.EffortsEntry            `json:"efforts,omitempty"`
	EntryNamedOnly   bool                               `json:"entry_named_only,omitempty"`
	EffortsNamedOnly bool                               `json:"efforts_named_only,omitempty"`
	Contributors     []priceFeedEffectiveCLIContributor `json:"contributors,omitempty"`
}

type priceFeedEffectiveCLIContributor struct {
	Scope          string           `json:"scope"`
	ScopeID        string           `json:"scope_id,omitempty"`
	Feed           string           `json:"feed"`
	Spec           *modelspecs.Spec `json:"spec,omitempty"`
	Efforts        []string         `json:"efforts,omitempty"`
	AnswersEntry   bool             `json:"answers_entry,omitempty"`
	AnswersEfforts bool             `json:"answers_efforts,omitempty"`
}

// level names a contributor's level: "platform", "org <id>", "team <id>".
func (c priceFeedEffectiveCLIContributor) level() string {
	if c.ScopeID == "" {
		return c.Scope
	}
	return c.Scope + " " + c.ScopeID
}

func printPriceFeedEffective(p *cli.Printer, view priceFeedEffectiveCLIView) {
	title := "Effective price feed — team " + view.TeamID
	if view.OrgID != "" {
		title += " (org " + view.OrgID + ")"
	}
	p.Header(title + " at " + view.At.Format("2006-01-02 15:04:05"))
	for _, s := range view.Scopes {
		label := s.Scope
		if s.ID != "" {
			label += " " + s.ID
		}
		feeds := "(no enabled feed)"
		if len(s.Feeds) > 0 {
			feeds = strings.Join(s.Feeds, ", ")
		}
		if s.Unread != "" {
			feeds += "; unread: " + s.Unread
		}
		p.KV(label, feeds)
	}
	switch {
	case view.OverBudget != nil:
		p.Line("  the folded table is %d bytes, over the run channel's %d: no run seals it whole — a run seals the prices of the gateway ids it names, then of the ids with accepted efforts and of the declared ids as the budget allows; each id below answers as a run naming it reads it, %q marking what a run naming no id (a model set by an env dial) lacks; --model ID shows the table a run naming ID seals",
			view.OverBudget.Bytes, view.OverBudget.Budget, priceFeedNamedOnlyMark)
	case view.Table == nil:
		p.Line("  no feed answers: a run carries no table")
	}
	for _, m := range view.Models {
		p.Header(m.ID)
		p.KV("entry", effectiveEntryText(m))
		p.KV("efforts", effectiveEffortsText(m))
		for _, c := range m.Contributors {
			var parts, answers []string
			if c.Spec != nil {
				parts = append(parts, priceEntrySummary(c.Spec))
			}
			if len(c.Efforts) > 0 {
				parts = append(parts, "efforts "+strings.Join(c.Efforts, ","))
			}
			if c.AnswersEntry {
				answers = append(answers, "the entry")
			}
			if c.AnswersEfforts {
				answers = append(answers, "the efforts")
			}
			text := strings.Join(parts, " ")
			if len(answers) > 0 {
				text += " (answers " + strings.Join(answers, " and ") + ")"
			}
			p.Line("    %s %s: %s", c.level(), c.Feed, text)
		}
	}
	p.Blank()
	for _, w := range view.Warnings {
		p.KV("warning", w)
	}
	p.KV("runner env", view.RunnerEnvFallback+" — the runner's OPENAI_COMPATIBLE_MODELS and OPENAI_COMPATIBLE_REASONING_EFFORTS answer what no feed states")
}

// priceFeedNamedOnlyMark follows an entry or efforts a run naming no
// gateway id does not seal (entry_named_only, efforts_named_only).
const priceFeedNamedOnlyMark = "only a run naming it"

// namedOnly appends the named-only mark to text when set.
func namedOnly(text string, set bool) string {
	if set {
		return text + " — " + priceFeedNamedOnlyMark
	}
	return text
}

// effectiveEntryText renders the answering entry with its feed, level and
// age.
func effectiveEntryText(m priceFeedEffectiveCLIModel) string {
	if m.Entry == nil {
		return "(none) — no feed prices it"
	}
	text := priceEntrySummary(&m.Entry.Spec) + " — feed " + m.Entry.Feed
	for _, c := range m.Contributors {
		if c.AnswersEntry {
			text += " (" + c.level() + ")"
			break
		}
	}
	if m.Entry.Declared {
		return namedOnly(text+", declared", m.EntryNamedOnly)
	}
	return namedOnly(fmt.Sprintf("%s, refreshed %s, fresh for %ds", text, m.Entry.RefreshedAt.Format("2006-01-02 15:04:05"), secs(m.Entry.FreshFor)), m.EntryNamedOnly)
}

// effectiveEffortsText renders the answering efforts with their feed and level.
func effectiveEffortsText(m priceFeedEffectiveCLIModel) string {
	if m.Efforts == nil {
		return "(none) — no feed states them"
	}
	text := strings.Join(m.Efforts.Values, ",") + " — feed " + m.Efforts.Feed
	for _, c := range m.Contributors {
		if c.AnswersEfforts {
			text += " (" + c.level() + ")"
			break
		}
	}
	return namedOnly(text, m.EffortsNamedOnly)
}

func init() {
	remotePriceFeedsEffectiveCmd.Flags().StringVar(&remotePriceFeedsTeamFlag, "team", "", "Team id (default: switched/active team)")
	remotePriceFeedsEffectiveCmd.Flags().StringArrayVar(&remotePriceFeedsEffectiveModels, "model", nil, "A gateway id to report (repeatable; default: every id a level states)")
	remotePriceFeedsCmd.AddCommand(remotePriceFeedsEffectiveCmd)
}
