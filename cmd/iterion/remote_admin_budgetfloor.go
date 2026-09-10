package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// remote admin budget-floor — capacity RESERVED for a workload, and the
// per-repository ceilings inside the shared budget. The one budget surface
// that is not a cap: everything else here answers "how far may this go",
// this answers "how much is held for this whatever else runs".

var (
	remoteFloorBot        string
	remoteFloorFiveHour   int
	remoteFloorWeek       int
	remoteFloorUSD        float64
	remoteFloorSlots      int
	remoteFloorNote       string
	remoteFloorRepo       string
	remoteFloorRepoSpends int
	remoteFloorRepoShare  int
	remoteFloorShareOfBot string
)

const budgetFloorPath = "/api/admin/settings/budget-floor"

// fetchFloorPolicy reads the stored policy so `reserve` / `quota` / `rm` can
// edit ONE entry without the operator restating the rest. The PUT replaces
// the document, so the read-modify-write has to happen somewhere; doing it
// here keeps the API honest (a reservation set is read as a whole) and the
// CLI ergonomic. The record's `updated_at` rides along as the CAS token the
// PUT is conditional on — decoded and re-sent, never rewritten here.
func fetchFloorPolicy(cmd *cobra.Command, c *cli.RemoteClient) (budgetfloor.Policy, error) {
	raw, err := c.Call(cmd.Context(), "GET", budgetFloorPath, nil, nil)
	if err != nil {
		return budgetfloor.Policy{}, err
	}
	var envelope struct {
		Stored budgetfloor.Policy `json:"stored"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return budgetfloor.Policy{}, fmt.Errorf("decode the stored policy: %w", err)
		}
	}
	return envelope.Stored, nil
}

func putFloorPolicy(cmd *cobra.Command, c *cli.RemoteClient, p *cli.Printer, pol budgetfloor.Policy) error {
	// Validated locally FIRST: the same refusal the server would give, but
	// naming the flag the operator typed rather than a JSON field.
	if err := pol.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(pol)
	if err != nil {
		return err
	}
	return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", budgetFloorPath, body)
}

// editFloorPolicy applies ONE edit to the current policy and writes it back
// under the CAS token the read carried.
//
// A 409 means another admin wrote between this read and this write, so the
// document in hand no longer knows about their reservation — writing it would
// delete theirs. Re-read and re-apply the SAME edit instead: it is expressed
// as an upsert of one entry, so replaying it onto the fresh document keeps
// both. Exactly once, then the operator hears about it: a retry loop that
// never gives up would hide a genuinely contended policy.
func editFloorPolicy(cmd *cobra.Command, c *cli.RemoteClient, p *cli.Printer, apply func(budgetfloor.Policy) (budgetfloor.Policy, error)) error {
	for attempt := 0; ; attempt++ {
		pol, err := fetchFloorPolicy(cmd, c)
		if err != nil {
			return err
		}
		next, err := apply(pol)
		if err != nil {
			return err
		}
		err = putFloorPolicy(cmd, c, p, next)
		var apiErr *cli.APIError
		if attempt == 0 && errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			continue
		}
		return err
	}
}

var remoteAdminBudgetFloorCmd = &cobra.Command{
	Use:   "budget-floor [reserve|quota|rm]",
	Short: "Reserve capacity for a workload, and cap a repository inside the shared budget",
	Long: `Show the reservations, or change one.

Every other budget dial in iterion is a CEILING. This is the floor: a band
of the shared budget held for a named bot, so a reviewer is not starved by
the campaign bots it shares a subscription with.

  iterion remote admin budget-floor
  iterion remote admin budget-floor reserve --bot review-pr --five-hour 20
  iterion remote admin budget-floor quota --repo owner/repo --monthly-usd 50
  iterion remote admin budget-floor rm --bot review-pr
  iterion remote admin budget-floor rm --repo owner/repo

The DEFAULT axis is --five-hour / --week, points of the provider's own usage
window. On a subscription the provider bills nothing per call, so a dollar
reserve there holds back a figure that is an estimate — the window is what
actually runs out. --monthly-usd and --concurrent-runs are available for the
cases where they ARE the honest answer: a metered key, and responsiveness.

Composition: a workload's ceiling is the deployment cap minus the reserves of
every OTHER workload. With review-pr at 20 and feature-dev at 10 under an 80%
cap, ordinary work stops at 50, review-pr may reach 70, feature-dev 60.

A reservation never lets its holder past the deployment's own caps, and never
creates a cap that was not configured.

` + "`reserve`" + ` and ` + "`quota`" + ` EDIT one entry: an axis you do not name keeps the
value it has, so adding --concurrent-runs to a bot that already holds a window
band keeps that band. Name an axis with 0 to clear it, or ` + "`rm`" + ` for the whole
entry.`,
	Args: cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, budgetFloorPath)
		}
		edit := floorEditFromFlags(cmd, args[0])
		return editFloorPolicy(cmd, c, p, func(pol budgetfloor.Policy) (budgetfloor.Policy, error) {
			return applyFloorEdit(pol, edit)
		})
	}),
}

// floorEdit is one operator command, carrying WHICH axes were named as well as
// their values — the distinction that makes an edit an edit.
//
// Without it, `reserve --bot review-pr --concurrent-runs 2` after
// `reserve --bot review-pr --five-hour 20` writes a reservation whose window
// band is zero: the default axis, the only one that measures what actually
// runs out on a subscription, silently gone and indistinguishable from a
// deliberate clear. So an axis the operator did not type is left as stored,
// and typing `--five-hour 0` is how it is cleared.
type floorEdit struct {
	action string
	named  map[string]bool
}

func floorEditFromFlags(cmd *cobra.Command, action string) floorEdit {
	e := floorEdit{action: action, named: map[string]bool{}}
	cmd.Flags().Visit(func(f *pflag.Flag) { e.named[f.Name] = true })
	return e
}

// applyFloorEdit is the operator's single edit, as a pure function of the
// policy it is applied to — which is what makes replaying it onto a freshly
// read document (after a lost CAS race) the same edit and not a different one.
func applyFloorEdit(pol budgetfloor.Policy, e floorEdit) (budgetfloor.Policy, error) {
	switch e.action {
	case "reserve":
		bot := strings.TrimSpace(remoteFloorBot)
		if bot == "" {
			return pol, fmt.Errorf("--bot is required (the workload the reservation protects)")
		}
		// Start from what is stored: this is an edit of one reservation, not
		// a fresh statement of it.
		res, _ := pol.Reserved(bot)
		res.BotID = bot
		if e.named["note"] {
			res.Note = strings.TrimSpace(remoteFloorNote)
		}
		if e.named["five-hour"] {
			res.Reserve.FiveHourPercent = remoteFloorFiveHour
		}
		if e.named["week"] {
			res.Reserve.WeekPercent = remoteFloorWeek
		}
		if e.named["monthly-usd"] {
			res.Reserve.MonthlyUSD = remoteFloorUSD
		}
		if e.named["concurrent-runs"] {
			res.Reserve.ConcurrentRuns = remoteFloorSlots
		}
		if res.Reserve.Empty() {
			return pol, fmt.Errorf("reserve nothing? name at least one axis: --five-hour, --week, --monthly-usd or --concurrent-runs (use `rm --bot %s` to remove the reservation)", bot)
		}
		pol.Reservations = upsertReservation(pol.Reservations, res)
	case "quota":
		repo := strings.TrimSpace(remoteFloorRepo)
		if repo == "" {
			return pol, fmt.Errorf("--repo is required (the forge slug, e.g. owner/repo)")
		}
		q := findRepoQuota(pol.RepoQuotas, repo)
		q.Repo = repo
		if e.named["monthly-usd"] {
			q.MonthlyUSD = remoteFloorUSD
		}
		if e.named["route-spends-per-month"] {
			q.RouteSpendsPerMonth = remoteFloorRepoSpends
		}
		if e.named["reserve-share"] {
			q.ReserveSharePercent = remoteFloorRepoShare
		}
		if e.named["share-of-bot"] {
			q.ShareOfBot = strings.TrimSpace(remoteFloorShareOfBot)
		}
		if q.Empty() {
			return pol, fmt.Errorf("cap nothing? name at least one of --monthly-usd, --route-spends-per-month or --reserve-share (use `rm --repo %s` to remove the quota)", repo)
		}
		pol.RepoQuotas = upsertRepoQuota(pol.RepoQuotas, q)
	case "rm":
		bot, repo := strings.TrimSpace(remoteFloorBot), strings.TrimSpace(remoteFloorRepo)
		if bot == "" && repo == "" {
			return pol, fmt.Errorf("name what to remove: --bot <id> or --repo <slug>")
		}
		if bot != "" {
			pol.Reservations = dropReservation(pol.Reservations, bot)
		}
		if repo != "" {
			pol.RepoQuotas = dropRepoQuota(pol.RepoQuotas, repo)
		}
	default:
		return pol, fmt.Errorf("unknown budget-floor action %q (want reserve|quota|rm)", e.action)
	}
	return pol, nil
}

// findRepoQuota returns the stored quota for a repository, or the zero value.
// Trimmed on BOTH sides, like every lookup in pkg/budgetfloor: an id stored
// with stray whitespace must not read as a different repository.
func findRepoQuota(in []budgetfloor.RepoQuota, repo string) budgetfloor.RepoQuota {
	for _, q := range in {
		if strings.TrimSpace(q.Repo) == repo {
			return q
		}
	}
	return budgetfloor.RepoQuota{}
}

// upsertReservation replaces the entry for a bot, or appends it — so
// `reserve` twice for one bot is an edit, not the duplicate Validate refuses.
func upsertReservation(in []budgetfloor.Reservation, res budgetfloor.Reservation) []budgetfloor.Reservation {
	for i := range in {
		if strings.TrimSpace(in[i].BotID) == res.BotID {
			in[i] = res
			return in
		}
	}
	return append(in, res)
}

func upsertRepoQuota(in []budgetfloor.RepoQuota, q budgetfloor.RepoQuota) []budgetfloor.RepoQuota {
	for i := range in {
		if strings.TrimSpace(in[i].Repo) == q.Repo {
			in[i] = q
			return in
		}
	}
	return append(in, q)
}

func dropReservation(in []budgetfloor.Reservation, bot string) []budgetfloor.Reservation {
	out := in[:0]
	for _, r := range in {
		// Trimmed like every other lookup: `rm --bot review-pr` must remove an
		// entry stored as " review-pr " rather than silently no-op on it.
		if strings.TrimSpace(r.BotID) != bot {
			out = append(out, r)
		}
	}
	return out
}

func dropRepoQuota(in []budgetfloor.RepoQuota, repo string) []budgetfloor.RepoQuota {
	out := in[:0]
	for _, q := range in {
		if strings.TrimSpace(q.Repo) != repo {
			out = append(out, q)
		}
	}
	return out
}

func init() {
	f := remoteAdminBudgetFloorCmd.Flags()
	f.StringVar(&remoteFloorBot, "bot", "", "The workload a reservation protects (bot id)")
	f.IntVar(&remoteFloorFiveHour, "five-hour", 0, "Points of the provider's 5h window held for this bot (the default axis)")
	f.IntVar(&remoteFloorWeek, "week", 0, "Points of the provider's weekly window held for this bot")
	f.Float64Var(&remoteFloorUSD, "monthly-usd", 0, "Dollars of the monthly cost cap held for this bot (or, with `quota`, the repository's ceiling)")
	f.IntVar(&remoteFloorSlots, "concurrent-runs", 0, "Concurrency slots held for this bot")
	f.StringVar(&remoteFloorNote, "note", "", "Why this reservation exists, shown wherever it is cited")
	f.StringVar(&remoteFloorRepo, "repo", "", "Forge slug of the repository to cap (owner/repo)")
	f.IntVar(&remoteFloorRepoSpends, "route-spends-per-month", 0,
		"Cap the repository on metered ACTIVITY instead of amount: one unit per (credential, backend, model) route a run charges, so a two-model run counts twice")
	f.IntVar(&remoteFloorRepoShare, "reserve-share", 0, "Cap the repository at N% of a reservation (needs --share-of-bot)")
	f.StringVar(&remoteFloorShareOfBot, "share-of-bot", "", "Which reservation --reserve-share slices")
	remoteAdminCmd.AddCommand(remoteAdminBudgetFloorCmd)
}
