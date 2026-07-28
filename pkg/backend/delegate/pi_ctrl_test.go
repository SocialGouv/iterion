package delegate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/piext"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The UI channel is shared with every extension the operator installed, so
// mistaking a third-party dialog for a control message would let a hostile or
// buggy extension fabricate a permission verdict.
func TestPiParseCtrl(t *testing.T) {
	t.Run("accepts a marked envelope", func(t *testing.T) {
		req := pisdk.UIRequest{Method: pisdk.UIMethodInput, ID: "1",
			Title: `{"__iterion":1,"v":1,"op":"permission.evaluate","seq":3,"data":{"tool":"Bash"}}`}
		env, ok := piParseCtrl(req)
		if !ok {
			t.Fatal("a properly marked envelope was rejected")
		}
		if env.Op != piOpPermissionEvaluate || env.Seq != 3 {
			t.Errorf("env = %+v, want the op and seq decoded", env)
		}
	})

	t.Run("rejects anything unmarked", func(t *testing.T) {
		for name, title := range map[string]string{
			"plain prose":       "Allow this dangerous command?",
			"json without mark": `{"op":"permission.evaluate","data":{}}`,
			"wrong mark":        `{"__iterion":2,"op":"permission.evaluate"}`,
			"marked but no op":  `{"__iterion":1,"v":1}`,
			"empty":             "",
			"not json":          "__iterion",
		} {
			if _, ok := piParseCtrl(pisdk.UIRequest{Method: pisdk.UIMethodInput, ID: "x", Title: title}); ok {
				t.Errorf("%s: accepted %q as a control message", name, title)
			}
		}
	})

	// confirm carries its text in Message rather than Title.
	t.Run("reads the confirm variant's field", func(t *testing.T) {
		req := pisdk.UIRequest{Method: pisdk.UIMethodConfirm, ID: "1",
			Message: `{"__iterion":1,"v":1,"op":"permission.evaluate"}`}
		if _, ok := piParseCtrl(req); !ok {
			t.Error("a control envelope on a confirm request was missed")
		}
	})
}

func TestPiCtrlReplyShape(t *testing.T) {
	resp := piCtrlAnswer("req-1", piPermissionVerdict{Decision: "allow"})
	if resp == nil || resp.ID != "req-1" {
		t.Fatalf("resp = %+v, want a value response correlated to req-1", resp)
	}
	var reply piCtrlReply
	if err := json.Unmarshal([]byte(resp.Value), &reply); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if !reply.OK || reply.V != piext.CtrlVersion {
		t.Errorf("reply = %+v, want ok with the current version", reply)
	}

	fail := piCtrlFail("req-2", "boom")
	var freply piCtrlReply
	if err := json.Unmarshal([]byte(fail.Value), &freply); err != nil {
		t.Fatal(err)
	}
	if freply.OK || freply.Error != "boom" {
		t.Errorf("fail reply = %+v, want ok=false carrying the reason", freply)
	}
}

func TestPiEvaluatePermission(t *testing.T) {
	mustPolicy := func(mode permission.Mode, allow []string) *permission.Policy {
		t.Helper()
		p, err := permission.NewPolicy(mode, allow, nil, nil)
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		return p
	}
	deny := mustPolicy(permission.ModeDeny, nil)
	ask := mustPolicy(permission.ModeAsk, nil)
	// nil is the "no gate configured" shape — Policy.Enabled() tolerates it.
	var off *permission.Policy

	t.Run("off allows", func(t *testing.T) {
		v, marker := piEvaluatePermission(Task{Permission: off},
			json.RawMessage(`{"tool":"Bash","input":{"command":"rm -rf /"}}`))
		if v.Decision != "allow" || marker != nil {
			t.Errorf("got %+v marker=%v, want a plain allow when no gate is configured", v, marker)
		}
	})

	t.Run("deny blocks with the reason", func(t *testing.T) {
		v, marker := piEvaluatePermission(Task{Permission: deny},
			json.RawMessage(`{"tool":"Bash","input":{"command":"ls"}}`))
		if v.Decision != "deny" || marker != nil {
			t.Errorf("got %+v marker=%v, want deny without escalation", v, marker)
		}
		if !strings.Contains(v.Reason, "Bash") {
			t.Errorf("reason %q does not name the tool, so the model cannot adapt", v.Reason)
		}
	})

	// `ask` cannot be answered from inside pi — the extension has no operator.
	// It is reported as an escalation so the host pauses the run, and blocked
	// meanwhile so the tool does not run in that window.
	t.Run("ask escalates and blocks", func(t *testing.T) {
		v, marker := piEvaluatePermission(Task{Permission: ask},
			json.RawMessage(`{"tool":"Bash","input":{"command":"ls"}}`))
		if marker == nil {
			t.Fatal("ask must return a permission marker so the pause renders as an approval card")
		}
		if marker["tool"] != "Bash" {
			t.Errorf("marker = %v, want it to carry the tool for the approval card", marker)
		}
		if v.Decision != "deny" || !v.Escalated {
			t.Errorf("got %+v, want the call blocked while the escalation lands", v)
		}
	})

	// A gate that waves through what it cannot read is not a gate.
	t.Run("malformed input fails closed", func(t *testing.T) {
		for _, bad := range []string{`{}`, `not json`, `{"tool":""}`, ``} {
			v, _ := piEvaluatePermission(Task{Permission: deny}, json.RawMessage(bad))
			if v.Decision != "deny" {
				t.Errorf("input %q → %+v, want deny (fail closed)", bad, v)
			}
		}
	})

	// The same policy object drives claude_code's hook and claw's gate; a
	// scoped rule must resolve identically here.
	t.Run("honours a scoped allow rule", func(t *testing.T) {
		pol := mustPolicy(permission.ModeDeny, []string{"Bash(ls:*)"})
		v, _ := piEvaluatePermission(Task{Permission: pol},
			json.RawMessage(`{"tool":"Bash","input":{"command":"ls -la"}}`))
		if v.Decision != "allow" {
			t.Errorf("scoped rule Bash(ls:*) did not allow `ls -la`: %+v", v)
		}
	})
}

