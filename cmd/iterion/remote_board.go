package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote issues / labels / board — the native kanban tracker
// (/api/v1/native) plus the board↔forge bridge.

var remoteIssuesCmd = &cobra.Command{
	Use:   "issues",
	Short: "Native board issues on the remote instance",
}

var (
	remoteIssuesStates   []string
	remoteIssuesLabels   []string
	remoteIssuesAssignee string
)

var remoteIssuesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List board issues",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteIssuesList(cmd.Context(), c, p, cli.RemoteIssuesListOptions{
			States: remoteIssuesStates, Labels: remoteIssuesLabels, Assignee: remoteIssuesAssignee,
		})
	}),
}

var remoteIssuesGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show one issue",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/native/issues/"+args[0])
	}),
}

var (
	remoteIssueTitle    string
	remoteIssueBody     string
	remoteIssueState    string
	remoteIssueLabels   []string
	remoteIssuePriority int
	remoteIssueAssignee string
	remoteIssueBot      string
	remoteIssueBotArgs  []string
)

func remoteIssueFieldsFromFlags() (cli.RemoteIssueFields, error) {
	botArgs, err := cli.ParseVarFlags(remoteIssueBotArgs)
	if err != nil {
		return cli.RemoteIssueFields{}, err
	}
	return cli.RemoteIssueFields{
		Title:    remoteIssueTitle,
		Body:     remoteIssueBody,
		State:    remoteIssueState,
		Labels:   remoteIssueLabels,
		Priority: remoteIssuePriority,
		Assignee: remoteIssueAssignee,
		Bot:      remoteIssueBot,
		BotArgs:  botArgs,
	}, nil
}

var remoteIssuesCreateCmd = &cobra.Command{
	Use:   "create <title>",
	Short: "Create an issue",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		f, err := remoteIssueFieldsFromFlags()
		if err != nil {
			return err
		}
		f.Title = args[0]
		return cli.RemoteIssuesCreate(cmd.Context(), c, p, f)
	}),
}

var remoteIssuesUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update an issue (only the flags you pass are changed)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		f, err := remoteIssueFieldsFromFlags()
		if err != nil {
			return err
		}
		set := map[string]bool{}
		for _, name := range []string{"title", "body", "label", "priority", "assignee", "bot", "bot-arg"} {
			if cmd.Flags().Changed(name) {
				set[name] = true
			}
		}
		if len(set) == 0 {
			return fmt.Errorf("nothing to update — pass at least one field flag")
		}
		return cli.RemoteIssuesUpdate(cmd.Context(), c, p, args[0], f, set)
	}),
}

var remoteIssuesDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete an issue",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/v1/native/issues/"+args[0], nil)
	}),
}

var remoteIssuesTransitionCmd = &cobra.Command{
	Use:   "transition <id> <state>",
	Short: "Move an issue to a state",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := fmt.Sprintf(`{"to":%q}`, args[1])
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/native/issues/"+args[0]+"/transition", []byte(body))
	}),
}

var (
	remoteCommentBot          string
	remoteCommentTransitionTo string
)

var remoteIssuesCommentCmd = &cobra.Command{
	Use:   "comment <id> <text>",
	Short: "Comment on an issue (--bot to also dispatch a run)",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := map[string]any{"body": args[1]}
		if remoteCommentBot != "" {
			body["bot"] = remoteCommentBot
		}
		if remoteCommentTransitionTo != "" {
			body["transition_to"] = remoteCommentTransitionTo
		}
		raw, err := c.Call(cmd.Context(), "POST", "/api/v1/native/issues/"+args[0]+"/comments", body, nil)
		if err != nil {
			return err
		}
		cli.PrintRemoteJSON(p, raw)
		return nil
	}),
}

// --- board↔forge bridge ---

var remoteIssuesPushCmd = &cobra.Command{
	Use:   "push <id>",
	Short: "Push the issue to the linked forge",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/native/issues/"+args[0]+"/push", nil)
	}),
}

var remotePullsData string

var remotePullsCmd = &cobra.Command{
	Use:   "pulls <issue-id> [create|ci <number>|merge <number>]",
	Short: "Forge pull requests linked to a board issue",
	Args:  cobra.RangeArgs(1, 3),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base := "/api/v1/native/issues/" + args[0] + "/pulls"
		if len(args) == 1 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		}
		switch args[1] {
		case "create":
			body, err := cli.ReadDataArg(remotePullsData)
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case "ci":
			if len(args) != 3 {
				return fmt.Errorf("usage: pulls <issue-id> ci <number>")
			}
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[2]+"/ci")
		case "merge":
			if len(args) != 3 {
				return fmt.Errorf("usage: pulls <issue-id> merge <number>")
			}
			body, err := cli.ReadDataArg(remotePullsData)
			if err != nil {
				return err
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[2]+"/merge", body)
		default:
			return fmt.Errorf("unknown pulls action %q (want create|ci|merge)", args[1])
		}
	}),
}

