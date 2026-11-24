package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/recordcodec"
)

type eventRecordCodec struct{ *recordcodec.Plan }

func (p eventRecordCodec) EncodeJSON(ctx context.Context, src any) ([]byte, error) {
	v, err := profileEvent(ctx, src)
	if err != nil {
		return nil, err
	}
	return p.Plan.EncodeJSON(ctx, v)
}
func (p eventRecordCodec) EncodeBSON(ctx context.Context, src any) ([]byte, error) {
	v, err := profileEvent(ctx, src)
	if err != nil {
		return nil, err
	}
	return p.Plan.EncodeBSON(ctx, v)
}

// Decoding applies the same narrow control profile to a temporary. It does not
// reinterpret other user strings as dates, and a failed profile never publishes.
func (p eventRecordCodec) DecodeJSON(ctx context.Context, raw []byte, dst any) error {
	return p.decodeEvent(ctx, raw, dst, p.Plan.DecodeJSON)
}
func (p eventRecordCodec) DecodeBSON(ctx context.Context, raw []byte, dst any) error {
	return p.decodeEvent(ctx, raw, dst, p.Plan.DecodeBSON)
}
func (p eventRecordCodec) decodeEvent(ctx context.Context, raw []byte, dst any, decode func(context.Context, []byte, any) error) error {
	dest, ok := dst.(*Event)
	if !ok || dest == nil {
		return fmt.Errorf("%w: wrong event destination", recordcodec.ErrInvalidRecord)
	}
	var tmp Event
	if err := decode(ctx, raw, &tmp); err != nil {
		return err
	}
	v, err := profileEvent(ctx, tmp)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	*dest = v
	return nil
}
func profileEvent(ctx context.Context, src any) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	var e Event
	switch v := src.(type) {
	case Event:
		e = v
	case *Event:
		if v == nil {
			return e, fmt.Errorf("%w: nil event", recordcodec.ErrInvalidRecord)
		}
		e = *v
	default:
		return e, fmt.Errorf("%w: wrong event source", recordcodec.ErrInvalidRecord)
	}
	key := ""
	switch e.Type {
	case EventArtifactWritten:
		key = "written_at"
	case EventRunStarted, EventRunResumed:
		key = "attempt"
	}
	if key == "" {
		return e, nil
	}
	value, present := e.Data[key]
	if !present {
		return e, nil
	}
	var normalized any
	if key == "written_at" {
		var t time.Time
		var err error
		switch v := value.(type) {
		case time.Time:
			t = v
		case string:
			t, err = parseExactEventTime(ctx, v)
		default:
			err = fmt.Errorf("written_at requires time.Time or RFC3339 text")
		}
		if err != nil {
			return Event{}, err
		}
		normalized, err = exactEventTime(t)
		if err != nil {
			return Event{}, err
		}
	} else {
		switch v := value.(type) {
		case map[string]any:
			if v == nil {
				return Event{}, fmt.Errorf("attempt requires a non-null native object")
			}
			normalized = v
		case RunAttempt:
			started, err := exactEventTime(v.StartedAt)
			if err != nil {
				return Event{}, err
			}
			m := map[string]any{"id": v.ID, "started_at": started}
			if v.WorkflowHash != "" {
				m["workflow_hash"] = v.WorkflowHash
			}
			if v.Budget != nil {
				b := v.Budget
				budget := map[string]any{}
				if b.MaxCostUSD != 0 {
					budget["max_cost_usd"] = b.MaxCostUSD
				}
				if b.MaxTokens != 0 {
					budget["max_tokens"] = b.MaxTokens
				}
				if b.WarnTokens != 0 {
					budget["warn_tokens"] = b.WarnTokens
				}
				if b.MaxIterations != 0 {
					budget["max_iterations"] = b.MaxIterations
				}
				if b.MaxDuration != "" {
					budget["max_duration"] = b.MaxDuration
				}
				if b.MaxParallelBranches != 0 {
					budget["max_parallel_branches"] = b.MaxParallelBranches
				}
				m["budget"] = budget
			}
			normalized = m
		default:
			return Event{}, fmt.Errorf("attempt requires RunAttempt value or native object")
		}
	}
	data := make(map[string]any, len(e.Data))
	for k, v := range e.Data {
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
		data[k] = v
	}
	data[key] = normalized
	e.Data = data
	return e, nil
}
func exactEventTime(t time.Time) (string, error) {
	raw, err := t.UTC().MarshalText()
	return string(raw), err
}

// time.Parse accepts offsets +24:00/+00:60 and truncates extra fraction digits.
// Check the owned string profile first; only redundant trailing zeros may be
// removed, without an arbitrary length bound or an uninterruptible large parse.
func parseExactEventTime(ctx context.Context, s string) (time.Time, error) {
	bad := func() (time.Time, error) { return time.Time{}, fmt.Errorf("invalid exact RFC3339 timestamp") }
	if len(s) < 20 {
		return bad()
	}
	for i := 0; i < 19; i++ {
		want := byte(0)
		switch i {
		case 4, 7:
			want = '-'
		case 10:
			want = 'T'
		case 13, 16:
			want = ':'
		}
		if want != 0 {
			if s[i] != want {
				return bad()
			}
		} else if s[i] < '0' || s[i] > '9' {
			return bad()
		}
	}
	end := 19
	fractionEnd := 19
	if s[end] == '.' {
		end++
		start := end
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			if err := ctx.Err(); err != nil {
				return time.Time{}, err
			}
			if end-start >= 9 && s[end] != '0' {
				return bad()
			}
			end++
		}
		if end == start {
			return bad()
		}
		fractionEnd = end
	}
	zone := s[end:]
	if zone != "Z" {
		if len(zone) != 6 || (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' {
			return bad()
		}
		for _, i := range []int{1, 2, 4, 5} {
			if zone[i] < '0' || zone[i] > '9' {
				return bad()
			}
		}
		if int(zone[1]-'0')*10+int(zone[2]-'0') >= 24 || int(zone[4]-'0')*10+int(zone[5]-'0') >= 60 {
			return bad()
		}
	}
	if fractionEnd > 29 {
		s = s[:29] + zone
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	// Explicit separators above exclude the standard parser's comma fraction.
	return time.Parse(time.RFC3339Nano, strings.Clone(s))
}
