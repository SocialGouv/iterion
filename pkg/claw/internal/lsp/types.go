package lsp

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// LspAction
// ---------------------------------------------------------------------------

// LspAction represents a supported LSP action.
type LspAction int

const (
	ActionDiagnostics LspAction = iota
	ActionHover
	ActionDefinition
	ActionReferences
	ActionCompletion
	ActionSymbols
	ActionFormat
)

var actionStrings = [...]string{
	"diagnostics",
	"hover",
	"definition",
	"references",
	"completion",
	"symbols",
	"format",
}

func (a LspAction) String() string {
	if int(a) < len(actionStrings) {
		return actionStrings[a]
	}
	return "unknown"
}

// ParseAction parses an action string, supporting aliases.
func ParseAction(s string) (LspAction, bool) {
	switch s {
	case "diagnostics":
		return ActionDiagnostics, true
	case "hover":
		return ActionHover, true
	case "definition", "goto_definition":
		return ActionDefinition, true
	case "references", "find_references":
		return ActionReferences, true
	case "completion", "completions":
		return ActionCompletion, true
	case "symbols", "document_symbols":
		return ActionSymbols, true
	case "format", "formatting":
		return ActionFormat, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// LspServerStatus
// ---------------------------------------------------------------------------

// LspServerStatus represents the status of an LSP server.
type LspServerStatus int

const (
	StatusConnected LspServerStatus = iota
	StatusDisconnected
	StatusStarting
	StatusError
)

var serverStatusStrings = [...]string{
	"connected",
	"disconnected",
	"starting",
	"error",
}

func (s LspServerStatus) String() string {
	if int(s) < len(serverStatusStrings) {
		return serverStatusStrings[s]
	}
	return "unknown"
}

func (s LspServerStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *LspServerStatus) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	for i, name := range serverStatusStrings {
		if name == str {
			*s = LspServerStatus(i)
			return nil
		}
	}
	return fmt.Errorf("unknown LSP server status: %q", str)
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

// LspDiagnostic represents a single diagnostic.
type LspDiagnostic struct {
	Path      string  `json:"path"`
	Line      uint32  `json:"line"`
	Character uint32  `json:"character"`
	Severity  string  `json:"severity"`
	Message   string  `json:"message"`
	Source    *string `json:"source,omitempty"`
}

// LspLocation represents a source code location.
type LspLocation struct {
	Path         string  `json:"path"`
	Line         uint32  `json:"line"`
	Character    uint32  `json:"character"`
	EndLine      *uint32 `json:"end_line,omitempty"`
	EndCharacter *uint32 `json:"end_character,omitempty"`
	Preview      *string `json:"preview,omitempty"`
}

// LspHoverResult represents hover information.
type LspHoverResult struct {
	Content  string  `json:"content"`
	Language *string `json:"language,omitempty"`
}

// LspCompletionItem represents a completion suggestion.
type LspCompletionItem struct {
	Label      string  `json:"label"`
	Kind       *string `json:"kind,omitempty"`
	Detail     *string `json:"detail,omitempty"`
	InsertText *string `json:"insert_text,omitempty"`
}

// LspSymbol represents a code symbol.
type LspSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// LspServerState tracks the state of a registered language server.
type LspServerState struct {
	Language     string          `json:"language"`
	Status       LspServerStatus `json:"status"`
	RootPath     *string         `json:"root_path,omitempty"`
	Capabilities []string        `json:"capabilities"`
	Diagnostics  []LspDiagnostic `json:"diagnostics"`
}

func (s *LspServerState) clone() LspServerState {
	c := *s
	if s.Capabilities != nil {
		c.Capabilities = make([]string, len(s.Capabilities))
		copy(c.Capabilities, s.Capabilities)
	}
	if s.Diagnostics != nil {
		c.Diagnostics = make([]LspDiagnostic, len(s.Diagnostics))
		copy(c.Diagnostics, s.Diagnostics)
	}
	if s.RootPath != nil {
		rp := *s.RootPath
		c.RootPath = &rp
	}
	return c
}
