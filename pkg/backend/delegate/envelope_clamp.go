package delegate

import (
	"encoding/json"
	"fmt"
)

// The IPC channel is NDJSON, and its reader fails the WHOLE channel on a
// line over [MaxEnvelopeLineBytes] — so an oversize payload is not one
// lost message, it is a dead run. The runner→launcher direction (relayed
// events) clamps in pkg/backend/model; this is the launcher→runner half,
// where the payload the host cannot bound is a TOOL RESULT: an MCP call's
// result, a large file read, a `go test ./...` transcript.
//
// Same discipline as the relay: cut text and MARK the cut with the size
// that was produced, never emit a fragment of a document the peer parses.

// envelopeWrapperBytes covers `{"type":"tool_result","id":"…","data":}`
// plus the newline when sizing a payload against the line cap. Generous
// on purpose: the check exists to keep the channel alive, and a few
// hundred spare bytes cost nothing.
const envelopeWrapperBytes = 256

// ClampToolResult bounds a tool_result payload so it crosses one NDJSON
// line. Output and Error are cut to [MaxToolResultBytes] with a marker
// naming what was produced on the host — the model must be able to tell a
// short answer from one that was cut.
//
// The ask_user payload is NOT cut: its Conversation is the pre-pause LLM
// state the runner rebuilds a typed *ErrAskUser from, and a truncated one
// would resume onto a corrupted conversation. When that alone still
// overflows the line, the result becomes an explicit tool ERROR naming
// the size — the runner turns it into a Go error inside the LLM loop, so
// the node fails with a reason instead of the channel dying with none.
func ClampToolResult(data ToolResultData) ToolResultData {
	data.Output = clampToolText(data.Output)
	data.Error = clampToolText(data.Error)
	if toolResultFitsOneLine(data) {
		return data
	}
	convBytes := 0
	if data.AskUser != nil {
		convBytes = len(data.AskUser.Conversation)
	}
	return ToolResultData{
		Error: fmt.Sprintf(
			"iterion sandbox IPC: the ask_user payload is %d bytes of conversation, over the %d-byte channel line cap; it cannot be truncated without corrupting the resumed state, so the question could not be forwarded",
			convBytes, MaxEnvelopeLineBytes),
	}
}

// clampToolText cuts one text field and marks the cut.
func clampToolText(s string) string {
	if len(s) <= MaxToolResultBytes {
		return s
	}
	return fmt.Sprintf("%s\n[iterion sandbox IPC: cut to %d bytes, %d bytes were produced on the host]",
		truncate(s, MaxToolResultBytes), MaxToolResultBytes, len(s))
}

// toolResultFitsOneLine reports whether data, wrapped in its envelope,
// stays under the channel's line cap.
func toolResultFitsOneLine(data ToolResultData) bool {
	buf, err := json.Marshal(data)
	if err != nil {
		// An unmarshalable payload is refused by the caller's own
		// marshal; nothing to size here.
		return true
	}
	return len(buf)+envelopeWrapperBytes <= MaxEnvelopeLineBytes
}

// newToolResultData builds a tool_result envelope from an already-shaped
// payload, clamping it first. The single construction site for the type,
// so no producer can bypass the bound.
func newToolResultData(id string, data ToolResultData) (Envelope, error) {
	buf, err := json.Marshal(ClampToolResult(data))
	if err != nil {
		return Envelope{}, fmt.Errorf("delegate: marshal tool_result: %w", err)
	}
	return Envelope{Type: EnvelopeToolResult, ID: id, Data: buf}, nil
}
