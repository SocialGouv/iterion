package cli

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

// InspectOptions holds the configuration for the inspect command.
type InspectOptions struct {
	RunID    string
	StoreDir string
	Events   bool // show event log
	Full     bool // show all details
}

// RunInspect loads and displays a run's state.
func RunInspect(opts InspectOptions, p *Printer) error {
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	// If no run ID, list all runs.
	if opts.RunID == "" {
		return listRuns(s, p)
	}

	// Load run.
	r, err := s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	if p.Format == OutputJSON {
		result := map[string]interface{}{
			"run": r,
		}
		if opts.Events || opts.Full {
			events, err := s.LoadEvents(opts.RunID)
			if err == nil {
				result["events"] = events
			}
		}
		if opts.Full {
			interactions, _ := s.ListInteractions(opts.RunID)
			if len(interactions) > 0 {
				var ints []interface{}
				for _, id := range interactions {
					inter, err := s.LoadInteraction(opts.RunID, id)
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
		inter, err := s.LoadInteraction(opts.RunID, r.Checkpoint.InteractionID)
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
		events, err := s.LoadEvents(opts.RunID)
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
		interIDs, _ := s.ListInteractions(opts.RunID)
		if len(interIDs) > 0 {
			p.Blank()
			p.Header("Interactions")
			for _, id := range interIDs {
				inter, err := s.LoadInteraction(opts.RunID, id)
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
func listRuns(s *store.RunStore, p *Printer) error {
	ids, err := s.ListRuns()
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
			r, err := s.LoadRun(id)
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
		r, err := s.LoadRun(id)
		if err != nil {
			rows = append(rows, []string{id, "?", "?", "?"})
			continue
		}
		rows = append(rows, []string{
			r.ID,
			string(r.Status),
			r.WorkflowName,
			FormatTime(r.CreatedAt),
		})
	}
	p.Table([]string{"ID", "STATUS", "WORKFLOW", "CREATED"}, rows)
	return nil
}
