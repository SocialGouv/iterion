package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var issueStoreDir string

var issueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Manage native kanban issues",
	Long: `Manage iterion's built-in kanban tracker.

Issues live under <store-dir>/dispatcher/. They are independent of any
remote tracker (GitHub, Forgejo, …) and can be dispatched by
` + "`iterion dispatch`" + ` via the native tracker adapter.

Subcommands:
  create   Create a new issue
  list     List issues (with filters)
  show     Show one issue
  move     Move an issue to a new state
  update   Update title/body/labels/priority/assignee/fields
  close    Move an issue to the first terminal state
  board    Show or initialize the kanban board
  import   Import a forge repo's issues into the board
`,
}

// ---------------------------------------------------------------------------
// import
// ---------------------------------------------------------------------------

var issueImportOpts cli.IssueImportOptions
var issueImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import a self-hosted forge repo's issues into the native board",
	Long: `Mirror a forge repo's issues into the native kanban board.

Forge is the source of truth (one-way sync); pull requests are skipped.
Open issues land in the first column, closed ones in the terminal column.
The import is idempotent — re-running upserts existing cards, never
duplicates them (so it doubles as an incremental --since sync).

The forge token is read ONLY from the named environment variable
(--token-env), never passed as a flag value.

With --project, a second pass mirrors a forge PROJECT board onto the
cards the first pass created: its Status becomes the card's column
(Inbox/Planned/In progress/Blocked/Done ↔ inbox/ready/in_progress/
blocked/done) and its Area/Mode/Priority become area:/mode:/prio:
labels. It hydrates cards, never creates them — run the issue import
for each repo the board spans. A card already in a terminal column is
left alone: reopening is an operator gesture, not an import's.

Examples:
  FORGE_TOKEN=... iterion issue import \
    --forge forgejo --repo owner/name \
    --base-url https://forge.example.com --token-env FORGE_TOKEN

  GH_TOKEN=... iterion issue import \
    --forge github --repo SocialGouv/iterion \
    --project SocialGouv/203 --token-env GH_TOKEN`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		issueImportOpts.StoreDir = issueStoreDir
		return cli.RunIssueImport(newPrinter(), issueImportOpts)
	},
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

var issueCreateOpts cli.IssueCreateOptions
var issueCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new issue",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		issueCreateOpts.StoreDir = issueStoreDir
		return cli.RunIssueCreate(newPrinter(), issueCreateOpts)
	},
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

var issueListOpts cli.IssueListOptions
var issueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List issues",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		issueListOpts.StoreDir = issueStoreDir
		return cli.RunIssueList(newPrinter(), issueListOpts)
	},
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

var issueShowCmd = &cobra.Command{
	Use:   "show <id-or-prefix>",
	Short: "Show one issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunIssueShow(newPrinter(), cli.IssueRefOptions{
			IssueCommonOptions: cli.IssueCommonOptions{StoreDir: issueStoreDir},
			IDOrPrefix:         args[0],
		})
	},
}

// ---------------------------------------------------------------------------
// move
// ---------------------------------------------------------------------------

var issueMoveTo string
var issueMoveCmd = &cobra.Command{
	Use:   "move <id-or-prefix>",
	Short: "Move an issue to a new state",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunIssueMove(newPrinter(), cli.IssueMoveOptions{
			IssueCommonOptions: cli.IssueCommonOptions{StoreDir: issueStoreDir},
			IDOrPrefix:         args[0],
			To:                 issueMoveTo,
		})
	},
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

var (
	issueUpdateTitle        string
	issueUpdateBody         string
	issueUpdateLabels       []string
	issueUpdatePriority     int
	issueUpdateAssignee     string
	issueUpdateBlockers     []string
	issueUpdateFields       []string
	issueUpdateClearField   []string
	issueUpdateClearLastRun bool
)

