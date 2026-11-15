package binding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

const storedEvent = `{"outputs":{"subject":"PR-7","count":3,"title":"<b>hi</b>"},` +
	`"payload":{"channel":"C1","n":12345678901234567890123,"ratio":1.5,"ok":true,"tags":["a","b"],"meta":{"z":1,"a":"x"}},` +
	`"headers":{"X-Event":"message"}}`

func decoded(t *testing.T) Event {
	t.Helper()
	ev, err := DecodeEvent([]byte(storedEvent))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestDecodeEventReadsTheNumbersTheExtractionRead(t *testing.T) {
	ev := decoded(t)
	if _, ok := ev.Outputs["count"].(int64); !ok {
		t.Errorf("an integral output decoded as %T, want int64", ev.Outputs["count"])
	}
	if got := ev.Payload["n"]; got != "12345678901234567890123" {
		t.Errorf("an integer past int64 decoded as %v (%T), want its literal", got, got)
	}
	if _, err := DecodeEvent([]byte(`{"payload":[1]}`)); err == nil {
		t.Error("a payload that is not an object decoded")
	}
}

func TestLaunchStringIsTheOneConversion(t *testing.T) {
	ev := decoded(t)
	for _, tc := range []struct {
		v    any
		want string
	}{
		{"x", "x"},
		{int64(3), "3"},
		{1.5, "1.5"},
		{true, "true"},
		{ev.Payload["tags"], `["a","b"]`},
		{ev.Payload["meta"], `{"a":"x","z":1}`},
		{ev.Outputs["title"], "<b>hi</b>"},
		{[]any{"<"}, `["<"]`},
	} {
		if got, ok := LaunchString(tc.v); !ok || got != tc.want {
			t.Errorf("LaunchString(%#v) = %q, %v; want %q", tc.v, got, ok, tc.want)
		}
	}
	if _, ok := LaunchString(nil); ok {
		t.Error("nil converted to a value: an absent value must set no var")
	}
}

func evalEvent(t *testing.T, src string) (any, error) {
	t.Helper()
	ast, err := expr.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return ast.Eval(decoded(t).EventContext())
}

func TestTheEventNamespace(t *testing.T) {
	for src, want := range map[string]any{
		"event.subject":            "PR-7",
		"event.payload.channel":    "C1",
		`event.headers["X-Event"]`: "message",
		"event.payload.tags[1]":    "b",
		"event.missing":            nil,
		"event.payload.missing":    nil,
	} {
		if got, err := evalEvent(t, src); err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", src, got, err, want)
		}
	}
	for _, src := range []string{"payload.channel", "subject"} {
		if _, err := evalEvent(t, src); err == nil {
			t.Errorf("%s resolved outside the event namespace", src)
		}
	}
	// `input` is a standard root the evaluator reads as nil: the save
	// validation is what refuses it.
	if err := CheckEventExpr("input.x == 'y'"); !errors.Is(err, ErrNotEvent) {
		t.Errorf("an input root passed save validation: %v", err)
	}
	if err := CheckEventExpr("event.subject == 'PR-7'"); err != nil {
		t.Errorf("an event expression refused: %v", err)
	}
	if err := CheckEventExpr("payload.channel == 'C1'"); !errors.Is(err, ErrNotEvent) {
		t.Errorf("a payload root passed save validation: %v", err)
	}
}

var slackTrigger = spec.Trigger{ID: "slack.message.posted", Filters: []spec.TriggerFilter{
	{ID: "channel", From: "payload.channel", Type: "string"},
	{ID: "bot", From: "payload.ok", Type: "bool"},
	{ID: "absent", From: "payload.nope", Type: "string"},
}}

