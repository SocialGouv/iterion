// Package binding reads a stored connector event the way a subscription
// binds it (ADR-168 §6, §9): which subscriptions an event matches, and the
// vars a matched one launches with. It is pure — no store, no clock, no
// network — so the matcher, the launch-time re-judgment and the preview call
// the same code and reach the same verdict.
package binding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// NamespaceEvent is the one namespace a subscription's `when` and var_map
// read (ADR-168 §9): event.<output>, event.payload, event.headers.
const NamespaceEvent = "event"

// Event is a stored sanitized event (ingress.Delivery.Event), decoded.
type Event struct {
	Outputs map[string]any `json:"outputs"`
	Payload map[string]any `json:"payload"`
	Headers map[string]any `json:"headers"`
}

// DecodeEvent decodes a stored event with the numbers the extraction read:
// integral values as int64, others as float64, an integer past int64 as its
// literal (ingress.NormalizeNumbers).
func DecodeEvent(raw json.RawMessage) (Event, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return Event{}, fmt.Errorf("binding: decode the stored event: %w", err)
	}
	ingress.NormalizeNumbers(m)
	ev := Event{}
	for _, f := range []struct {
		key string
		dst *map[string]any
	}{{"outputs", &ev.Outputs}, {"payload", &ev.Payload}, {"headers", &ev.Headers}} {
		switch v := m[f.key].(type) {
		case nil:
		case map[string]any:
			*f.dst = v
		default:
			return Event{}, fmt.Errorf("binding: the stored event's %s is a %T, not an object", f.key, v)
		}
	}
	return ev, nil
}

// EventContext is the context a subscription's `when` and var_map run
// under: event.payload and event.headers read the delivery, any other
// event.<name> reads the output of that name. No other root resolves.
func (e Event) EventContext() *expr.Context {
	return &expr.Context{Named: map[string]func([]string) any{
		NamespaceEvent: func(p []string) any {
			if len(p) == 0 {
				return nil
			}
			switch p[0] {
			case spec.NamespacePayload:
				return spec.Descend(e.Payload, p[1:])
			case spec.NamespaceHeaders:
				return spec.Descend(e.Headers, p[1:])
			}
			return spec.Descend(e.Outputs, p)
		},
	}}
}

// deliveryContext is the context a package's filters[].from runs under:
// the delivery's payload and listed headers, as at extraction.
func (e Event) deliveryContext() *expr.Context {
	return &expr.Context{Named: map[string]func([]string) any{
		spec.NamespacePayload: func(p []string) any { return spec.Descend(e.Payload, p) },
		spec.NamespaceHeaders: func(p []string) any { return spec.Descend(e.Headers, p) },
	}}
}

// LaunchString is the one conversion a value takes to reach a launch or a
// filter comparison (ADR-168 §9): a string as is, a number in canonical
// decimal, a bool as true/false, a list or map as compact JSON. ok is false
// for nil — an absent value, which sets no var and matches no filter.
func LaunchString(v any) (s string, ok bool) {
	switch c := v.(type) {
	case nil:
		return "", false
	case string:
		return c, true
	case bool:
		return strconv.FormatBool(c), true
	case int64:
		return strconv.FormatInt(c, 10), true
	case int:
		return strconv.Itoa(c), true
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64), true
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v), true
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// Verdict is a subscription's judgment of one event. Refusal, when set, is
// a verdict no retry changes (an expression that cannot be evaluated, its
// budget spent) and wins over Matched.
type Verdict struct {
	Matched bool
	Reason  string
	Refusal string
}

