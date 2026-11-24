package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCodemodeStateReplyRequiresCorrelationAndExplicitOutcome(t *testing.T) {
	reply, err := stateEnvelope(EnvelopeStateReply, "state-1", StateReply{Op: StateLoad, Code: StateOK, Found: true, Value: []byte(`null`)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeStateReply(reply, "state-1", StateLoad); err != nil || !got.Found || string(got.Value) != "null" {
		t.Fatalf("a valid correlated load did not return null: found=%v err=%v", got.Found, err)
	}
	if _, err := DecodeStateReply(reply, "state-2", StateLoad); err == nil {
		t.Fatal("a reply to another request was accepted")
	}
	if _, err := DecodeStateReply(reply, "state-1", StateStore); err == nil {
		t.Fatal("a load reply was accepted as write acknowledgement")
	}
	reply.Data = json.RawMessage(`{}`)
	if _, err := DecodeStateReply(reply, "state-1", StateLoad); err == nil {
		t.Fatal("a reply without an explicit outcome was accepted")
	}
}

func TestCodemodeStateRequestRejectsOutOfContractFrames(t *testing.T) {
	for name, data := range map[string]string{
		"extra fields":     `{"op":"load","key":"item","note":"ignored?"}`,
		"over payload cap": `{"op":"load","key":"item"` + strings.Repeat(" ", maxStatePayload) + `}`,
		"load with value":  `{"op":"load","key":"item","value":"bnVsbA=="}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := Envelope{Type: EnvelopeStateRequest, ID: "state-1", Data: json.RawMessage(data)}
			if _, err := decodeStateRequest(env); err == nil {
				t.Fatal("invalid state request was accepted")
			}
		})
	}
}

func TestCodemodeStateMalformedEnvelopeDoesNotExcerptPayload(t *testing.T) {
	const fixture = "benign sample value"
	mux := NewMultiplexer(strings.NewReader(`{"type":"codemode_state_request","data":"`+fixture+`",,}`+"\n"), nil,
		MultiplexerHandler{OnCodemodeState: func(context.Context, StateRequest) (StateReply, error) {
			t.Error("a malformed frame must never dispatch")
			return StateReply{}, nil
		}})
	if _, err := mux.Run(t.Context()); err == nil || strings.Contains(err.Error(), fixture) {
		t.Fatal("expected a malformed private frame refusal without the raw sample")
	}
}
