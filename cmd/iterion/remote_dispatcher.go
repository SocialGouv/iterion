package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote dispatcher / triggers — long-running dispatcher control and
// the event-trigger subscription registry.

var remoteDispatcherCmd = &cobra.Command{
	Use:   "dispatcher",
	Short: "Dispatcher control on the remote instance",
}

// dispatcherGET/POST wire the fixed-verb endpoints without one var block each.
func dispatcherSimpleCmd(use, short, method, path string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := remoteClient()
			if err != nil {
				return err
			}
			if method == "GET" {
				return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), path)
			}
			return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), method, path, nil)
		},
	}
}

var remoteDispatcherConfigData string

var remoteDispatcherConfigCmd = &cobra.Command{
	Use:   "config [set]",
	Short: "Show (or with `set --data @file`, replace) the dispatcher config",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/dispatcher/config")
		}
		if args[0] != "set" {
			return fmt.Errorf("unknown config action %q (want set)", args[0])
		}
		body, err := cli.ReadDataArg(remoteDispatcherConfigData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required (dispatcher config JSON)")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", "/api/v1/dispatcher/config", body)
	},
}

var remoteDispatcherIssueCmd = &cobra.Command{
	Use:   "issue <id>",
	Short: "Dispatcher view of one issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/dispatcher/issues/"+args[0])
	},
}

var remoteDispatcherCancelCmd = &cobra.Command{
	Use:   "cancel <issue-id>",
	Short: "Cancel the dispatcher's in-flight run for an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/v1/dispatcher/issues/"+args[0]+"/cancel", nil)
	},
}

// --- triggers ---

var remoteTriggersCmd = &cobra.Command{
	Use:   "triggers",
	Short: "Event-trigger subscriptions on the remote instance",
}

var remoteTriggersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List trigger subscriptions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/triggers")
	},
}

var remoteTriggersGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show a trigger subscription",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/triggers/"+args[0])
	},
}

var remoteTriggersData string

var remoteTriggersCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a trigger subscription (--data @file)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteTriggersData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/v1/triggers", body)
	},
}

var remoteTriggersUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a trigger subscription (--data @file)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteTriggersData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "PUT", "/api/v1/triggers/"+args[0], body)
	},
}

var remoteTriggersDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a trigger subscription",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "DELETE", "/api/v1/triggers/"+args[0], nil)
	},
}

var remoteTriggersEmitCmd = &cobra.Command{
	Use:   "emit",
	Short: "Publish a trigger event (--data @file)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteTriggersData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/v1/triggers/emit", body)
	},
}

func init() {
	remoteDispatcherConfigCmd.Flags().StringVar(&remoteDispatcherConfigData, "data", "", "Dispatcher config JSON (literal or @file)")
	remoteDispatcherCmd.AddCommand(
		dispatcherSimpleCmd("status", "Dispatcher status", "GET", "/api/v1/dispatcher/status"),
		dispatcherSimpleCmd("state", "Dispatcher live state snapshot", "GET", "/api/v1/dispatcher/state"),
		dispatcherSimpleCmd("start", "Start the dispatcher", "POST", "/api/v1/dispatcher/start"),
		dispatcherSimpleCmd("stop", "Stop the dispatcher", "POST", "/api/v1/dispatcher/stop"),
		dispatcherSimpleCmd("pause", "Pause dispatching", "POST", "/api/v1/dispatcher/pause"),
		dispatcherSimpleCmd("resume", "Resume dispatching", "POST", "/api/v1/dispatcher/resume"),
		dispatcherSimpleCmd("refresh", "Trigger an immediate poll", "POST", "/api/v1/dispatcher/refresh"),
		dispatcherSimpleCmd("reload", "Reload the dispatcher config", "POST", "/api/v1/dispatcher/reload"),
		remoteDispatcherConfigCmd, remoteDispatcherIssueCmd, remoteDispatcherCancelCmd,
	)

	for _, c := range []*cobra.Command{remoteTriggersCreateCmd, remoteTriggersUpdateCmd, remoteTriggersEmitCmd} {
		c.Flags().StringVar(&remoteTriggersData, "data", "", "Request body JSON (literal or @file)")
	}
	remoteTriggersCmd.AddCommand(
		remoteTriggersListCmd, remoteTriggersGetCmd, remoteTriggersCreateCmd,
		remoteTriggersUpdateCmd, remoteTriggersDeleteCmd, remoteTriggersEmitCmd,
	)
	remoteCmd.AddCommand(remoteDispatcherCmd, remoteTriggersCmd)
}
