package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote runs — drive the remote instance's run lifecycle (launch,
// follow, inspect, control) from the CLI. Cobra layer only; behavior
// lives in pkg/cli/remote_runs.go.

var remoteRunsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Launch, follow and control runs on the remote instance",
}

func remoteClient() (*cli.RemoteClient, error) { return cli.NewRemoteClient() }

var (
	remoteRunsListStatus   string
	remoteRunsListWorkflow string
	remoteRunsListRepo     string
	remoteRunsListSince    string
	remoteRunsListLimit    int
)

var remoteRunsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List runs",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsList(cmd.Context(), c, p, cli.RemoteRunsListOptions{
			Status:   remoteRunsListStatus,
			Workflow: remoteRunsListWorkflow,
			Repo:     remoteRunsListRepo,
			Since:    remoteRunsListSince,
			Limit:    remoteRunsListLimit,
		})
	}),
}

var (
	remoteLaunchBot             string
	remoteLaunchVars            []string
	remoteLaunchPreset          string
	remoteLaunchTimeout         string
	remoteLaunchBackend         string
	remoteLaunchCompress        string
	remoteLaunchAutoMemory      string
	remoteLaunchLoopBudgetGuard string
	remoteLaunchPermission      string
	remoteLaunchReviewMode      string
	remoteLaunchMergeInto       string
	remoteLaunchBranch          string
	remoteLaunchMergeStrategy   string
	remoteLaunchAutoMerge       bool
	remoteLaunchAttach          []string
	remoteLaunchModelOverrides  string
	remoteLaunchCallbackURL     string
	remoteLaunchCallbackToken   string
	remoteLaunchFollow          bool
	remoteLaunchInterval        time.Duration
)

var remoteRunsLaunchCmd = &cobra.Command{
	Use:   "launch [file.bot]",
	Short: "Launch a run (uploads the file inline, or --bot <catalog id>)",
	Args:  cobra.MaximumNArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		vars, err := cli.ParseVarFlags(remoteLaunchVars)
		if err != nil {
			return err
		}
		attach, err := cli.ParseAttachFlags(remoteLaunchAttach)
		if err != nil {
			return err
		}
		var overrides []byte
		if remoteLaunchModelOverrides != "" {
			overrides, err = cli.ReadDataArg(remoteLaunchModelOverrides)
			if err != nil {
				return err
			}
		}
		opts := cli.RemoteRunsLaunchOptions{
			BotID:              remoteLaunchBot,
			Vars:               vars,
			Preset:             remoteLaunchPreset,
			Timeout:            remoteLaunchTimeout,
			Backend:            remoteLaunchBackend,
			Compress:           remoteLaunchCompress,
			AutoMemory:         remoteLaunchAutoMemory,
			LoopBudgetGuard:    remoteLaunchLoopBudgetGuard,
			Permission:         remoteLaunchPermission,
			ReviewMode:         remoteLaunchReviewMode,
			MergeInto:          remoteLaunchMergeInto,
			BranchName:         remoteLaunchBranch,
			MergeStrategy:      remoteLaunchMergeStrategy,
			AutoMerge:          remoteLaunchAutoMerge,
			Attach:             attach,
			ModelOverridesJSON: overrides,
			CallbackURL:        remoteLaunchCallbackURL,
			CallbackToken:      remoteLaunchCallbackToken,
			Follow:             remoteLaunchFollow,
			FollowInterval:     remoteLaunchInterval,
		}
		if len(args) == 1 {
			opts.FilePath = args[0]
		}
		return cli.RemoteRunsLaunch(cmd.Context(), c, p, opts)
	}),
}

var remoteRunsGetCmd = &cobra.Command{
	Use:   "get <run-id>",
	Short: "Inspect a run",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsGet(cmd.Context(), c, p, args[0])
	}),
}

var (
	remoteEventsFrom     int64
	remoteEventsFollow   bool
	remoteFollowInterval time.Duration
)

var remoteRunsEventsCmd = &cobra.Command{
	Use:   "events <run-id>",
	Short: "Print run events (--follow tails by seq cursor)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsEvents(cmd.Context(), c, p, args[0], remoteEventsFrom, remoteEventsFollow, remoteFollowInterval)
	}),
}

var remoteRunsFollowCmd = &cobra.Command{
	Use:   "follow <run-id>",
	Short: "Tail a run until it terminates (exit 1 on failure)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsFollow(cmd.Context(), c, p, args[0], remoteFollowInterval)
	}),
}

var remoteRunsLogCmd = &cobra.Command{
	Use:   "log <run-id>",
	Short: "Print the raw run log",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsRaw(cmd.Context(), c, p, args[0], "/log")
	}),
}