// Judge evaluates a subscription's filters, then its `when`, against an
// event its trigger declared. A filter with no chosen value constrains
// nothing; one with values matches when the event's value, converted by
// LaunchString, is among them — or, for a list filter, when any element of
// the event's list, each converted alike, is. A list filter whose event
// value is not a list is a refusal: the package's contract and the event
// disagree, and no retry changes that. Only ctx's own end is an error:
// everything an expression does is the verdict.
func Judge(ctx context.Context, sub trigger.Subscription, trig spec.Trigger, ev Event) (Verdict, error) {
	ids := make([]string, 0, len(sub.Filters))
	for id := range sub.Filters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	dctx := ev.deliveryContext()
	for _, id := range ids {
		chosen := sub.Filters[id]
		if len(chosen) == 0 {
			continue
		}
		f, ok := filterOf(trig, id)
		if !ok {
			return Verdict{Refusal: fmt.Sprintf("filter %q is not declared by trigger %s", id, trig.ID)}, nil
		}
		v, err := eval(ctx, f.From, dctx)
		if err != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			return Verdict{Refusal: fmt.Sprintf("filter %s (%s): %v", id, f.From, err)}, nil
		}
		s, ok := LaunchString(v)
		if !ok {
			return Verdict{Reason: fmt.Sprintf("filter %s: the event carries no value", id)}, nil
		}
		if f.Type == spec.FilterList {
			items, isList := v.([]any)
			if !isList {
				return Verdict{Refusal: fmt.Sprintf("filter %s (%s): reads a %T, not the list its type declares", id, f.From, v)}, nil
			}
			if !anyChosen(items, chosen) {
				return Verdict{Reason: fmt.Sprintf("filter %s: no element of %s is one of %q", id, s, chosen)}, nil
			}
			continue
		}
		if !slices.Contains(chosen, s) {
			return Verdict{Reason: fmt.Sprintf("filter %s: %q is not one of %q", id, s, chosen)}, nil
		}
	}
	if strings.TrimSpace(sub.When) != "" {
		holds, err := evalBool(ctx, sub.When, ev.EventContext())
		if err != nil {
			if ctx.Err() != nil {
				return Verdict{}, ctx.Err()
			}
			return Verdict{Refusal: fmt.Sprintf("when (%s): %v", sub.When, err)}, nil
		}
		if !holds {
			return Verdict{Reason: fmt.Sprintf("when (%s) does not hold", sub.When)}, nil
		}
	}
	return Verdict{Matched: true}, nil
}

// Vars computes the vars a matched subscription launches with: each var_map
// entry evaluated over the event, or — with no var_map — each output named
// like one of the bot's declared vars under that name; the subscription's
// constant vars on top. A nil value sets no var. An expression that cannot
// be evaluated is a refusal naming it; only ctx's own end is an error.
func Vars(ctx context.Context, sub trigger.Subscription, ev Event, botVars []string) (vars map[string]string, refusal string, err error) {
	vars = map[string]string{}
	if len(sub.VarMap) > 0 {
		names := make([]string, 0, len(sub.VarMap))
		for name := range sub.VarMap {
			names = append(names, name)
		}
		sort.Strings(names)
		ectx := ev.EventContext()
		for _, name := range names {
			v, err := eval(ctx, sub.VarMap[name], ectx)
			if err != nil {
				if ctx.Err() != nil {
					return nil, "", ctx.Err()
				}
				return nil, fmt.Sprintf("var_map %s (%s): %v", name, sub.VarMap[name], err), nil
			}
			if s, ok := LaunchString(v); ok {
				vars[name] = s
			}
		}
	} else {
		for _, name := range botVars {
			if s, ok := LaunchString(ev.Outputs[name]); ok {
				vars[name] = s
			}
		}
	}
	for k, v := range sub.Vars {
		vars[k] = v
	}
	return vars, "", nil
}

// anyChosen reports whether an element of a list value, converted by
// LaunchString, is among the chosen values. An absent element matches
// nothing, as an absent value does.
func anyChosen(items []any, chosen []string) bool {
	for _, item := range items {
		if s, ok := LaunchString(item); ok && slices.Contains(chosen, s) {
			return true
		}
	}
	return false
}

func filterOf(trig spec.Trigger, id string) (spec.TriggerFilter, bool) {
	for _, f := range trig.Filters {
		if f.ID == id {
			return f, true
		}
	}
	return spec.TriggerFilter{}, false
}

func eval(ctx context.Context, src string, ectx *expr.Context) (any, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return nil, err
	}
	return ast.EvalContext(ctx, ectx)
}

// evalBool reads `when` with the evaluator's own coercion.
func evalBool(ctx context.Context, src string, ectx *expr.Context) (bool, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return false, err
	}
	return ast.EvalBoolContext(ctx, ectx)
}

// ErrNotEvent marks an expression that reads outside the event namespace.
var ErrNotEvent = errors.New("binding: an expression may read the event namespace only")

// CheckEventExpr refuses an expression that does not parse or reads any
// root other than `event` — validated at save, so the evaluator's implicit
// `input` fallback never reads a field no event has (ADR-168 §9).
func CheckEventExpr(src string) error {
	ast, err := expr.Parse(src)
	if err != nil {
		return err
	}
	for _, r := range ast.Refs() {
		if r.Namespace != NamespaceEvent {
			return fmt.Errorf("%w: %q reads %q", ErrNotEvent, src, r.Namespace)
		}
	}
	return nil
}
