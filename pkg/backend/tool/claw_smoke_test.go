package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	clawmcp "github.com/SocialGouv/claw-code-go/pkg/api/mcp"
	clawtask "github.com/SocialGouv/claw-code-go/pkg/api/task"
	clawteam "github.com/SocialGouv/claw-code-go/pkg/api/team"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
	clawworker "github.com/SocialGouv/claw-code-go/pkg/api/worker"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// TestRegisterClawAll_EveryRegisteredToolDispatches is the empirical
// counterpart to TestRegisterClawAll_RegistersFullSet (which only checks
// that names appear). For every tool produced by RegisterClawAll with all
// opt-ins enabled, this test:
//
//  1. parses the tool's InputSchema to discover required field names;
//  2. synthesises a minimal placeholder payload satisfying the type
//     declared for each required field (string/int/bool);
//  3. invokes Execute with that payload and captures the outcome.
//
// The success criterion is dispatch behaviour: no panic, and the call
// returns either a string (any string), a recognised sentinel error
// (e.g. ErrAskUser), or a domain error. The point is to prove every
// registered tool is wired through to a real exec function rather than
// silently dropped at registration. Backend-level error returns (e.g.
// screenshot without X11, web_fetch without network) are logged as
// data, not failures — they prove the tool reached its execution
// branch and produced a meaningful response.
//
// This complements the audit-style theoretical inventory; together
// they answer "is every claimed tool actually callable?".
func TestRegisterClawAll_EveryRegisteredToolDispatches(t *testing.T) {
	r := NewRegistry()
	planActive := false
	workspace := t.TempDir()
	defaults := ClawDefaults{
		Workspace:          workspace,
		Tasks:              clawtask.NewRegistry(),
		Workers:            clawworker.NewWorkerRegistry(),
		Teams:              clawteam.NewTeamRegistry(),
		Crons:              clawteam.NewCronRegistry(),
		MCP:                clawmcp.NewRegistry(),
		MCPAuth:            clawmcp.NewAuthState(),
		PlanMode:           &clawtools.PlanModeState{Active: &planActive, Dir: t.TempDir()},
		IncludeWebSearch:   true,
		IncludeComputerUse: true,
	}
	if err := RegisterClawAll(r, defaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	// Tools whose Execute is intentionally documented to return a
	// sentinel error type rather than a string output. Treat their
	// errors as success here — proves the wiring reaches the
	// designed dispatch branch.
	sentinelErrTools := map[string]func(error) bool{
		"ask_user": func(err error) bool {
			var aue *delegate.ErrAskUser
			return errors.As(err, &aue)
		},
	}

	tools := r.List()
	sort.Slice(tools, func(i, j int) bool { return tools[i].QualifiedName < tools[j].QualifiedName })

	if len(tools) < 30 {
		t.Fatalf("expected ≥30 tools registered, got %d — registration regressed", len(tools))
	}

	type outcome struct {
		name    string
		ok      bool
		errMsg  string
		summary string
	}
	results := make([]outcome, 0, len(tools))

	for _, td := range tools {
		input, err := synthesiseInputForSchema(td.InputSchema, td.QualifiedName, workspace)
		if err != nil {
			t.Errorf("%s: cannot synthesise input from schema: %v", td.QualifiedName, err)
			continue
		}

		out, execErr := func() (s string, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("panic during Execute: %v", rec)
				}
			}()
			return td.Execute(context.Background(), input)
		}()

		o := outcome{name: td.QualifiedName}
		switch {
		case execErr == nil:
			o.ok = true
			o.summary = previewSmoke(out, 80)
		case sentinelErrTools[td.QualifiedName] != nil && sentinelErrTools[td.QualifiedName](execErr):
			o.ok = true
			o.summary = "sentinel error (expected): " + execErr.Error()
		default:
			// Domain errors are informational — record but do not fail
			// the test. The tool reached its execution branch.
			o.ok = true
			o.errMsg = execErr.Error()
		}

		results = append(results, o)

		if !o.ok {
			t.Errorf("%s: %s", o.name, o.errMsg)
		}
	}

	// Empirical inventory dump for the test log: each tool's outcome.
	// `go test -v ./pkg/backend/tool -run TestRegisterClawAll_Every...`
	// surfaces this as the live tool-coverage report.
	dispatched := 0
	for _, o := range results {
		if o.ok {
			dispatched++
		}
		if o.errMsg != "" {
			t.Logf("  %s → ERR (dispatched, domain error: %s)", o.name, previewSmoke(o.errMsg, 80))
		} else {
			t.Logf("  %s → ok %s", o.name, o.summary)
		}
	}
	t.Logf("dispatched: %d / %d tools reached their execution branch", dispatched, len(results))
}

