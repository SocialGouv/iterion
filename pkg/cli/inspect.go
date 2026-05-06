package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// InspectSection enumerates the per-node report buckets the caller can
// restrict to via --section. Empty value behaves as SectionAll.
type InspectSection string

const (
	SectionAll          InspectSection = "all"
	SectionSummary      InspectSection = "summary"
	SectionEvents       InspectSection = "events"
	SectionTrace        InspectSection = "trace"
	SectionTools        InspectSection = "tools"
	SectionArtifacts    InspectSection = "artifacts"
	SectionInteractions InspectSection = "interactions"
	SectionLog          InspectSection = "log"
)

// validSections is the source of truth for both --section validation
// and the help string. Order is the documented preference.
var validSections = []InspectSection{
	SectionSummary, SectionEvents, SectionTrace, SectionTools,
	SectionArtifacts, SectionInteractions, SectionLog, SectionAll,
}

// InspectOptions holds the configuration for the inspect command.
type InspectOptions struct {
	RunID    string
	StoreDir string
	Events   bool // show event log
	Full     bool // show all details

	// Per-node selection (additive; absent = legacy run-level path).

	Node      string
	Branch    string
	Iteration *int // nil = unset; -1 = latest started
	// ExecutionID is the alternative single-string selector
	// ("exec:<branch>:<node>:<iter>"). Mutually exclusive with --node.
	ExecutionID string
	Section     InspectSection
	LogTail     int
	ListNodes   bool
}

// IterationLatest is the sentinel value of *InspectOptions.Iteration
// meaning "use the most recently started iteration of (branch, node)".
const IterationLatest = -1