// The extension asset must be embedded, self-contained, and land somewhere a
// sandboxed run can actually read.
func TestPiExtAsset(t *testing.T) {
	body, err := piext.Asset()
	if err != nil {
		t.Fatalf("embedded asset missing — the build did not run: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("embedded asset is empty")
	}
	// esbuild must have inlined everything: a runtime import would fail inside
	// the sandbox, where there is no node_modules.
	for _, forbidden := range []string{`from "@earendil-works/`, `require("@earendil-works/`} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("asset has a runtime dependency (%s) — it cannot load without node_modules", forbidden)
		}
	}
	// The contract version the extension checks must match the Go constant, or
	// every run silently loses its capabilities.
	if !strings.Contains(string(body), `"`+piext.ContractVersion+`"`) {
		t.Errorf("asset does not carry contract version %q — rebuild it", piext.ContractVersion)
	}

	t.Run("materialises under the workspace and cleans up", func(t *testing.T) {
		dir := t.TempDir()
		path, cleanup, err := piext.Materialise(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(path, dir) {
			t.Errorf("path %q escapes the workspace — a container could not read it", path)
		}
		cleanup()
	})

	t.Run("requires a workspace", func(t *testing.T) {
		if _, _, err := piext.Materialise(""); err == nil {
			t.Error("expected an error with no WorkDir")
		}
	})
}

func TestPiExtensionEnv(t *testing.T) {
	t.Run("contract and identity always present", func(t *testing.T) {
		env := piExtensionEnv(Task{NodeID: "n1", Iteration: 2})
		if env["ITERION_PI_CONTRACT"] != piext.ContractVersion {
			t.Errorf("contract = %q, want %q", env["ITERION_PI_CONTRACT"], piext.ContractVersion)
		}
		if env["ITERION_PI_NODE_ID"] != "n1" || env["ITERION_PI_ITERATION"] != "2" {
			t.Errorf("identity missing from %v", env)
		}
	})

	// No gate configured → no variable → the extension registers no hook, so a
	// node without a gate pays no per-tool-call round-trip.
	t.Run("permission absent when no gate is configured", func(t *testing.T) {
		if _, ok := piExtensionEnv(Task{})["ITERION_PI_PERMISSION"]; ok {
			t.Error("ITERION_PI_PERMISSION set with no gate — the hook would cost a round-trip per call")
		}
	})

	t.Run("permission mode forwarded when set", func(t *testing.T) {
		pol, err := permission.NewPolicy(permission.ModeDeny, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		env := piExtensionEnv(Task{Permission: pol})
		if env["ITERION_PI_PERMISSION"] != "deny" {
			t.Errorf("ITERION_PI_PERMISSION = %q, want deny", env["ITERION_PI_PERMISSION"])
		}
	})

	// Secret values must never ride the extension's configuration surface.
	t.Run("carries no secret-looking values", func(t *testing.T) {
		for k, v := range piExtensionEnv(Task{NodeID: "n", Iteration: 0}) {
			if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "key") {
				t.Errorf("%s=%q looks like a credential on the extension env surface", k, v)
			}
		}
	})
}