var remoteRunsWorkflowCmd = &cobra.Command{
	Use:   "workflow <run-id>",
	Short: "Print the run's compiled workflow",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsRaw(cmd.Context(), c, p, args[0], "/workflow")
	}),
}

var (
	remoteArtifactsNode string
	remoteArtifactsFile string
)

var remoteRunsArtifactsCmd = &cobra.Command{
	Use:   "artifacts <run-id>",
	Short: "List run artifacts (--node for one node, --file for artifact files)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsArtifacts(cmd.Context(), c, p, args[0], remoteArtifactsNode, remoteArtifactsFile)
	}),
}

var (
	remoteFilesDiff    bool
	remoteFilesContent bool
	remoteFilesMode    string
	remoteFilesEdit    string
)

var remoteRunsFilesCmd = &cobra.Command{
	Use:   "files <run-id> [path]",
	Short: "Run workspace files: list, --diff/--content on a path, --edit @local to write",
	Args:  cobra.RangeArgs(1, 2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		opts := cli.RemoteRunsFilesOptions{Diff: remoteFilesDiff, Content: remoteFilesContent, Mode: remoteFilesMode}
		if len(args) == 2 {
			opts.Path = args[1]
		}
		if remoteFilesEdit != "" {
			if !strings.HasPrefix(remoteFilesEdit, "@") {
				return fmt.Errorf("--edit takes @<local-file>")
			}
			opts.EditFile = remoteFilesEdit[1:]
		}
		return cli.RemoteRunsFiles(cmd.Context(), c, p, args[0], opts)
	}),
}

var remoteCommitsDiff bool

var remoteRunsCommitsCmd = &cobra.Command{
	Use:   "commits <run-id> [sha]",
	Short: "List the run's commits (with a sha: detail; --diff: its diff)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		sha := ""
		if len(args) == 2 {
			sha = args[1]
		}
		return cli.RemoteRunsCommits(cmd.Context(), c, p, args[0], sha, remoteCommitsDiff)
	}),
}

var remoteRunsCancelCmd = &cobra.Command{
	Use:   "cancel <run-id>",
	Short: "Cancel a run (checkpoint preserved — resumable)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "cancel", nil)
	}),
}

var remoteRunsPauseCmd = &cobra.Command{
	Use:   "pause <run-id>",
	Short: "Pause a run",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "pause", nil)
	}),
}

var (
	remoteResumeAnswers string
	remoteResumeFile    string
	remoteResumeForce   bool
	remoteResumeTimeout string
)

var remoteRunsResumeCmd = &cobra.Command{
	Use:   "resume <run-id>",
	Short: "Resume a paused/failed run (--answers @file for human answers)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		answers := strings.TrimPrefix(remoteResumeAnswers, "@")
		return cli.RemoteRunsResume(cmd.Context(), c, p, args[0], cli.RemoteRunsResumeOptions{
			AnswersFile: answers,
			FilePath:    remoteResumeFile,
			Force:       remoteResumeForce,
			Timeout:     remoteResumeTimeout,
		})
	}),
}

var (
	remoteForkNode   string
	remoteForkTurn   int
	remoteForkRewind bool
	remoteForkName   string
)

var remoteRunsForkCmd = &cobra.Command{
	Use:   "fork <run-id>",
	Short: "Fork a run at a node (--node required; resume the fork after)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remoteForkNode == "" {
			return fmt.Errorf("--node is required")
		}
		body := map[string]any{"node_id": remoteForkNode}
		if remoteForkTurn > 0 {
			body["turn_index"] = remoteForkTurn
		}
		if remoteForkRewind {
			body["rewind_code"] = true
		}
		if remoteForkName != "" {
			body["fork_name"] = remoteForkName
		}
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "fork", body)
	}),
}

var remoteSendSkills []string

var remoteRunsSendCmd = &cobra.Command{
	Use:   "send <run-id> <message>",
	Short: "Queue a steering message for a live run's next agent turn",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := map[string]any{"text": args[1]}
		if len(remoteSendSkills) > 0 {
			body["skills"] = remoteSendSkills
		}
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "queue-message", body)
	}),
}

var (
	remoteMergeStrategy string
	remoteMergeInto     string
	remoteMergeMessage  string
)

var remoteRunsMergeCmd = &cobra.Command{
	Use:   "merge <run-id>",
	Short: "Merge the run's storage branch (--strategy squash|merge)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := map[string]any{}
		if remoteMergeStrategy != "" {
			body["merge_strategy"] = remoteMergeStrategy
		}
		if remoteMergeInto != "" {
			body["merge_into"] = remoteMergeInto
		}
		if remoteMergeMessage != "" {
			body["commit_message"] = remoteMergeMessage
		}
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "merge", body)
	}),
}

var remoteConflictsData string

