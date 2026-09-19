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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	// ReadOnly marks tools that never mutate state — it feeds the MCP
	// readOnlyHint annotation, so it must be TRUTHFUL about the tool's
	// capability, not about a mode gate.
	ReadOnly bool
	// ListedInReadOnly keeps a mutating tool exposed in read-only mode
	// because its handler enforces its own read-only restriction
	// (remote_api goes GET-only). Meaningless on ReadOnly tools.
	ListedInReadOnly bool
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

	// storeMu guards the lazily-opened stores. Laziness keeps
	// remote-only usage from ever touching the local disk; a failed
	// open is NOT latched, so a store created later in the session
	// (e.g. by a first mutating call) becomes visible to reads.
	storeMu    sync.Mutex
	runStore   *store.FilesystemRunStore
	boardStore *native.Store

	// spawnGate serializes the check-then-spawn sections of
	// local_run/local_resume so two concurrent calls cannot both pass
	// the live-runner check and clobber each other's .pid.
	spawnGate sync.Mutex

	tools     []Tool
	toolIndex map[string]*Tool
	buildOnce sync.Once

	// argsDecodeObserver, when set, sees every strict decode before it
	// runs: the tool's name and the pointer its handler decodes into.
	// A test seam — the schema↔struct parity test reads each handler's
	// argument type from the one chokepoint every tool crosses, so the
	// parity it checks is the decode that runs, not a parallel list.
	argsDecodeObserver func(toolName string, dest any)
}

// build assembles the tool registry honoring Only + ReadOnly.
func (s *Server) build() {
	s.buildOnce.Do(func() {
		var all []Tool
		if s.Only != FamilyRemote {
			all = append(all, localTools()...)
			all = append(all, localBoardTools()...)
			all = append(all, localMapTools()...)
		}
		if s.Only != FamilyLocal {
			all = append(all, remoteTools()...)
		}
		filtered := all[:0]
		for _, t := range all {
			if s.ReadOnly && !t.ReadOnly && !t.ListedInReadOnly {
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

// store returns the lazily-opened local run store. In read-only mode
// an ABSENT store is an explicit error instead of an implicit mkdir:
// store.New creates runs/ + .gitignore, and "read-only" must not
// write anything to disk.
func (s *Server) store() (*store.FilesystemRunStore, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.runStore != nil {
		return s.runStore, nil
	}
	if s.ReadOnly {
		if fi, err := os.Stat(s.StoreDir); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("read-only mode: no run store at %s (opening one would create it)", s.StoreDir)
		}
	}
	st, err := store.New(s.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("open run store %s: %w", s.StoreDir, err)
	}
	s.runStore = st
	return st, nil
}

// board returns the lazily-opened native board store at
// <store-dir>/dispatcher — the same resolution as `iterion issue` and
// the __mcp-board server. Same read-only rule as store(): an absent
// board is reported, never initialised (native.NewStore writes
// board.json).
func (s *Server) board() (*native.Store, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	if s.boardStore != nil {
		return s.boardStore, nil
	}
	root := boardRoot(s.StoreDir)
	if s.ReadOnly {
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("read-only mode: no board at %s (opening one would initialise it)", root)
		}
	}
	b, err := native.NewStore(root)
	if err != nil {
		return nil, fmt.Errorf("open board store %s: %w", root, err)
	}
	s.boardStore = b
	return b, nil
}

// unmarshalArgs decodes a tools/call arguments blob strictly. An
// argument the tool does not declare is a tool error naming the
// unknown key and the accepted arguments (from its inputSchema),
// not a silent drop against the schema's declared
// additionalProperties: false. An absent/empty blob is treated as
// an empty object.
//
// toolName identifies the tool being called — used to look up the
// inputSchema and render the accepted-keys list in the error
// message. Handlers pass their own name literal; a name unknown to
// the server yields an error without the accepted-keys hint, which
// is still louder than a silent drop.
//
// Fixes #1335: a misspelt or misplaced argument (e.g. `vras` for
// `vars`) used to be a silent no-op, letting the LLM proceed on a
// false result while every tool's schema advertised the promise
// the server did not keep.
func (s *Server) unmarshalArgs(toolName string, raw json.RawMessage, dest any) error {
	if s.argsDecodeObserver != nil {
		s.argsDecodeObserver(toolName, dest)
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return s.wrapArgsError(toolName, err)
	}
	// Decoder.Decode reads the FIRST value only: without this, trailing
	// garbage after the arguments object is silently ignored — the same
	// class of silent tolerance the unknown-key check exists for.
	if dec.More() {
		return fmt.Errorf("invalid arguments: trailing data after the arguments JSON value")
	}
	return nil
}

// wrapArgsError renders a decode failure as a tool-friendly message.
// When DisallowUnknownFields triggered — Go's json package returns
// `json: unknown field "…"` verbatim — the message names the
// offending key AND the accepted keys the tool's inputSchema
// declares, so an LLM reading the tool error can correct the call
// on the next turn (the whole point of #1335).
func (s *Server) wrapArgsError(toolName string, err error) error {
	const prefix = "json: unknown field "
	msg := err.Error()
	if strings.HasPrefix(msg, prefix) {
		unknown := strings.TrimSpace(strings.TrimPrefix(msg, prefix))
		unknown = strings.Trim(unknown, `"`)
		accepted := s.acceptedArgKeys(toolName)
		acceptedStr := "(none declared)"
		if len(accepted) > 0 {
			acceptedStr = strings.Join(accepted, ", ")
		}
		return fmt.Errorf("invalid arguments: unknown field %q — the tool's inputSchema declares additionalProperties: false; accepted keys: %s", unknown, acceptedStr)
	}
	return fmt.Errorf("invalid arguments: %w", err)
}

// acceptedArgKeys returns the sorted list of keys the tool's
// inputSchema.properties declares. Returns an empty list when the
// tool is unknown to the server or its schema is malformed —
// neither should happen for a registered tool, and both are safe
// fallbacks (the error text degrades to "(none declared)").
func (s *Server) acceptedArgKeys(toolName string) []string {
	s.build()
	t, ok := s.toolIndex[toolName]
	if !ok {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return nil
	}
	keys := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
