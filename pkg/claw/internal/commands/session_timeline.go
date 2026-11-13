package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionDirProvider is implemented by adapters that can hand out the
// directory where session JSON files live. Timeline and lineage are
// pure-read operations: they don't need the full SessionManager surface,
// just enough to locate the on-disk store.
type SessionDirProvider interface {
	SessionDir() string
}

// timelineSession is the minimal subset of runtime.Session this package
// needs to render a chronological view. Decoupling from runtime types
// keeps the commands package's dependency graph shallow and makes the
// renderer easy to unit-test.
type timelineSession struct {
	ID            string                `json:"id"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Messages      []timelineMessage     `json:"messages"`
	PromptHistory []timelinePromptEntry `json:"prompt_history,omitempty"`
	Fork          *timelineFork         `json:"fork,omitempty"`
	Compaction    int                   `json:"compaction_count,omitempty"`
}

type timelineMessage struct {
	Role    string                 `json:"role"`
	Content []timelineContentBlock `json:"content"`
}

type timelineContentBlock struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

type timelinePromptEntry struct {
	TimestampMs int64  `json:"timestamp_ms"`
	Text        string `json:"text"`
}

type timelineFork struct {
	ParentSessionID string `json:"parent_session_id"`
	BranchName      string `json:"branch_name,omitempty"`
}

// loadTimelineSession reads a single session JSON file from dir.
func loadTimelineSession(dir, id string) (*timelineSession, error) {
	path := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", id, err)
	}
	var s timelineSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", id, err)
	}
	if s.ID == "" {
		s.ID = id
	}
	return &s, nil
}

// renderTimeline writes a chronological event listing for the session to
// w. Each line is prefixed with a relative timestamp from the session's
// CreatedAt. Messages with no per-message timestamp are slotted in after
// the most recent prompt-history entry so order tracks user turns.
func renderTimeline(w io.Writer, s *timelineSession) {
	fmt.Fprintf(w, "Session: %s\n", s.ID)
	fmt.Fprintf(w, "Started: %s\n", s.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Updated: %s\n", s.UpdatedAt.UTC().Format(time.RFC3339))
	if s.Fork != nil && s.Fork.ParentSessionID != "" {
		if s.Fork.BranchName != "" {
			fmt.Fprintf(w, "Forked from %s (branch %s)\n", s.Fork.ParentSessionID, s.Fork.BranchName)
		} else {
			fmt.Fprintf(w, "Forked from %s\n", s.Fork.ParentSessionID)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Timeline:")
	fmt.Fprintf(w, "  🟢 [+0.000s] session created\n")

	type evt struct {
		offset time.Duration
		emoji  string
		line   string
	}
	var events []evt

	start := s.CreatedAt
	for _, ph := range s.PromptHistory {
		ts := time.UnixMilli(ph.TimestampMs)
		events = append(events, evt{
			offset: ts.Sub(start),
			emoji:  "💬",
			line:   "user prompt: " + truncate(ph.Text, 80),
		})
	}

	// Walk messages in their saved order. We don't have per-message
	// timestamps, so we space them evenly between the last prompt and
	// UpdatedAt — purely for monotonic display, not millisecond accuracy.
	if len(s.Messages) > 0 {
		var lastTs time.Time
		if n := len(s.PromptHistory); n > 0 {
			lastTs = time.UnixMilli(s.PromptHistory[n-1].TimestampMs)
		} else {
			lastTs = start
		}
		span := s.UpdatedAt.Sub(lastTs)
		if span < 0 {
			span = 0
		}
		step := span / time.Duration(len(s.Messages)+1)
		for i, msg := range s.Messages {
			ts := lastTs.Add(time.Duration(i+1) * step)
			for _, line := range describeMessage(msg) {
				events = append(events, evt{
					offset: ts.Sub(start),
					emoji:  line.emoji,
					line:   line.text,
				})
			}
		}
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].offset < events[j].offset
	})

	for _, e := range events {
		fmt.Fprintf(w, "  %s [+%.3fs] %s\n", e.emoji, e.offset.Seconds(), e.line)
	}

	if s.Compaction > 0 {
		fmt.Fprintf(w, "  📦 (%d compaction event(s) on this session)\n", s.Compaction)
	}
}

type messageLine struct {
	emoji string
	text  string
}

func describeMessage(msg timelineMessage) []messageLine {
	var lines []messageLine
	for _, cb := range msg.Content {
		switch cb.Type {
		case "text":
			emoji := "🤖"
			if msg.Role == "user" {
				emoji = "👤"
			} else if msg.Role == "system" {
				emoji = "⚙️"
			}
			text := truncate(cb.Text, 80)
			if text == "" {
				continue
			}
			lines = append(lines, messageLine{emoji: emoji, text: msg.Role + ": " + text})
		case "tool_use":
			lines = append(lines, messageLine{emoji: "🔧", text: "tool call: " + cb.Name})
		case "tool_result":
			emoji := "✅"
			label := "tool result"
			if cb.IsError {
				emoji = "❌"
				label = "tool error"
			}
			snippet := truncate(cb.Text, 80)
			if snippet != "" {
				label += ": " + snippet
			}
			lines = append(lines, messageLine{emoji: emoji, text: label})
		case "image":
			lines = append(lines, messageLine{emoji: "🖼️", text: "image attached"})
		}
	}
	return lines
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lineageNode is a single node in the fork tree.
type lineageNode struct {
	ID       string
	Branch   string
	Children []*lineageNode
}

// buildLineage walks every session JSON file under dir and reconstructs
// the parent→children tree rooted at rootID.
func buildLineage(dir, rootID string) (*lineageNode, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read session dir: %w", err)
	}
	parents := make(map[string][]*lineageNode) // parent → children
	branches := make(map[string]string)
	all := make(map[string]bool)
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		all[id] = true
		s, err := loadTimelineSession(dir, id)
		if err != nil {
			continue
		}
		if s.Fork == nil || s.Fork.ParentSessionID == "" {
			continue
		}
		node := &lineageNode{ID: id, Branch: s.Fork.BranchName}
		parents[s.Fork.ParentSessionID] = append(parents[s.Fork.ParentSessionID], node)
		if s.Fork.BranchName != "" {
			branches[id] = s.Fork.BranchName
		}
	}

	if !all[rootID] {
		return nil, fmt.Errorf("session %s not found in %s", rootID, dir)
	}

	root := &lineageNode{ID: rootID, Branch: branches[rootID]}
	var attach func(n *lineageNode)
	attach = func(n *lineageNode) {
		kids := parents[n.ID]
		sort.Slice(kids, func(i, j int) bool { return kids[i].ID < kids[j].ID })
		n.Children = kids
		for _, k := range kids {
			attach(k)
		}
	}
	attach(root)
	return root, nil
}

func renderLineage(w io.Writer, root *lineageNode) {
	fmt.Fprintf(w, "Lineage rooted at %s:\n", root.ID)
	if len(root.Children) == 0 {
		fmt.Fprintf(w, "  %s (no forks)\n", root.ID)
		return
	}
	fmt.Fprintf(w, "%s (root)\n", root.ID)
	var walk func(n *lineageNode, prefix string)
	walk = func(n *lineageNode, prefix string) {
		for i, child := range n.Children {
			isLast := i == len(n.Children)-1
			branch := ""
			if child.Branch != "" {
				branch = " [" + child.Branch + "]"
			}
			connector := "├─ "
			extension := "│  "
			if isLast {
				connector = "└─ "
				extension = "   "
			}
			fmt.Fprintf(w, "%s%s%s%s\n", prefix, connector, child.ID, branch)
			walk(child, prefix+extension)
		}
	}
	walk(root, "")
}

// RegisterSessionTimelineCommands registers the /timeline and /lineage
// commands. Both are read-only and look up the session directory via the
// SessionDirProvider interface, so they work even when no active loop is
// attached as long as the adapter exposes its session dir.
func RegisterSessionTimelineCommands(r *Registry) {
	r.Register(Command{
		Name:            "timeline",
		Description:     "Show a chronological event view of a saved session",
		ArgumentHint:    "<session-id>",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			id := strings.TrimSpace(args)
			if id == "" {
				fmt.Println("Usage: /timeline <session-id>")
				return nil
			}
			dir, err := resolveSessionDir(loop)
			if err != nil {
				return err
			}
			s, err := loadTimelineSession(dir, id)
			if err != nil {
				return err
			}
			renderTimeline(os.Stdout, s)
			return nil
		},
	})

	r.Register(Command{
		Name:            "lineage",
		Description:     "Show the fork tree rooted at a saved session",
		ArgumentHint:    "<session-id>",
		ResumeSupported: true,
		Category:        CategorySession,
		Handler: func(args string, loop interface{}) error {
			id := strings.TrimSpace(args)
			if id == "" {
				fmt.Println("Usage: /lineage <session-id>")
				return nil
			}
			dir, err := resolveSessionDir(loop)
			if err != nil {
				return err
			}
			root, err := buildLineage(dir, id)
			if err != nil {
				return err
			}
			renderLineage(os.Stdout, root)
			return nil
		},
	})
}

func resolveSessionDir(loop interface{}) (string, error) {
	if p, ok := loop.(SessionDirProvider); ok {
		if dir := p.SessionDir(); dir != "" {
			return dir, nil
		}
	}
	return "", fmt.Errorf("session directory not available in this context")
}
