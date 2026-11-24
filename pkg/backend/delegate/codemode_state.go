package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

const (
	// EnvelopeCodemodeStateV1 is an empty launcher-to-runner marker sent
	// before a task that requires the private bank. Old runners reject an
	// unknown pre-task marker rather than silently omitting the capability.
	EnvelopeCodemodeStateV1 EnvelopeType = "codemode_state_v1"
	EnvelopeStateRequest    EnvelopeType = "codemode_state_request"
	EnvelopeStateReply      EnvelopeType = "codemode_state_reply"

	StateStore       = "store"
	StateLoad        = "load"
	StateOK          = "ok"
	StateUnavailable = "unavailable"
	StateInvalid     = "invalid"
	StateQuota       = "quota"

	// Values are base64 at this hop so JSON marshal cannot re-escape the
	// stored document and grow or change the bytes being accounted for.
	maxStatePayload     = (scripthost.MaxStateValueBytes+2)/3*4 + 6*scripthost.MaxStateKeyBytes + 256
	StateRequestTimeout = 5 * time.Second
)

var (
	errStateRequest = errors.New("delegate: invalid codemode state request")
	errStateReply   = errors.New("delegate: invalid codemode state reply")
)

type StateRequest struct {
	Op               string `json:"op"`
	Key              string `json:"key"`
	Value            []byte `json:"value,omitempty"`
	DeadlineUnixNano int64  `json:"deadline_unix_nano,omitempty"`
}

// LauncherDeadline lets the runner shorten, but never extend, the relay budget.
func (r StateRequest) LauncherDeadline(now time.Time) time.Time {
	limit := now.Add(StateRequestTimeout)
	if r.DeadlineUnixNano != 0 {
		if runner := time.Unix(0, r.DeadlineUnixNano); runner.Before(limit) {
			return runner
		}
	}
	return limit
}

type StateReply struct {
	Op    string `json:"op"`
	Found bool   `json:"found,omitempty"`
	Value []byte `json:"value,omitempty"`
	Code  string `json:"code"`
}

func validStateValue(value []byte) bool {
	return len(value) <= scripthost.MaxStateValueBytes && utf8.Valid(value) && json.Valid(value)
}

func validStateID(id string) bool {
	return len(id) > 0 && len(id) <= 64
}

func (r StateRequest) valid() bool {
	return r.Key != "" && len(r.Key) <= scripthost.MaxStateKeyBytes && utf8.ValidString(r.Key) &&
		((r.Op == StateStore && validStateValue(r.Value)) || (r.Op == StateLoad && r.Value == nil))
}

func (r StateReply) valid(op string) bool {
	switch r.Code {
	case StateUnavailable, StateInvalid, StateQuota:
		return !r.Found && r.Value == nil
	case StateOK:
		if r.Found {
			return op == StateLoad && validStateValue(r.Value)
		}
		return r.Value == nil
	default:
		return false
	}
}

func NewStateRequest(id string, req StateRequest) (Envelope, error) {
	if !validStateID(id) || !req.valid() {
		return Envelope{}, errStateRequest
	}
	return stateEnvelope(EnvelopeStateRequest, id, req)
}

func decodeStateRequest(env Envelope) (StateRequest, error) {
	var req StateRequest
	if !decodeStatePayload(env, &req) || !req.valid() {
		return StateRequest{}, errStateRequest
	}
	return req, nil
}

func DecodeStateReply(env Envelope, id, op string) (StateReply, error) {
	var reply StateReply
	if env.Type != EnvelopeStateReply || env.ID != id || (op != StateStore && op != StateLoad) ||
		!decodeStatePayload(env, &reply) || reply.Op != op || !reply.valid(op) {
		return StateReply{}, errStateReply
	}
	return reply, nil
}

func decodeStatePayload(env Envelope, dst any) bool {
	if !validStateID(env.ID) || len(env.Data) > maxStatePayload || !utf8.Valid(env.Data) || !json.Valid(env.Data) || len(env.Data) == 0 || env.Data[0] != '{' {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(env.Data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst) == nil
}

func stateEnvelope(kind EnvelopeType, id string, payload any) (Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > maxStatePayload {
		return Envelope{}, errors.New("delegate: cannot encode codemode state envelope within its payload bound")
	}
	return Envelope{Type: kind, ID: id, Data: data}, nil
}

func (m *Multiplexer) handleState(ctx context.Context, id string, req StateRequest) (Envelope, error) {
	var reply StateReply
	if m.handler.OnCodemodeState == nil || ctx.Err() != nil {
		reply.Code = StateUnavailable
	} else {
		var err error
		reply, err = m.handler.OnCodemodeState(ctx, req)
		if err != nil {
			return Envelope{}, errors.New("delegate: launcher codemode state operation failed")
		}
	}
	reply.Op = req.Op
	if !reply.valid(req.Op) {
		return Envelope{}, errStateReply
	}
	return stateEnvelope(EnvelopeStateReply, id, reply)
}