var remoteRunsConflictsCmd = &cobra.Command{
	Use:   "conflicts <run-id> [resolve|resolve-with-agent|finalize|abort]",
	Short: "Show or act on the run's merge conflicts",
	Args:  cobra.RangeArgs(1, 2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if len(args) == 1 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/runs/"+args[0]+"/merge/conflicts")
		}
		action := args[1]
		switch action {
		case "resolve", "resolve-with-agent", "finalize", "abort":
		default:
			return fmt.Errorf("unknown conflicts action %q (want resolve|resolve-with-agent|finalize|abort)", action)
		}
		body, err := cli.ReadDataArg(remoteConflictsData)
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/runs/"+args[0]+"/merge/conflicts/"+action, body)
	}),
}

var remoteRunsRenameCmd = &cobra.Command{
	Use:   "rename <run-id> <name>",
	Short: "Rename a run",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteRunsAction(cmd.Context(), c, p, args[0], "rename", map[string]string{"name": args[1]})
	}),
}

var remoteDeleteYes bool

var remoteRunsDeleteCmd = &cobra.Command{
	Use:   "delete <run-id>",
	Short: "Delete a run and its artifacts (asks unless --yes)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if !remoteDeleteYes {
			return fmt.Errorf("refusing to delete run %s without --yes", args[0])
		}
		return cli.RemoteRunsDelete(cmd.Context(), c, p, args[0])
	}),
}

var remotePreviewCostVars []string

var remoteRunsPreviewCostCmd = &cobra.Command{
	Use:   "preview-cost <file.bot>",
	Short: "Estimate a workflow's cost before launching",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		vars, err := cli.ParseVarFlags(remotePreviewCostVars)
		if err != nil {
			return err
		}
		return cli.RemoteRunsPreviewCost(cmd.Context(), c, p, args[0], vars)
	}),
}

var remoteRunsUploadCmd = &cobra.Command{
	Use:   "upload <path>",
	Short: "Stage an attachment upload; prints the upload id",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		id, err := cli.RemoteRunsUploadFile(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		if p.Format == cli.OutputJSON {
			p.JSON(map[string]string{"upload_id": id})
			return nil
		}
		p.Line("%s", id)
		return nil
	}),
}

var remoteRunsStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Cross-run statistics",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/runs/stats")
	}),
}

var remoteRunsReposCmd = &cobra.Command{
	Use:   "repos",
	Short: "Distinct repositories seen across runs",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/runs/repos")
	}),
}