// --- labels ---

var remoteLabelsCmd = &cobra.Command{
	Use:   "labels",
	Short: "Board label management",
}

var remoteLabelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List labels",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/native/labels")
	}),
}

var remoteLabelsRenameCmd = &cobra.Command{
	Use:   "rename <from> <to>",
	Short: "Rename a label across all issues",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := fmt.Sprintf(`{"from":%q,"to":%q}`, args[0], args[1])
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/native/labels/rename", []byte(body))
	}),
}

var remoteLabelsMergeCmd = &cobra.Command{
	Use:   "merge <from> <to>",
	Short: "Merge a label into another",
	Args:  cobra.ExactArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		body := fmt.Sprintf(`{"from":%q,"to":%q}`, args[0], args[1])
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/native/labels/merge", []byte(body))
	}),
}

var remoteLabelsDeleteCmd = &cobra.Command{
	Use:   "delete <label>",
	Short: "Delete a label from all issues",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", "/api/v1/native/labels/"+args[0], nil)
	}),
}

// --- board config ---

var remoteBoardCmd = &cobra.Command{
	Use:   "board",
	Short: "Board configuration (states, fields, views)",
}

var remoteBoardGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Show the board configuration",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/native/board")
	}),
}

var remoteBoardSetData string

var remoteBoardSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Replace the board configuration (--data @file)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/v1/native/board", remoteBoardSetData, "board config JSON")
	}),
}

func init() {
	remoteIssuesListCmd.Flags().StringArrayVar(&remoteIssuesStates, "state", nil, "Filter by state (repeatable)")
	remoteIssuesListCmd.Flags().StringArrayVar(&remoteIssuesLabels, "label", nil, "Filter by label (repeatable)")
	remoteIssuesListCmd.Flags().StringVar(&remoteIssuesAssignee, "assignee", "", "Filter by assignee")

	for _, c := range []*cobra.Command{remoteIssuesCreateCmd, remoteIssuesUpdateCmd} {
		c.Flags().StringVar(&remoteIssueTitle, "title", "", "Issue title")
		c.Flags().StringVar(&remoteIssueBody, "body", "", "Issue body")
		c.Flags().StringVar(&remoteIssueState, "state", "", "Issue state")
		c.Flags().StringArrayVar(&remoteIssueLabels, "label", nil, "Label (repeatable)")
		c.Flags().IntVar(&remoteIssuePriority, "priority", 0, "Priority")
		c.Flags().StringVar(&remoteIssueAssignee, "assignee", "", "Assignee")
		c.Flags().StringVar(&remoteIssueBot, "bot", "", "Bot to dispatch for this issue")
		c.Flags().StringArrayVar(&remoteIssueBotArgs, "bot-arg", nil, "Bot arg key=value (repeatable)")
	}

	remoteIssuesCommentCmd.Flags().StringVar(&remoteCommentBot, "bot", "", "Also dispatch this bot")
	remoteIssuesCommentCmd.Flags().StringVar(&remoteCommentTransitionTo, "transition-to", "", "Move the issue after commenting")

	remotePullsCmd.Flags().StringVar(&remotePullsData, "data", "", "Request body JSON for create/merge (literal or @file)")

	remoteIssuesCmd.AddCommand(
		remoteIssuesListCmd, remoteIssuesGetCmd, remoteIssuesCreateCmd, remoteIssuesUpdateCmd,
		remoteIssuesDeleteCmd, remoteIssuesTransitionCmd, remoteIssuesCommentCmd,
		remoteIssuesPushCmd, remotePullsCmd,
	)
	remoteLabelsCmd.AddCommand(remoteLabelsListCmd, remoteLabelsRenameCmd, remoteLabelsMergeCmd, remoteLabelsDeleteCmd)
	remoteBoardCmd.AddCommand(remoteBoardGetCmd, remoteBoardSetCmd)
	remoteBoardSetCmd.Flags().StringVar(&remoteBoardSetData, "data", "", "Board config JSON (literal or @file)")
	remoteCmd.AddCommand(remoteIssuesCmd, remoteLabelsCmd, remoteBoardCmd)
}
