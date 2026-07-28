package delegate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/piext"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The control channel between the iterion pi extension and this process.
//
// pi's extension-UI protocol is a CLOSED union — an extension cannot add a
// method — so the channel is tunnelled through two of its members:
// `ctx.ui.input` for request/response and `ctx.ui.notify` for one-way. Neither
// needs a listener, a port, or a token, and both work identically inside a
// sandbox where a network callback would have to cross the container boundary.
//
// The channel is SHARED with every other extension the operator has installed.
// So a request only counts as iterion's if its payload parses as an envelope
// carrying `__iterion`; anything else is a genuine dialog from a third-party
// extension and gets cancelled (its safe default) with a warning. Without that
// check a hostile or buggy extension could fabricate a permission verdict.

// piCtrlEnvelope is one request from the extension.
type piCtrlEnvelope struct {
	Iterion   int             `json:"__iterion"`
	V         int             `json:"v"`
	Op        string          `json:"op"`
	RunID     string          `json:"runId,omitempty"`
	NodeID    string          `json:"nodeId,omitempty"`
	Iteration int             `json:"iteration,omitempty"`
	Seq       int             `json:"seq"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// piCtrlReply is the answer, carried as the string value of the UI response.
type piCtrlReply struct {
	V     int    `json:"v"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// Control-channel operations.
const (
	piOpPermissionEvaluate = "permission.evaluate"
	piOpAskUser            = "ask_user"
)

// piPendingPause is a suspension the extension raised, waiting to be turned
// into an ErrAskUser once the turn unwinds.
//
// It cannot be raised inline: the control channel's handler runs on the
// client's dispatcher goroutine and must return a reply promptly, while a
// pause has to travel out through Execute's return value so the executor can
// persist an interaction and stop the run. So the handler records the pause,
// tells the extension it escalated, and Execute picks it up — the same shape
// as claude_code's hook, which cancels the stream and returns ErrAskUser.
type piPendingPause struct {
	question      string
	options       []AskUserOption
	allowFreeText bool
	toolUseID     string
	// permissionMarker is set when the pause is a permission escalation
	// rather than an LLM ask_user call: the studio then renders an approval
	// card instead of a question.
	permissionMarker map[string]any
}

// err renders the pause as the sentinel the executor's pause machinery keys on.
func (p *piPendingPause) err() error {
	return &ErrAskUser{
		Question:         p.question,
		Options:          p.options,
		AllowFreeText:    p.allowFreeText,
		PendingToolUseID: p.toolUseID,
		PermissionMarker: p.permissionMarker,
	}
}

// piAskUserRequest is the payload of an ask_user op.
type piAskUserRequest struct {
	Question      string          `json:"question"`
	Options       []AskUserOption `json:"options,omitempty"`
	AllowFreeText bool            `json:"allow_free_text,omitempty"`
	ToolUseID     string          `json:"tool_use_id,omitempty"`
}

// piParseAskUser decodes an ask_user op. A question is mandatory: pausing a
// run on an empty prompt would strand the operator with nothing to answer.
func piParseAskUser(data json.RawMessage) (*piPendingPause, error) {
	var req piAskUserRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("malformed ask_user request: %w", err)
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("ask_user needs a question")
	}
	return &piPendingPause{
		question:      req.Question,
		options:       req.Options,
		allowFreeText: req.AllowFreeText || len(req.Options) == 0,
		toolUseID:     req.ToolUseID,
	}, nil
}

// piParseCtrl decodes a UI request as an iterion control envelope. Reports
// false for anything that is not one — including a payload that merely looks
// like JSON — so a third-party extension's dialog is never mistaken for a
// control message.
func piParseCtrl(req pisdk.UIRequest) (piCtrlEnvelope, bool) {
	raw := strings.TrimSpace(req.Prompt())
	if raw == "" || raw[0] != '{' {
		return piCtrlEnvelope{}, false
	}
	var env piCtrlEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return piCtrlEnvelope{}, false
	}
	if env.Iterion != 1 || env.Op == "" {
		return piCtrlEnvelope{}, false
	}
	return env, true
}

// piCtrlAnswer marshals a reply into the UI response the extension awaits.
func piCtrlAnswer(id string, data any) *pisdk.UIResponse {
	body, err := json.Marshal(piCtrlReply{V: piext.CtrlVersion, OK: true, Data: data})
	if err != nil {
		return piCtrlFail(id, err.Error())
	}
	resp := pisdk.NewUIValue(id, string(body))
	return &resp
}

// piCtrlFail answers with an explicit failure. The extension applies its own
// fail-safe — for the permission gate, blocking.
func piCtrlFail(id, reason string) *pisdk.UIResponse {
	body, _ := json.Marshal(piCtrlReply{V: piext.CtrlVersion, OK: false, Error: reason})
	resp := pisdk.NewUIValue(id, string(body))
	return &resp
}

// piPermissionRequest is the payload of a permission.evaluate op.
type piPermissionRequest struct {
	Tool  string         `json:"tool"`
	Input map[string]any `json:"input"`
}

// piPermissionVerdict is what the extension acts on.
type piPermissionVerdict struct {
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Escalated bool   `json:"escalated,omitempty"`
}

// piEvaluatePermission answers a permission.evaluate op from the task's policy.
//
// The decision is made HERE, by the same permission.Policy that drives
// claude_code's PreToolUse hook and claw's tool gate, so the three backends
// reach identical verdicts for the same workflow. Re-implementing the rule
// parser and glob matcher in the extension's TypeScript would be a second
// implementation guaranteed to drift.
//
// `ask` is reported as an escalation: the caller pauses the run for a human,
// which the extension cannot do from inside pi.
func piEvaluatePermission(task Task, data json.RawMessage) (verdict piPermissionVerdict, marker map[string]any) {
	var req piPermissionRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Tool == "" {
		// Malformed: fail closed. A gate that waves through what it cannot
		// read is not a gate.
		return piPermissionVerdict{
			Decision: "deny",
			Reason:   "iterion could not read the permission request for this call",
		}, nil
	}

	decision, rule := task.Permission.Evaluate(req.Tool, req.Input)
	switch decision {
	case permission.Allow:
		return piPermissionVerdict{Decision: "allow"}, nil
	case permission.Ask:
		return piPermissionVerdict{
			Decision:  "deny",
			Escalated: true,
			Rule:      rule,
			Reason:    fmt.Sprintf("%s needs operator approval", req.Tool),
		}, permission.Marker(req.Tool, req.Input, rule)
	default:
		return piPermissionVerdict{
			Decision: "deny",
			Rule:     rule,
			Reason:   fmt.Sprintf("%s is denied by this workflow's permission policy", req.Tool),
		}, nil
	}
}
