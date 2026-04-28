package api

import (
	"bytes"
	"fmt"
	"strings"
)

// SseParser is a byte-buffer state machine for parsing Server-Sent Events.
// It accumulates bytes via Push, scans for frame delimiters, extracts complete
// frames, and deserializes them into StreamEvent values.
type SseParser struct {
	buffer   []byte
	provider string // for error context
	model    string // for error context
}

// SseFrame is a raw parsed SSE frame before JSON deserialization.
type SseFrame struct {
	EventType string // from "event:" line, empty if not present
	Data      string // joined "data:" lines
}

// NewSseParser creates a new SSE parser with an empty buffer.
func NewSseParser() *SseParser {
	return &SseParser{}
}

// WithContext sets provider and model for richer error messages.
func (p *SseParser) WithContext(provider, model string) *SseParser {
	p.provider = provider
	p.model = model
	return p
}

// Push appends a chunk of bytes to the buffer and returns any complete
// StreamEvents that can be parsed from the accumulated data.
func (p *SseParser) Push(chunk []byte) ([]StreamEvent, error) {
	p.buffer = append(p.buffer, chunk...)
	return p.drainFrames()
}

// Finish flushes any remaining data in the buffer as a final frame.
// This handles the case where the stream ends without a trailing delimiter.
func (p *SseParser) Finish() ([]StreamEvent, error) {
	if len(bytes.TrimSpace(p.buffer)) == 0 {
		p.buffer = nil
		return nil, nil
	}
	// Append a delimiter so parseFrames can extract it
	p.buffer = append(p.buffer, '\n', '\n')
	return p.drainFrames()
}

// drainFrames extracts all complete frames and converts them to events.
func (p *SseParser) drainFrames() ([]StreamEvent, error) {
	frames := p.parseFrames()
	if len(frames) == 0 {
		return nil, nil
	}

	var events []StreamEvent
	for _, frame := range frames {
		ev, err := p.frameToEvent(frame)
		if err != nil {
			return events, err
		}
		if ev != nil {
			events = append(events, *ev)
		}
	}
	return events, nil
}

// parseFrames scans the buffer for complete frames delimited by a blank line
// (\n\n or \r\n\r\n), removes them from the buffer, and returns the raw frames.
func (p *SseParser) parseFrames() []SseFrame {
	var frames []SseFrame

	for {
		pos, dlen := findFrameDelimiter(p.buffer)
		if pos < 0 {
			break
		}

		raw := p.buffer[:pos]
		p.buffer = p.buffer[pos+dlen:]

		frame := p.parseFrame(raw)
		if frame != nil {
			frames = append(frames, *frame)
		}
	}

	return frames
}

// findFrameDelimiter finds the first blank-line delimiter in buf.
// Returns (position, delimiter length) or (-1, 0) if not found.
// Recognizes \n\n, \r\n\r\n, and \n\r\n patterns.
func findFrameDelimiter(buf []byte) (int, int) {
	for i := 0; i < len(buf)-1; i++ {
		if buf[i] == '\n' {
			if buf[i+1] == '\n' {
				return i, 2
			}
			if buf[i+1] == '\r' && i+2 < len(buf) && buf[i+2] == '\n' {
				return i, 3
			}
		}
		if buf[i] == '\r' && i+3 < len(buf) &&
			buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n' {
			return i, 4
		}
	}
	return -1, 0
}

// parseFrame parses the lines of a single raw frame into an SseFrame.
// Returns nil if the frame contains no data lines.
func (p *SseParser) parseFrame(frame []byte) *SseFrame {
	lines := bytes.Split(frame, []byte("\n"))

	var eventType string
	var dataLines []string

	for _, line := range lines {
		// Trim \r for \r\n line endings
		line = bytes.TrimRight(line, "\r")

		// Skip empty lines within a frame
		if len(line) == 0 {
			continue
		}

		// Skip comment lines (start with ':')
		if line[0] == ':' {
			continue
		}

		str := string(line)

		if strings.HasPrefix(str, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(str, "event:"))
		} else if strings.HasPrefix(str, "data:") {
			data := strings.TrimPrefix(str, "data:")
			// SSE spec: if there's a space after "data:", strip exactly one space
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			dataLines = append(dataLines, data)
		}
		// Other fields (id:, retry:) are ignored for our purposes
	}

	if len(dataLines) == 0 && eventType == "" {
		return nil
	}

	return &SseFrame{
		EventType: eventType,
		Data:      strings.Join(dataLines, "\n"),
	}
}

// frameToEvent converts a parsed SseFrame into a StreamEvent by deserializing
// the JSON data. Returns nil for frames that should be filtered (ping, [DONE]).
func (p *SseParser) frameToEvent(frame SseFrame) (*StreamEvent, error) {
	// Filter ping events
	if frame.EventType == "ping" {
		return nil, nil
	}

	// Filter [DONE] sentinel
	if frame.Data == "[DONE]" {
		return nil, nil
	}

	// Skip frames with no data
	if frame.Data == "" {
		return nil, nil
	}

	event, err := parseSSEData(frame.Data)
	if err != nil {
		ctx := ""
		if p.provider != "" || p.model != "" {
			ctx = fmt.Sprintf(" (provider=%s, model=%s)", p.provider, p.model)
		}
		return nil, fmt.Errorf("SSE parse error%s: %w", ctx, err)
	}

	// Also filter ping by parsed type (belt and suspenders)
	if event.Type == EventPing {
		return nil, nil
	}

	return &event, nil
}
