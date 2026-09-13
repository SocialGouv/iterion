package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

var contractsStoreDir string
var contractsExclusiveStore bool
var contractsProofFile string

var contractsCmd = &cobra.Command{Use: "contracts", Short: "Inspect and activate public-contract runtime capabilities"}

func contractsLocalStore() (*store.FilesystemRunStore, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return store.OpenExisting(store.ResolveStoreDir(cwd, contractsStoreDir))
}

var contractsInspectCmd = &cobra.Command{
	Use: "inspect", Short: "Show native-runtime capabilities and current activation", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := contractsLocalStore()
		if err != nil {
			return err
		}
		inspection, err := portsactivation.Inspect(cmd.Context(), s)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(inspection)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "scope: %s\nstore: %s\ncapability: %s\nqueue schema: %d\nactivation: %s\n", inspection.Scope, inspection.StoreIdentity, inspection.CapabilityDigest, inspection.QueueVersion, activationLabel(inspection.Activation))
		return err
	},
}

func activationLabel(a *store.PortActivation) string {
	if a == nil {
		return "off (no record)"
	}
	if !a.Enabled {
		return fmt.Sprintf("off (revision %d)", a.Revision)
	}
	return fmt.Sprintf("on, %s scope, revision %d, expires %s", a.Scope, a.Revision, a.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"))
}

var contractsProbeCmd = &cobra.Command{
	Use: "probe", Short: "Produce a reviewable local activation proof", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := contractsLocalStore()
		if err != nil {
			return err
		}
		proof, err := portsactivation.ProbeLocal(cmd.Context(), s, contractsExclusiveStore)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(proof)
	},
}

var contractsActivateCmd = &cobra.Command{
	Use: "activate", Short: "Activate a recently probed local store", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if contractsProofFile == "" {
			return fmt.Errorf("contracts activate requires --proof FILE")
		}
		raw, err := os.ReadFile(contractsProofFile)
		if err != nil {
			return err
		}
		var proof portsactivation.Proof
		if err := json.Unmarshal(raw, &proof); err != nil {
			return err
		}
		s, err := contractsLocalStore()
		if err != nil {
			return err
		}
		record, err := portsactivation.ActivateLocal(cmd.Context(), s, proof)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(record)
	},
}

var contractsDeactivateCmd = &cobra.Command{
	Use: "deactivate", Short: "Stop new native launches while preserving existing runs", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := contractsLocalStore()
		if err != nil {
			return err
		}
		record, err := portsactivation.Disable(cmd.Context(), s)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(record)
	},
}

func init() {
	contractsCmd.PersistentFlags().StringVar(&contractsStoreDir, "store-dir", "", "Existing local store directory (default: managed store for the working directory)")
	contractsProbeCmd.Flags().BoolVar(&contractsExclusiveStore, "exclusive-store", false, "Attest that no incompatible automation can write this local store")
	contractsActivateCmd.Flags().StringVar(&contractsProofFile, "proof", "", "JSON proof produced by contracts probe")
	contractsCmd.AddCommand(contractsInspectCmd, contractsProbeCmd, contractsActivateCmd, contractsDeactivateCmd)
	rootCmd.AddCommand(contractsCmd)
}