// RunInspect loads and displays a run's state.
func RunInspect(opts InspectOptions, p *Printer) error {
	if err := validateInspectOptions(&opts); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	// If no run ID, list all runs.
	if opts.RunID == "" {
		return listRuns(s, p)
	}

	// Per-node / per-execution / list-nodes paths take priority over
	// the legacy run-level summary so they're available alongside
	// `--events` / `--full` for power users.
	if opts.ListNodes {
		return listNodeExecutions(s, opts.RunID, p)
	}
	if opts.Node != "" || opts.ExecutionID != "" {
		return runInspectNode(s, storeDir, opts, p)
	}

	// Load run.
	r, err := s.LoadRun(context.Background(), opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	if p.Format == OutputJSON {
		result := map[string]interface{}{
			"run": r,
		}
		if opts.Events || opts.Full {
			events, err := s.LoadEvents(context.Background(), opts.RunID)
			if err == nil {
				result["events"] = events
			}
		}
		if opts.Full {
			interactions, _ := s.ListInteractions(context.Background(), opts.RunID)
			if len(interactions) > 0 {
				var ints []interface{}
				for _, id := range interactions {
					inter, err := s.LoadInteraction(context.Background(), opts.RunID, id)
					if err == nil {
						ints = append(ints, inter)
					}
				}
				result["interactions"] = ints
			}
		}
		p.JSON(result)
		return nil
	}

	// Human output.
	p.Header("Inspect: " + opts.RunID)
	if r.Name != "" {
		p.KV("Name", r.Name)
	}
	p.KV("Workflow", r.WorkflowName)
	p.KV("Status", StatusIcon(string(r.Status))+" "+string(r.Status))
	p.KV("Created", FormatTime(r.CreatedAt))
	p.KV("Updated", FormatTime(r.UpdatedAt))
	if r.FinishedAt != nil {
		p.KV("Finished", FormatTime(*r.FinishedAt))
		p.KV("Duration", FormatDuration(r.FinishedAt.Sub(r.CreatedAt)))
	}
	if r.Error != "" {
		p.KV("Error", r.Error)
	}

	// Checkpoint info (for paused runs).
	if r.Checkpoint != nil {
		p.Blank()
		p.Header("Checkpoint")
		p.KV("Paused at", r.Checkpoint.NodeID)
		p.KV("Interaction", r.Checkpoint.InteractionID)

		// Show pending interaction questions.
		inter, err := s.LoadInteraction(context.Background(), opts.RunID, r.Checkpoint.InteractionID)
		if err == nil && len(inter.Questions) > 0 {
			p.Blank()
			p.Line("  Questions:")
			for k, v := range inter.Questions {
				p.Line("    %s: %v", k, v)
			}
		}
	}

	// Events.
	if opts.Events || opts.Full {
		events, err := s.LoadEvents(context.Background(), opts.RunID)
		if err == nil && len(events) > 0 {
			p.Blank()
			p.Header("Events")
			rows := make([][]string, 0, len(events))
			for _, evt := range events {
				rows = append(rows, []string{
					fmt.Sprintf("%d", evt.Seq),
					string(evt.Type),
					evt.NodeID,
					FormatTime(evt.Timestamp),
				})
			}
			p.Table([]string{"SEQ", "TYPE", "NODE", "TIMESTAMP"}, rows)
		}
	}

	// Interactions.
	if opts.Full {
		interIDs, _ := s.ListInteractions(context.Background(), opts.RunID)
		if len(interIDs) > 0 {
			p.Blank()
			p.Header("Interactions")
			for _, id := range interIDs {
				inter, err := s.LoadInteraction(context.Background(), opts.RunID, id)
				if err != nil {
					continue
				}
				p.KV("ID", inter.ID)
				p.KV("Node", inter.NodeID)
				p.KV("Requested", FormatTime(inter.RequestedAt))
				if inter.AnsweredAt != nil {
					p.KV("Answered", FormatTime(*inter.AnsweredAt))
				}
				p.Blank()
			}
		}
	}

	return nil
}

// listRuns shows all runs in the store.
func listRuns(s store.RunStore, p *Printer) error {
	ids, err := s.ListRuns(context.Background())
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		if p.Format == OutputJSON {
			p.JSON([]interface{}{})
		} else {
			p.Line("No runs found.")
		}
		return nil
	}

	if p.Format == OutputJSON {
		var runs []interface{}
		for _, id := range ids {
			r, err := s.LoadRun(context.Background(), id)
			if err == nil {
				runs = append(runs, r)
			}
		}
		p.JSON(runs)
		return nil
	}

	p.Header("Runs")
	rows := make([][]string, 0, len(ids))
	for _, id := range ids {
		r, err := s.LoadRun(context.Background(), id)
		if err != nil {
			rows = append(rows, []string{"?", id, "?", "?", "?"})
			continue
		}
		name := r.Name
		if name == "" {
			name = "—"
		}
		rows = append(rows, []string{
			name,
			r.ID,
			string(r.Status),
			r.WorkflowName,
			FormatTime(r.CreatedAt),
		})
	}
	p.Table([]string{"NAME", "ID", "STATUS", "WORKFLOW", "CREATED"}, rows)
	return nil
}

// validateInspectOptions enforces the mutually-exclusive flag
// combinations cobra does not detect on its own.
func validateInspectOptions(opts *InspectOptions) error {
	if opts.Node != "" && opts.ExecutionID != "" {
		return fmt.Errorf("--node and --exec are mutually exclusive")
	}
	if (opts.Branch != "" || opts.Iteration != nil) && opts.Node == "" {
		return fmt.Errorf("--branch / --iteration require --node")
	}
	if opts.Section != "" && opts.Node == "" && opts.ExecutionID == "" {
		return fmt.Errorf("--section requires --node or --exec")
	}
	if opts.LogTail < 0 {
		return fmt.Errorf("--log-tail must be >= 0")
	}
	if opts.ListNodes && (opts.Node != "" || opts.ExecutionID != "" || opts.Section != "") {
		return fmt.Errorf("--list-nodes is mutually exclusive with --node / --exec / --section")
	}
	if opts.ListNodes && opts.RunID == "" {
		return fmt.Errorf("--list-nodes requires --run-id")
	}
	if opts.Section != "" && !isValidSection(opts.Section) {
		return fmt.Errorf("invalid --section %q (valid: %s)", opts.Section, sectionList())
	}
	return nil
}

func isValidSection(s InspectSection) bool {
	for _, v := range validSections {
		if v == s {
			return true
		}
	}
	return false
}

func sectionList() string {
	out := make([]string, len(validSections))
	for i, v := range validSections {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