var issueUpdateCmd = &cobra.Command{
	Use:   "update <id-or-prefix>",
	Short: "Update issue fields",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.IssueUpdateOptions{
			IssueCommonOptions: cli.IssueCommonOptions{StoreDir: issueStoreDir},
			IDOrPrefix:         args[0],
			Fields:             issueUpdateFields,
			ClearField:         issueUpdateClearField,
			ClearLastRun:       issueUpdateClearLastRun,
		}
		if cmd.Flags().Changed("title") {
			opts.Title = &issueUpdateTitle
		}
		if cmd.Flags().Changed("body") {
			opts.Body = &issueUpdateBody
		}
		if cmd.Flags().Changed("labels") {
			opts.Labels = &issueUpdateLabels
		}
		if cmd.Flags().Changed("priority") {
			opts.Priority = &issueUpdatePriority
		}
		if cmd.Flags().Changed("assignee") {
			opts.Assignee = &issueUpdateAssignee
		}
		if cmd.Flags().Changed("blockers") {
			opts.Blockers = &issueUpdateBlockers
		}
		return cli.RunIssueUpdate(newPrinter(), opts)
	},
}

// ---------------------------------------------------------------------------
// close
// ---------------------------------------------------------------------------

var issueCloseCmd = &cobra.Command{
	Use:   "close <id-or-prefix>",
	Short: "Close an issue (move to first terminal state)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunIssueClose(newPrinter(), cli.IssueRefOptions{
			IssueCommonOptions: cli.IssueCommonOptions{StoreDir: issueStoreDir},
			IDOrPrefix:         args[0],
		})
	},
}

// ---------------------------------------------------------------------------
// board
// ---------------------------------------------------------------------------

var issueBoardCmd = &cobra.Command{
	Use:   "board",
	Short: "Show or manage the kanban board",
}

var issueBoardShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the current board configuration",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunIssueBoardShow(newPrinter(), cli.IssueCommonOptions{StoreDir: issueStoreDir})
	},
}

var (
	issueBoardInitFrom  string
	issueBoardInitForce bool
)

var issueBoardInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize (or replace) the kanban board",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunIssueBoardInit(newPrinter(), cli.IssueBoardInitOptions{
			IssueCommonOptions: cli.IssueCommonOptions{StoreDir: issueStoreDir},
			From:               issueBoardInitFrom,
			Force:              issueBoardInitForce,
		})
	},
}

