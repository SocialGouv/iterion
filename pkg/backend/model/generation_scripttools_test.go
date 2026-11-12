package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The claim of CM-1b is that a call a SCRIPT makes is the same call the model
// would have made. These tests drive the two paths against one dispatcher and
// compare the verdicts, rather than asserting that the server "goes through"
// the funnel — which is a claim about code shape and not about behaviour.

// A denied call is denied identically whether the model asked or a script did.
// The gate is the anti-prompt-injection boundary, and a script is exactly the
// surface a prompt injection would reach for.
func TestAScriptsCallMeetsTheSameGateVerdictAsTheModels(t *testing.T) {
	t.Parallel()

	pol, err := permission.NewPolicy(permission.ModeAsk, []string{"Bash(go test:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, command string
		wantRun       bool
		wantInResult  string
	}{
		{"a denied command", "rm -rf /", false, "denied"},
		{"an allow-listed command", "go test ./...", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The DIRECT path: the model's own block through the door.
			var directRan bool
			dd, dctx := newToolDispatcher(context.Background(), bashTool(&directRan), nil, nil, nil, nil, pol)
			directBlock, abort := dd.dispatch(dctx, bashCall(tc.command)[0])
			if abort != nil {
				t.Fatalf("direct: unexpected abort: %v", abort)
			}

			// The NESTED path: the same command, asked for by a script.
			var nestedRan bool
			nd, nctx := newToolDispatcher(context.Background(), bashTool(&nestedRan), nil, nil, nil, nil, pol)
			srv, err := newScriptToolServer(nctx, "")
			if err != nil {
				t.Fatalf("building the server from the node's own ctx: %v", err)
			}
			_ = nd // the server holds it, taken from the ctx
			out, abort := srv.CallTool(context.Background(),
				"Bash", json.RawMessage(`{"command":`+quote(tc.command)+`}`))
			if abort != nil {
				t.Fatalf("nested: unexpected abort: %v", abort)
			}

			if nestedRan != directRan {
				t.Errorf("the tool ran=%v directly and ran=%v from a script: the gate decided differently for the two paths",
					directRan, nestedRan)
			}
			if nestedRan != tc.wantRun {
				t.Errorf("the tool ran=%v from a script, want %v", nestedRan, tc.wantRun)
			}
			if out.IsError == tc.wantRun {
				t.Errorf("the script's outcome reports isError=%v for a call that should have run=%v", out.IsError, tc.wantRun)
			}
			// The TEXT the script reads is the text the model would have read.
			if want := toolResultText(directBlock); out.Text != want {
				t.Errorf("the script read %q and the model would have read %q", out.Text, want)
			}
			if tc.wantInResult != "" && !strings.Contains(out.Text, tc.wantInResult) {
				t.Errorf("the script's result %q does not carry %q", out.Text, tc.wantInResult)
			}
		})
	}
}

// A server can only be built from the ctx of a node's own tool call. Anything
// else is an ERROR and not a server that refuses: a script host handed a
// refuse-everything server would look like a tool-less node rather than a
// wiring mistake, and the mistake would ship.
func TestAServerCannotBeBuiltWithoutTheNodesOwnDoor(t *testing.T) {
	t.Parallel()

	if _, err := newScriptToolServer(context.Background(), ""); err == nil {
		t.Fatal("a bare context produced a tool server: a script would have borrowed a door that does not exist")
	}

	// And the door a server holds is the one its ctx vouches for. A server
	// built from one node's ctx cannot serve while another node's dispatcher
	// is the owner: dispatch compares by POINTER, so the refusal is silent —
	// an isError tool_result, not an error — which is why this is asserted
	// rather than assumed.
	var ran bool
	_, ctxA := newToolDispatcher(context.Background(), bashTool(&ran), nil, nil, nil, nil, allowAllPolicy(t))
	srv, err := newScriptToolServer(ctxA, "")
	if err != nil {
		t.Fatal(err)
	}
	_, ctxB := newToolDispatcher(context.Background(), bashTool(&ran), nil, nil, nil, nil, allowAllPolicy(t))
	srv.ctx = ctxB // what a cached server across model steps would amount to

	out, abort := srv.CallTool(context.Background(), "Bash", json.RawMessage(`{"command":"go test ./..."}`))
	if abort != nil {
		t.Fatalf("unexpected abort: %v", abort)
	}
	if !out.IsError || !strings.Contains(out.Text, "did not come from this node's dispatcher") {
		t.Errorf("a server driven with another node's context was served: %+v", out)
	}
	if ran {
		t.Error("the tool RAN for a context the door could not vouch for")
	}
}