func judge(t *testing.T, sub trigger.Subscription) Verdict {
	t.Helper()
	v, err := Judge(context.Background(), sub, slackTrigger, decoded(t))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJudgeMatchesByMembershipThenWhen(t *testing.T) {
	for name, tc := range map[string]struct {
		sub     trigger.Subscription
		matched bool
		refused bool
	}{
		"no filter":            {trigger.Subscription{}, true, false},
		"equality":             {trigger.Subscription{Filters: map[string][]string{"channel": {"C1"}}}, true, false},
		"membership":           {trigger.Subscription{Filters: map[string][]string{"channel": {"C9", "C1"}}}, true, false},
		"bool as text":         {trigger.Subscription{Filters: map[string][]string{"bot": {"true"}}}, true, false},
		"other value":          {trigger.Subscription{Filters: map[string][]string{"channel": {"C2"}}}, false, false},
		"no chosen value":      {trigger.Subscription{Filters: map[string][]string{"channel": {}}}, true, false},
		"absent event value":   {trigger.Subscription{Filters: map[string][]string{"absent": {""}}}, false, false},
		"undeclared filter":    {trigger.Subscription{Filters: map[string][]string{"ghost": {"x"}}}, false, true},
		"when holds":           {trigger.Subscription{When: "event.count > 2"}, true, false},
		"when does not hold":   {trigger.Subscription{When: "event.count > 5"}, false, false},
		"when cannot evaluate": {trigger.Subscription{When: "nosuchfn(event.count)"}, false, true},
		"filter then when":     {trigger.Subscription{Filters: map[string][]string{"channel": {"C2"}}, When: "event.count > 2"}, false, false},
	} {
		v := judge(t, tc.sub)
		if v.Matched != tc.matched || (v.Refusal != "") != tc.refused {
			t.Errorf("%s: verdict %+v, want matched=%v refused=%v", name, v, tc.matched, tc.refused)
		}
		if !v.Matched && !tc.refused && v.Reason == "" {
			t.Errorf("%s: a non-match carries no reason", name)
		}
	}
}

// An expression past its work budget is a refused verdict naming it — a
// per-pair outcome, never an error that would retry the whole intake.
func TestASpentBudgetIsARefusal(t *testing.T) {
	t.Setenv(expr.EvalEnv, "1")
	v, err := Judge(context.Background(), trigger.Subscription{When: "event.count > 1 && event.count < 9"}, slackTrigger, decoded(t))
	if err != nil {
		t.Fatalf("a spent budget surfaced as an error: %v", err)
	}
	if v.Matched || !strings.Contains(v.Refusal, "when") {
		t.Fatalf("verdict %+v, want a refusal naming the when", v)
	}
}

func TestJudgeStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Judge(ctx, trigger.Subscription{When: "event.count > 1"}, slackTrigger, decoded(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled judgment = %v, want the context's end", err)
	}
}

func TestVarsMapThenFallBackToOutputs(t *testing.T) {
	ev := decoded(t)
	mapped := trigger.Subscription{
		VarMap: map[string]string{"prompt": "event.title", "gone": "event.missing", "chan": "event.payload.channel"},
		Vars:   map[string]string{"chan": "fixed"},
	}
	vars, refusal, err := Vars(context.Background(), mapped, ev, []string{"subject"})
	if err != nil || refusal != "" {
		t.Fatal(err, refusal)
	}
	if vars["prompt"] != "<b>hi</b>" || vars["chan"] != "fixed" {
		t.Errorf("vars = %v", vars)
	}
	if _, set := vars["gone"]; set {
		t.Error("a nil value set its var")
	}
	if _, set := vars["subject"]; set {
		t.Error("a var_map still passed same-named outputs")
	}

	plain, _, _ := Vars(context.Background(), trigger.Subscription{}, ev, []string{"subject", "count", "nope"})
	if plain["subject"] != "PR-7" || plain["count"] != "3" || len(plain) != 2 {
		t.Errorf("same-named outputs = %v, want subject and count", plain)
	}

	broken := trigger.Subscription{VarMap: map[string]string{"x": "nosuchfn(event.title)"}}
	if _, refusal, err := Vars(context.Background(), broken, ev, nil); err != nil || !strings.Contains(refusal, "var_map x") {
		t.Errorf("an unevaluable var_map = %q, %v; want a refusal naming it", refusal, err)
	}
}