func init() {
	issueCmd.PersistentFlags().StringVar(&issueStoreDir, "store-dir", "", "Override the iterion store directory")

	// create
	issueCreateCmd.Flags().StringVar(&issueCreateOpts.Title, "title", "", "Issue title (required)")
	issueCreateCmd.Flags().StringVar(&issueCreateOpts.Body, "body", "", "Issue body / description")
	issueCreateCmd.Flags().StringVar(&issueCreateOpts.State, "state", "", "Initial state (default: first board state)")
	issueCreateCmd.Flags().StringSliceVar(&issueCreateOpts.Labels, "label", nil, "Label (repeatable)")
	issueCreateCmd.Flags().IntVar(&issueCreateOpts.Priority, "priority", 0, "Priority (higher = sooner)")
	issueCreateCmd.Flags().StringVar(&issueCreateOpts.Assignee, "assignee", "", "Assignee")
	issueCreateCmd.Flags().StringSliceVar(&issueCreateOpts.Blockers, "blocker", nil, "Blocker issue ID (repeatable)")
	issueCreateCmd.Flags().StringSliceVar(&issueCreateOpts.Fields, "field", nil, "Custom field as key=value (repeatable)")
	issueCreateCmd.Flags().StringVar(&issueCreateOpts.Bot, "bot", "", "Typed workflow/bot override the dispatcher routes to (e.g. docs-refresh)")
	issueCreateCmd.Flags().StringArrayVar(&issueCreateOpts.BotArgs, "bot-arg", nil, "Typed dispatcher var override as key=value, merged over config vars (repeatable; values kept verbatim incl. commas)")

	// list
	issueListCmd.Flags().StringSliceVar(&issueListOpts.States, "state", nil, "Filter by state (repeatable)")
	issueListCmd.Flags().StringSliceVar(&issueListOpts.Labels, "label", nil, "Filter by label (all must be present, repeatable)")
	issueListCmd.Flags().StringVar(&issueListOpts.Assignee, "assignee", "", "Filter by assignee")
	issueListCmd.Flags().BoolVar(&issueListOpts.OnlyClaim, "claimed", false, "Only claimed issues")
	issueListCmd.Flags().BoolVar(&issueListOpts.OnlyFree, "unclaimed", false, "Only unclaimed issues")

	// move
	issueMoveCmd.Flags().StringVar(&issueMoveTo, "to", "", "Target state (required)")

	// update
	issueUpdateCmd.Flags().StringVar(&issueUpdateTitle, "title", "", "New title")
	issueUpdateCmd.Flags().StringVar(&issueUpdateBody, "body", "", "New body")
	issueUpdateCmd.Flags().StringSliceVar(&issueUpdateLabels, "labels", nil, "Replace labels (comma-separated)")
	issueUpdateCmd.Flags().IntVar(&issueUpdatePriority, "priority", 0, "New priority")
	issueUpdateCmd.Flags().StringVar(&issueUpdateAssignee, "assignee", "", "New assignee")
	issueUpdateCmd.Flags().StringSliceVar(&issueUpdateBlockers, "blockers", nil, "Replace blockers (comma-separated)")
	issueUpdateCmd.Flags().StringSliceVar(&issueUpdateFields, "field", nil, "Set custom field as key=value (repeatable)")
	issueUpdateCmd.Flags().StringSliceVar(&issueUpdateClearField, "clear-field", nil, "Clear a custom field (repeatable)")
	issueUpdateCmd.Flags().BoolVar(&issueUpdateClearLastRun, "clear-last-run", false,
		"Forget the last run: the next dispatch starts fresh instead of resuming it (run history is kept)")

	// board
	issueBoardInitCmd.Flags().StringVar(&issueBoardInitFrom, "from", "", "Load board.json from this file (default: built-in starter board)")
	issueBoardInitCmd.Flags().BoolVar(&issueBoardInitForce, "force", false, "Overwrite existing board without prompt")

	// import
	issueImportCmd.Flags().StringVar(&issueImportOpts.Forge, "forge", "", "Forge provider: github|forgejo|gitlab (required)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.Repo, "repo", "", "Repository as owner/name (required)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.TokenEnv, "token-env", "", "Name of the env var holding the forge token (required; the value is never a flag)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.BaseURL, "base-url", "", "Forge base URL (default: provider SaaS host; required for self-hosted forgejo/gitlab)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.Since, "since", "", "Only import issues updated since this RFC3339 timestamp (default: full re-sync)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.MinAuthorRole, "min-author-role", "", "Minimum repo role (guest|reporter|developer|maintainer|owner) an issue author needs for the card to be stamped triage:auto; below it the card parks as needs:approval (default: developer)")
	issueImportCmd.Flags().StringVar(&issueImportOpts.Project, "project", "", "Also mirror a forge project board as <owner>/<number> (e.g. SocialGouv/203): Status → card column, Area/Mode/Priority → card labels")
	issueImportCmd.Flags().StringVar(&issueImportOpts.ProjectOwnerKind, "project-owner-kind", "", "Namespace the --project board lives under: org (default) or user")

	issueBoardCmd.AddCommand(issueBoardShowCmd)
	issueBoardCmd.AddCommand(issueBoardInitCmd)

	issueCmd.AddCommand(issueCreateCmd, issueListCmd, issueShowCmd, issueMoveCmd, issueUpdateCmd, issueCloseCmd, issueBoardCmd, issueImportCmd)
	rootCmd.AddCommand(issueCmd)
}