func init() {
	remoteRunsListCmd.Flags().StringVar(&remoteRunsListStatus, "status", "", "Filter by status")
	remoteRunsListCmd.Flags().StringVar(&remoteRunsListWorkflow, "workflow", "", "Filter by workflow name")
	remoteRunsListCmd.Flags().StringVar(&remoteRunsListRepo, "repo", "", "Filter by repository")
	remoteRunsListCmd.Flags().StringVar(&remoteRunsListSince, "since", "", "Only runs created after (RFC3339)")
	remoteRunsListCmd.Flags().IntVar(&remoteRunsListLimit, "limit", 0, "Max results")

	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchBot, "bot", "", "Catalog bot id (alternative to a local file)")
	remoteRunsLaunchCmd.Flags().StringArrayVar(&remoteLaunchVars, "var", nil, "Workflow var key=value (repeatable)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchPreset, "preset", "", "In-source preset name")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchTimeout, "timeout", "", "Run timeout (Go duration, e.g. 30m)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchBackend, "backend", "", "Backend override (claude_code|claw)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchCompress, "compress", "", "Compression override (on|ultra|off)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchAutoMemory, "auto-memory", "", "Auto-memory (MEMORY.md) override (on|off)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchLoopBudgetGuard, "loop-budget-guard", "", "Loop back-edge affordability guard override (on|off): refuse a loop iteration the budget cannot fund so the run exits through its own tail")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchPermission, "permission", "", "Permission gate override (off|ask|deny)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchReviewMode, "review-mode", "", "Review topology (auto|mono|dual)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchMergeInto, "merge-into", "", "Worktree finalization target (current|none|<branch>)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchBranch, "branch", "", "Storage branch name override")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchMergeStrategy, "merge-strategy", "", "Merge strategy (squash|merge)")
	remoteRunsLaunchCmd.Flags().BoolVar(&remoteLaunchAutoMerge, "auto-merge", false, "Merge automatically at end of run")
	remoteRunsLaunchCmd.Flags().StringArrayVar(&remoteLaunchAttach, "attach", nil, "Attachment name=local-path (repeatable)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchModelOverrides, "model-overrides", "", "Model overrides JSON array (literal or @file)")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchCallbackURL, "callback-url", "", "Completion webhook URL")
	remoteRunsLaunchCmd.Flags().StringVar(&remoteLaunchCallbackToken, "callback-token", "", "Token echoed in the completion webhook")
	remoteRunsLaunchCmd.Flags().BoolVar(&remoteLaunchFollow, "follow", false, "Tail the run until it terminates")
	remoteRunsLaunchCmd.Flags().DurationVar(&remoteLaunchInterval, "interval", 2*time.Second, "Follow poll interval")

	remoteRunsEventsCmd.Flags().Int64Var(&remoteEventsFrom, "from", 0, "Start seq cursor")
	remoteRunsEventsCmd.Flags().BoolVar(&remoteEventsFollow, "follow", false, "Keep polling until the run terminates")
	remoteRunsEventsCmd.Flags().DurationVar(&remoteFollowInterval, "interval", 2*time.Second, "Poll interval")
	remoteRunsFollowCmd.Flags().DurationVar(&remoteFollowInterval, "interval", 2*time.Second, "Poll interval")

	remoteRunsArtifactsCmd.Flags().StringVar(&remoteArtifactsNode, "node", "", "Artifacts of one node")
	remoteRunsArtifactsCmd.Flags().StringVar(&remoteArtifactsFile, "file", "", "Artifact file path (tree when empty)")

	remoteRunsFilesCmd.Flags().BoolVar(&remoteFilesDiff, "diff", false, "Show the diff of the given path")
	remoteRunsFilesCmd.Flags().BoolVar(&remoteFilesContent, "content", false, "Show the content of the given path")
	remoteRunsFilesCmd.Flags().StringVar(&remoteFilesMode, "mode", "", "File view mode (uncommitted|branch)")
	remoteRunsFilesCmd.Flags().StringVar(&remoteFilesEdit, "edit", "", "@<local-file> whose bytes replace the given path")

	remoteRunsCommitsCmd.Flags().BoolVar(&remoteCommitsDiff, "diff", false, "Show the commit's diff")

	remoteRunsResumeCmd.Flags().StringVar(&remoteResumeAnswers, "answers", "", "Answers JSON file (@file)")
	remoteRunsResumeCmd.Flags().StringVar(&remoteResumeFile, "file", "", "Push a modified workflow file with the resume")
	remoteRunsResumeCmd.Flags().BoolVar(&remoteResumeForce, "force", false, "Resume even if the workflow source changed")
	remoteRunsResumeCmd.Flags().StringVar(&remoteResumeTimeout, "timeout", "", "New timeout for the resumed run")

	remoteRunsForkCmd.Flags().StringVar(&remoteForkNode, "node", "", "Node id to fork at (required)")
	remoteRunsForkCmd.Flags().IntVar(&remoteForkTurn, "turn", 0, "LLM turn index to rewind to")
	remoteRunsForkCmd.Flags().BoolVar(&remoteForkRewind, "rewind-code", false, "Also rewind the code workspace")
	remoteRunsForkCmd.Flags().StringVar(&remoteForkName, "name", "", "Name for the fork")

	remoteRunsSendCmd.Flags().StringArrayVar(&remoteSendSkills, "skill", nil, "Attach a bundle skill to the message (repeatable)")

	remoteRunsMergeCmd.Flags().StringVar(&remoteMergeStrategy, "strategy", "", "Merge strategy (squash|merge)")
	remoteRunsMergeCmd.Flags().StringVar(&remoteMergeInto, "into", "", "Merge target branch")
	remoteRunsMergeCmd.Flags().StringVar(&remoteMergeMessage, "message", "", "Commit message override")

	remoteRunsConflictsCmd.Flags().StringVar(&remoteConflictsData, "data", "", "Action body JSON (literal or @file)")

	remoteRunsDeleteCmd.Flags().BoolVar(&remoteDeleteYes, "yes", false, "Confirm deletion")

	remoteRunsPreviewCostCmd.Flags().StringArrayVar(&remotePreviewCostVars, "var", nil, "Workflow var key=value (repeatable)")

	remoteRunsCmd.AddCommand(
		remoteRunsListCmd, remoteRunsLaunchCmd, remoteRunsGetCmd, remoteRunsEventsCmd,
		remoteRunsFollowCmd, remoteRunsLogCmd, remoteRunsWorkflowCmd, remoteRunsArtifactsCmd,
		remoteRunsFilesCmd, remoteRunsCommitsCmd, remoteRunsCancelCmd, remoteRunsPauseCmd,
		remoteRunsResumeCmd, remoteRunsForkCmd, remoteRunsSendCmd, remoteRunsMergeCmd,
		remoteRunsConflictsCmd, remoteRunsRenameCmd, remoteRunsDeleteCmd,
		remoteRunsPreviewCostCmd, remoteRunsUploadCmd, remoteRunsStatsCmd, remoteRunsReposCmd,
	)
	remoteCmd.AddCommand(remoteRunsCmd)
}
