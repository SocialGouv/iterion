package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// WatchStore is the minimal run-store surface the watch tools need: the
// per-run watched-issue set mutators added by MVP3b. *store.FilesystemRunStore
// satisfies it structurally, so this package stays decoupled from pkg/store.
type WatchStore interface {
	AddWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error)
	RemoveWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error)
}

// WatchConfig configures in-process watch-subscription tools for the claw
// backend. RunID binds every call to the executing run — the claw executor
// is built once per run, so the value is constant for the executor's life.
// Pass a nil Store or empty RunID to disable registration (no-op).
type WatchConfig struct {
	Store        WatchStore
	RunID        string
	Capabilities []string
}

// watchInputSchema is shared by both tools: a single issue_id string.
const watchInputSchema = `{
  "type":"object",
  "properties":{"issue_id":{"type":"string","description":"Native-board issue ID (or the unambiguous prefix the dispatcher resolved) to watch."}},
  "required":["issue_id"]
}`

// RegisterClawWatchTools registers watch.subscribe / watch.unsubscribe as
// in-process claw tools under the iterion_watch server (claude_code-FQN
// mcp__iterion_watch__*), filtered by the granted capabilities. Subscribing
// adds the issue to this run's WatchedIssueIDs set; the server-side watch
// coordinator (MVP3b) then delivers a queued message to the run whenever the
// issue changes state on the native board. cfg.Capabilities is the surface
// the REGISTRY carries; a node is held to its own watch grant when the tool
// is CALLED (requireHostCapability), same as the board tools.
func RegisterClawWatchTools(reg *Registry, cfg *WatchConfig) error {
	if cfg == nil || cfg.Store == nil || cfg.RunID == "" {
		return nil
	}
	granted := make(map[string]bool, len(cfg.Capabilities))
	for _, c := range cfg.Capabilities {
		granted[c] = true
	}

	type watchTool struct {
		name string
		add  bool
		desc string
	}
	tools := []watchTool{
		{"subscribe", true, "Subscribe this run to a native-board issue: you will receive a queued message whenever the issue changes state. Use it right after dispatching an issue you want to track."},
		{"unsubscribe", false, "Stop receiving updates for a native-board issue this run previously watched."},
	}

	for _, t := range tools {
		if !granted[delegate.WatchToolCapability(t.name)] {
			continue
		}
		t := t
		exec := func(ctx context.Context, input json.RawMessage) (string, error) {
			if err := requireHostCapability(ctx, "mcp."+delegate.WatchMCPServerName+"."+t.name, delegate.WatchToolCapability(t.name)); err != nil {
				return "", err
			}
			var args struct {
				IssueID string `json:"issue_id"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("watch %s: invalid args: %w", t.name, err)
			}
			if args.IssueID == "" {
				return "", fmt.Errorf("watch %s: issue_id is required", t.name)
			}
			var (
				set []string
				err error
			)
			if t.add {
				set, err = cfg.Store.AddWatchedIssues(ctx, cfg.RunID, []string{args.IssueID})
			} else {
				set, err = cfg.Store.RemoveWatchedIssues(ctx, cfg.RunID, []string{args.IssueID})
			}
			if err != nil {
				return "", fmt.Errorf("watch %s: %w", t.name, err)
			}
			out, _ := json.Marshal(map[string]any{"watched_issue_ids": set})
			return string(out), nil
		}
		if err := reg.RegisterMCP(delegate.WatchMCPServerName, t.name, t.desc, json.RawMessage(watchInputSchema), exec); err != nil {
			return fmt.Errorf("register %s/%s: %w", delegate.WatchMCPServerName, t.name, err)
		}
	}
	return nil
}