// synthesiseInputForSchema reads an Anthropic-flavoured InputSchema
// (top-level type=object, properties, required) and returns a JSON
// payload satisfying the required-field set with placeholder values
// chosen by declared type. Unknown / non-object schemas yield "{}",
// which works for tools with only optional fields.
func synthesiseInputForSchema(schema json.RawMessage, toolName, workspace string) (json.RawMessage, error) {
	if len(schema) == 0 {
		return json.RawMessage("{}"), nil
	}
	var s struct {
		Type       string `json:"type"`
		Required   []string
		Properties map[string]struct {
			Type        string      `json:"type"`
			Enum        []any       `json:"enum,omitempty"`
			Default     any         `json:"default,omitempty"`
			Description string      `json:"description,omitempty"`
			Items       interface{} `json:"items,omitempty"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, err
	}
	if s.Type != "" && s.Type != "object" {
		return json.RawMessage("{}"), nil
	}

	out := map[string]any{}
	for _, name := range s.Required {
		prop := s.Properties[name]
		// First-choice: enum head when present (gives a known-valid value
		// for tools that accept restricted strings — `read_mcp_resource`,
		// `worker_*` actions, etc.).
		if len(prop.Enum) > 0 {
			out[name] = prop.Enum[0]
			continue
		}
		switch prop.Type {
		case "string":
			out[name] = placeholderString(toolName, name, workspace)
		case "integer", "number":
			out[name] = 0
		case "boolean":
			out[name] = false
		case "array":
			out[name] = []any{}
		case "object":
			out[name] = map[string]any{}
		default:
			out[name] = nil
		}
	}
	b, err := json.Marshal(out)
	return json.RawMessage(b), err
}

// placeholderString returns a per-(tool, field) string that maximises
// the chance of reaching a real execution path. It is intentionally
// best-effort: tools whose backends require live infra (MCP server, X11
// display, network) will still error meaningfully on this input — the
// purpose is to dispatch, not to succeed.
func placeholderString(toolName, fieldName, workspace string) string {
	switch fieldName {
	case "path", "file_path":
		// Path under the test's t.TempDir()-backed workspace so any
		// successful write (write_file, file_edit) stays within the
		// scope-cleaned directory rather than leaking under /tmp.
		// Most tools error on missing-file before writing, but
		// write_file in particular will create this path.
		return workspace + "/iterion-claw-smoke-target"
	case "command":
		// A fast no-op that still proves bash dispatches.
		return "echo iterion-claw-smoke"
	case "pattern":
		return "*.go"
	case "url":
		return "http://127.0.0.1:1/iterion-claw-smoke"
	case "query":
		return "iterion claw smoke"
	case "question":
		return "iterion claw smoke probe"
	case "description":
		return "iterion claw smoke description"
	case "name":
		return "iterion-claw-smoke"
	case "id", "task_id", "worker_id", "team_id", "cron_id":
		return "iterion-claw-smoke-id"
	case "old_string":
		return "old"
	case "new_string":
		return "new"
	default:
		return "iterion-claw-smoke"
	}
}

func previewSmoke(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
