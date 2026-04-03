// Package protocol implements the NDJSON control protocol between the SDK and CLI.
package protocol

import "encoding/json"

// RawMessage is a raw NDJSON line with the "type" field pre-parsed.
type RawMessage struct {
	Type string
	Data json.RawMessage
}

// ParseLine extracts the "type" field from a raw NDJSON line.
func ParseLine(line []byte) (*RawMessage, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, err
	}
	// Make a copy of the line data since bufio.Scanner reuses its buffer.
	data := make([]byte, len(line))
	copy(data, line)
	return &RawMessage{
		Type: probe.Type,
		Data: json.RawMessage(data),
	}, nil
}

// IsControlRequest returns true if the line is a control_request from the CLI.
func IsControlRequest(rm *RawMessage) bool {
	return rm.Type == "control_request"
}

// IsControlResponse returns true if the line is a control_response from the CLI.
func IsControlResponse(rm *RawMessage) bool {
	return rm.Type == "control_response"
}

// IsMessage returns true if the line is a regular message (not control protocol).
func IsMessage(rm *RawMessage) bool {
	switch rm.Type {
	case "system", "assistant", "user", "result", "stream_event":
		return true
	}
	return false
}
