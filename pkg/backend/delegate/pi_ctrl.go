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
	piOpAskUserAsync       = "ask_user_async"
	piOpAwaitAnswers       = "await_answers"
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
	// awaitPending is set when the pause is an await_answers escalation
	// (ADR-081): the agent chose to block on questions it had already
	// posted. The refs let the pause list them and the resume path fan the
	// operator's answers back out.
	awaitPending []PendingAsync
}

// err renders the pause as the sentinel the executor's pause machinery keys on.
func (p *piPendingPause) err() error {
	return &ErrAskUser{
		Question:         p.question,
		Options:          p.options,
		AllowFreeText:    p.allowFreeText,
		PendingToolUseID: p.toolUseID,
		PermissionMarker: p.permissionMarker,
		AwaitPending:     p.awaitPending,
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

// piAsyncPosted is the answer to an ask_user_async op.
//
// Message carries the text the tool must return to the model. It is filled
// from delegate.AsyncQuestionPostedText rather than written in the extension:
// identical prompting across backends is an ADR-081 goal, so the wording has
// exactly one home.
type piAsyncPosted struct {
	InteractionID string `json:"interactionId"`
	Message       string `json:"message"`
}

// piAwaitResult is the answer to an await_answers op — one of two shapes.
//
// Nothing pending: Answers carries the collected replies and the agent simply
// continues, having paid nothing. Otherwise Escalated is set, the run pauses,
// and Pending lists what it is waiting on.
type piAwaitResult struct {
	Answers   string         `json:"answers,omitempty"`
	Escalated bool           `json:"escalated,omitempty"`
	Pending   []piPendingRef `json:"pending,omitempty"`
}

// piPendingRef is the wire shape of a pending question. PendingAsync carries
// no JSON tags, so marshalling it directly would put Go field names on a
// documented contract.
type piPendingRef struct {
	InteractionID string `json:"interactionId"`
	Question      string `json:"question"`
}

func piPendingRefs(pending []PendingAsync) []piPendingRef {
	out := make([]piPendingRef, 0, len(pending))
	for _, p := range pending {
		// Same fields, different JSON tags — which is the whole point, and
		// why the conversion is legal.
		out = append(out, piPendingRef(p))
	}
	return out
}

// piPostAsyncQuestion answers an ask_user_async op: persist the question and
// let the agent keep working. It never pauses — that is the whole point of the
// async pair, and the reason a node can front-load its questions.
func piPostAsyncQuestion(task Task, data json.RawMessage) (piAsyncPosted, error) {
	if task.PostAsyncQuestion == nil {
		return piAsyncPosted{}, fmt.Errorf("async questions are not enabled for this node")
	}
	req, err := piParseAskUser(data)
	if err != nil {
		return piAsyncPosted{}, err
	}
	id, err := task.PostAsyncQuestion(AsyncQuestion{
		Question:      req.question,
		Options:       req.options,
		AllowFreeText: req.allowFreeText,
	})
	if err != nil {
		return piAsyncPosted{}, fmt.Errorf("could not post the question: %w", err)
	}
	return piAsyncPosted{InteractionID: id, Message: AsyncQuestionPostedText}, nil
}

// piAwaitAnswers answers an await_answers op.
//
// The two outcomes are deliberately asymmetric. Everything answered is the
// cheap, common case and must not cost a pause — the agent gets the answers
// inline and continues in the same turn. Something still pending is the real
// sync point: the run suspends, which only the caller can do.
func piAwaitAnswers(task Task) (piAwaitResult, *piPendingPause, error) {
	if task.PendingAsyncQuestions == nil || task.CollectAsyncAnswers == nil {
		return piAwaitResult{}, nil, fmt.Errorf("async questions are not enabled for this node")
	}
	pending, err := task.PendingAsyncQuestions()
	if err != nil {
		return piAwaitResult{}, nil, fmt.Errorf("could not read the pending questions: %w", err)
	}
	if len(pending) == 0 {
		answers, err := task.CollectAsyncAnswers()
		if err != nil {
			return piAwaitResult{}, nil, fmt.Errorf("could not read the answers: %w", err)
		}
		return piAwaitResult{Answers: answers}, nil, nil
	}
	return piAwaitResult{Escalated: true, Pending: piPendingRefs(pending)},
		&piPendingPause{question: AwaitPauseQuestion(pending), awaitPending: pending},
		nil
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
