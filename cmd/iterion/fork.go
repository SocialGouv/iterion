package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

var forkOpts struct {
	runID         string
	nodeID        string
	turnIndex     int
	rewindCode    bool
	forkName      string
	newInputsFile string
	storeDir      string
}

var forkCmd = &cobra.Command{
	Use:   "fork",
	Short: "Fork a run at a prior LLM turn (rehydrates conversation; optionally rewinds code)",
	Long: `Fork creates a new run that resumes from a prior LLM turn of an existing run.

The new run starts in 'cancelled' status with a synthetic checkpoint anchored
at the chosen (node, turn). Use 'iterion resume --run-id <new-id>' (or the
studio Resume button) to actually execute it.

By default the new worktree inherits the parent's current files. Pass
--rewind-code to git-reset the new worktree to the snapshot captured at
the chosen node boundary (requires per-node snapshots; Phase 2+).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if forkOpts.runID == "" {
			return fmt.Errorf("--run-id is required")
		}
		if forkOpts.nodeID == "" {
			return fmt.Errorf("--node is required")
		}
		ctx := cmd.Context()
		storeRoot := forkOpts.storeDir
		if storeRoot == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve cwd: %w", err)
			}
			storeRoot = store.ResolveStoreDir(cwd, "")
		}
		absStore, err := filepath.Abs(storeRoot)
		if err != nil {
			return fmt.Errorf("resolve store dir: %w", err)
		}
		svc, err := runview.NewService(absStore)
		if err != nil {
			return fmt.Errorf("open service: %w", err)
		}

		var newInputs map[string]interface{}
		if forkOpts.newInputsFile != "" {
			data, err := os.ReadFile(forkOpts.newInputsFile)
			if err != nil {
				return fmt.Errorf("read --new-inputs file: %w", err)
			}
			if err := json.Unmarshal(data, &newInputs); err != nil {
				return fmt.Errorf("decode --new-inputs JSON: %w", err)
			}
		}
		result, err := svc.Fork(ctx, runview.ForkSpec{
			RunID:      forkOpts.runID,
			NodeID:     forkOpts.nodeID,
			TurnIndex:  forkOpts.turnIndex,
			RewindCode: forkOpts.rewindCode,
			ForkName:   forkOpts.forkName,
			NewInputs:  newInputs,
		})
		if err != nil {
			return fmt.Errorf("fork: %w", err)
		}
		p := newPrinter()
		p.JSON(result)
		fmt.Fprintf(os.Stderr, "forked %s → %s (resume with: iterion resume --run-id %s)\n",
			result.ParentRunID, result.NewRunID, result.NewRunID)
		return nil
	},
}

func init() {
	f := forkCmd.Flags()
	f.StringVar(&forkOpts.runID, "run-id", "", "Parent run to fork from")
	f.StringVar(&forkOpts.nodeID, "node", "", "Anchor node id (the node the fork re-executes from)")
	f.IntVar(&forkOpts.turnIndex, "turn", -1, "Turn index within the node (-1 = latest)")
	f.BoolVar(&forkOpts.rewindCode, "rewind-code", false, "Reset the new worktree to the snapshot captured at this node boundary")
	f.StringVar(&forkOpts.forkName, "name", "", "Friendly name for the forked run (default auto-generated)")
	f.StringVar(&forkOpts.newInputsFile, "new-inputs", "", "JSON file with input overrides merged onto the parent's inputs")
	f.StringVar(&forkOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	mustMarkRequired(forkCmd, "run-id")
	mustMarkRequired(forkCmd, "node")
	rootCmd.AddCommand(forkCmd)
}
