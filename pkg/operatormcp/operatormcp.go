// Package operatormcp implements the operator-facing iterion MCP tool
// surface served by `iterion mcp` — the seam that lets any MCP client
// (Claude Code, the desktop, an IDE) drive iterion end to end.
//
// Two tool families share one server:
//   - local_*  — the local store and engine: validate/launch/follow runs,
//     the native kanban board, bot discovery. Reads go straight to the
//     run store; launches spawn a detached `iterion run --background`
//     subprocess so the run survives the MCP client's session.
//   - remote_* — a logged-in remote instance (`iterion remote login`,
//     or ITERION_REMOTE_URL/_TOKEN) over its HTTP API: a typed core plus
//     the remote_api escape hatch and route/OpenAPI discovery, mirroring
//     the `iterion remote` CLI positioning.
//
// Following the boardops precedent, this package owns the tool
// definitions and dispatch; the stdio JSON-RPC framing lives with the
// other MCP servers in cmd/iterion.
package operatormcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Family selects which tool families a Server exposes.
type Family string

const (
	// FamilyAll exposes both the local_* and remote_* tools.
	FamilyAll Family = ""
	// FamilyLocal exposes only the local_* tools.
	FamilyLocal Family = "local"
	// FamilyRemote exposes only the remote_* tools.
	FamilyRemote Family = "remote"
)

// ParseFamily validates an `--only` flag value.
func ParseFamily(s string) (Family, error) {
	switch Family(strings.TrimSpace(strings.ToLower(s))) {
	case FamilyAll:
		return FamilyAll, nil
	case FamilyLocal:
		return FamilyLocal, nil
	case FamilyRemote:
		return FamilyRemote, nil
	}
	return FamilyAll, fmt.Errorf("invalid --only value %q (want local or remote)", s)
}

// Tool is one MCP tool: metadata for tools/list plus its handler.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnly marks tools that never mutate state. In read-only mode
	// only these are listed/callable (remote_api stays listed but its
	// handler enforces GET-only — see tools_remote.go).
	ReadOnly bool
	// handler returns the text content block for the tools/call result.
	// isErr flags a tool-level failure the LLM should route on (e.g. an
	// HTTP error body); a non-nil error is reported the same way with
	// err.Error() as the text.
	handler func(ctx context.Context, s *Server, raw json.RawMessage) (text string, isErr bool, err error)
}

// Server resolves and dispatches the operator MCP tool set.
type Server struct {
	// StoreDir is the resolved run-store directory local tools operate
	// on (store.ResolveStoreDir applied by the CLI entry point).
	StoreDir string
	// WorkDir is the working directory relative paths resolve against
	// (bot discovery, workflow files).
	WorkDir string
	// ReadOnly hides and refuses every mutating tool.
	ReadOnly bool
	// Only restricts the exposed families (FamilyAll = both).
	Only Family

	// openStoreOnce lazily opens the run store so remote-only usage
	// never creates a local store directory.
	openStoreOnce sync.Once
	runStore      *store.FilesystemRunStore
	runStoreErr   error

	openBoardOnce sync.Once
	boardStore    *native.Store
	boardStoreErr error

	tools     []Tool
	toolIndex map[string]*Tool
	buildOnce sync.Once
}

// build assembles the tool registry honoring Only + ReadOnly.
func (s *Server) build() {
	s.buildOnce.Do(func() {
		var all []Tool
		if s.Only != FamilyRemote {
			all = append(all, localTools()...)
			all = append(all, localBoardTools()...)
		}
		if s.Only != FamilyLocal {
			all = append(all, remoteTools()...)
		}
		filtered := all[:0]
		for _, t := range all {
			if s.ReadOnly && !t.ReadOnly {
				continue
			}
			filtered = append(filtered, t)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
		s.tools = filtered
		s.toolIndex = make(map[string]*Tool, len(filtered))
		for i := range filtered {
			s.toolIndex[filtered[i].Name] = &filtered[i]
		}
	})
}

// Tools returns the exposed tool list, sorted by name.
func (s *Server) Tools() []Tool {
	s.build()
	return s.tools
}

// CallResult is the MCP tools/call result payload: one text content
// block plus the isError routing flag.
type CallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// ContentBlock is a single MCP text content block.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ErrUnknownTool reports a tools/call against a name that is not
// exposed (unknown, family-filtered, or hidden by read-only mode). The
// transport maps it to a JSON-RPC method-level error rather than a
// tool result.
type ErrUnknownTool struct{ Name string }

func (e *ErrUnknownTool) Error() string { return fmt.Sprintf("unknown tool: %s", e.Name) }

// Call dispatches one tools/call invocation.
func (s *Server) Call(ctx context.Context, name string, raw json.RawMessage) (CallResult, error) {
	s.build()
	t, ok := s.toolIndex[name]
	if !ok {
		return CallResult{}, &ErrUnknownTool{Name: name}
	}
	text, isErr, err := t.handler(ctx, s, raw)
	if err != nil {
		return textResult(err.Error(), true), nil
	}
	return textResult(text, isErr), nil
}

func textResult(text string, isErr bool) CallResult {
	return CallResult{
		Content: []ContentBlock{{Type: "text", Text: text}},
		IsError: isErr,
	}
}

// store returns the lazily-opened local run store.
func (s *Server) store() (*store.FilesystemRunStore, error) {
	s.openStoreOnce.Do(func() {
		s.runStore, s.runStoreErr = store.New(s.StoreDir)
	})
	if s.runStoreErr != nil {
		return nil, fmt.Errorf("open run store %s: %w", s.StoreDir, s.runStoreErr)
	}
	return s.runStore, nil
}

// board returns the lazily-opened native board store at
// <store-dir>/dispatcher — the same resolution as `iterion issue` and
// the __mcp-board server.
func (s *Server) board() (*native.Store, error) {
	s.openBoardOnce.Do(func() {
		s.boardStore, s.boardStoreErr = native.NewStore(boardRoot(s.StoreDir))
	})
	if s.boardStoreErr != nil {
		return nil, fmt.Errorf("open board store %s: %w", boardRoot(s.StoreDir), s.boardStoreErr)
	}
	return s.boardStore, nil
}

// unmarshalArgs decodes a tools/call arguments blob into dest, treating
// an absent/empty blob as an empty object.
func unmarshalArgs(raw json.RawMessage, dest any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// marshalText renders v as indented JSON for a text content block.
func marshalText(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(b), nil
}

// captureJSON runs fn with a JSON-mode Printer writing into a buffer
// and returns what it printed — the reuse seam for the pkg/cli
// functions that render through a Printer.
func captureJSON(fn func(p *cli.Printer) error) (string, error) {
	var buf strings.Builder
	err := fn(&cli.Printer{W: &buf, Format: cli.OutputJSON})
	return strings.TrimSpace(buf.String()), err
}

// captureHuman is captureJSON in human mode — used where the human
// rendering is the LLM-friendly one (e.g. the markdown run report).
func captureHuman(fn func(p *cli.Printer) error) (string, error) {
	var buf strings.Builder
	err := fn(&cli.Printer{W: &buf, Format: cli.OutputHuman})
	return strings.TrimSpace(buf.String()), err
}