// The withheld class is keyed on the namespace, not on a list of spellings: the
// same tool reaches the funnel under four names depending on the backend, and a
// list would have to carry every one.
func TestTheWithheldClassCannotBeWalkedAroundBySpelling(t *testing.T) {
	t.Parallel()

	var ran bool
	_, ctx := newToolDispatcher(context.Background(), bashTool(&ran), nil, nil, nil, nil, allowAllPolicy(t))
	srv, err := newScriptToolServer(ctx, ScriptHostToolName)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"ask_user", "askUser", "ASK_USER", // the pause family, every case
		"send_user_message",                     // permission-EXEMPT, so the budget would be its only bound
		"structured_output", "StructuredOutput", // how the node returns its own result
		"mcp__iterion__ask_user",   // claude_code's spelling
		"mcp_iterion_board_create", // claw's
		"mcp.iterion_board.create", // the dotted one
		"agent", "task",            // a sub-agent is CM-13, with its own reserve
		ScriptHostToolName, "CODEMODE", // and a script cannot nest a script
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, abort := srv.CallTool(context.Background(), name, nil)
			if abort != nil {
				t.Fatalf("a withheld tool aborted the script instead of refusing: %v", abort)
			}
			if !out.IsError {
				t.Fatalf("tool %q was served to a script: %+v", name, out)
			}
			// The refusal SAYS what to do instead. A refusal a model cannot
			// act on makes it retry the same call.
			if !strings.Contains(out.Text, "directly") && !strings.Contains(out.Text, "another script") {
				t.Errorf("the refusal for %q does not say what to do instead: %q", name, out.Text)
			}
		})
	}
}

// An absent or empty input becomes the empty object, because a tool's own
// input validation reads it as JSON and would otherwise blame the tool for a
// parse error the script caused by passing nothing.
func TestANoArgumentCallArrivesAsAnEmptyObject(t *testing.T) {
	t.Parallel()

	for _, in := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  ")} {
		if got := string(normaliseInput(in)); got != "{}" {
			t.Errorf("an input of %q normalised to %q, want {}", in, got)
		}
	}
	if got := string(normaliseInput(json.RawMessage(`{"a":1}`))); got != `{"a":1}` {
		t.Errorf("a real input was rewritten to %q", got)
	}
}

// toolResultText must survive the one shape the type allows that the happy
// path never produces: the ZERO ContentBlock the funnel's abort path returns,
// whose Content is nil. Indexing [0] would panic the pump goroutine on it.
func TestTheResultTextSurvivesTheShapesTheFunnelCanReturn(t *testing.T) {
	t.Parallel()

	if got := toolResultText(api.ContentBlock{}); got != "" {
		t.Errorf("the zero block yielded %q, want the empty string", got)
	}
}

func allowAllPolicy(t *testing.T) *permission.Policy {
	t.Helper()
	pol, err := permission.NewPolicy(permission.ModeAsk, []string{"Bash(go test:*)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Discovery must answer for the set the script will actually be SERVED, not
// for the node's whole one. tool_search reads the executable-tool pin, so an
// unnarrowed pin would tell a script `ask_user` is available right before the
// server refused it — and a model told a tool exists retries it.
//
// The pin is observed from inside a tool's own Execute, which is where
// tool_search reads it, rather than from the ctx the test built: that is the
// only place the door's derivation has actually happened.
func TestDiscoveryAnswersWithTheSetTheScriptWillBeServed(t *testing.T) {
	t.Parallel()

	var directPin, nestedPin map[string]bool
	tools := func(seen *map[string]bool) map[string]*GenerationTool {
		probe := func(ctx context.Context, _ json.RawMessage) (string, error) {
			set, ok := tool.NodeExecutableToolsFrom(ctx)
			if !ok {
				return "", errNoPin
			}
			*seen = set
			return "ran", nil
		}
		return map[string]*GenerationTool{
			"Bash":      {Name: "Bash", Execute: probe},
			"ask_user":  {Name: "ask_user", Execute: probe},
			"agent":     {Name: "agent", Execute: probe},
			"read_file": {Name: "read_file", Execute: probe},
		}
	}

	// DIRECT: the model's own call sees the node's whole set, unchanged.
	dd, dctx := newToolDispatcher(context.Background(), tools(&directPin), nil, nil, nil, nil, allowAllPolicy(t))
	if _, abort := dd.dispatch(dctx, bashCall("go test ./...")[0]); abort != nil {
		t.Fatalf("direct: %v", abort)
	}
	for _, want := range []string{"Bash", "ask_user", "agent", "read_file"} {
		if !directPin[want] {
			t.Errorf("the direct path's pin lost %q: narrowing a script must not narrow the model", want)
		}
	}

	// NESTED: the script's call sees only what the server will serve.
	_, nctx := newToolDispatcher(context.Background(), tools(&nestedPin), nil, nil, nil, nil, allowAllPolicy(t))
	srv, err := newScriptToolServer(nctx, ScriptHostToolName)
	if err != nil {
		t.Fatal(err)
	}
	if out, abort := srv.CallTool(context.Background(), "Bash", json.RawMessage(`{"command":"go test ./..."}`)); abort != nil {
		t.Fatalf("nested: %v", abort)
	} else if out.IsError {
		t.Fatalf("nested: %+v", out)
	}
	for _, want := range []string{"Bash", "read_file"} {
		if !nestedPin[want] {
			t.Errorf("the script's pin lost %q, which the server WOULD serve: discovery under-reports", want)
		}
	}
	for _, gone := range []string{"ask_user", "agent"} {
		if nestedPin[gone] {
			t.Errorf("the script's pin still offers %q, which the server refuses: discovery and the gate disagree", gone)
		}
	}
}

var errNoPin = errors.New("no executable-tool pin on the context the door handed this tool")
