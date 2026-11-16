package binding

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

var validateTrigger = spec.Trigger{ID: "chat.message.posted",
	Filters: []spec.TriggerFilter{
		{ID: "room", From: "payload.room", Type: "string"},
		{ID: "kind", From: "payload.kind", Type: "enum", Options: []string{"post", "reply"}, Default: []string{"post"}},
		{ID: "bot", From: "payload.bot", Type: "bool"},
	},
	Outputs: []spec.TriggerOutput{{Name: "prompt", From: "payload.text"}},
}

var validateBot = []BotVar{{Name: "prompt", Required: true}, {Name: "tone"}}

func validSub() trigger.Subscription {
	return trigger.Subscription{ID: "s1", BotID: "b", TriggerID: "chat.message.posted", ConnectionID: "conn-a"}
}

func TestAValidSubscriptionIsStoredNormalised(t *testing.T) {
	got, err := ValidateSubscription(validSub(), validateTrigger, validateBot)
	if err != nil {
		t.Fatal(err)
	}
	if got.Invocation != bundle.InvocationKindConnector {
		t.Errorf("invocation = %q, want connector", got.Invocation)
	}
	want := trigger.Matcher{Sources: []trigger.Source{trigger.SourceConnector}, Kinds: []string{"chat.message.posted"}}
	if !reflect.DeepEqual(got.Match, want) {
		t.Errorf("match = %+v, want the server's %+v", got.Match, want)
	}
	if !reflect.DeepEqual(got.Filters, map[string][]string{"kind": {"post"}}) {
		t.Errorf("filters = %v, want the declared default filled", got.Filters)
	}

	chosen := validSub()
	chosen.Filters = map[string][]string{"kind": {"reply"}, "bot": {"false"}}
	chosen.Match = trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}}
	got, err = ValidateSubscription(chosen, validateTrigger, validateBot)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filters["kind"][0] != "reply" || !reflect.DeepEqual(got.Match, want) {
		t.Errorf("a chosen value or a client match survived: filters %v, match %+v", got.Filters, got.Match)
	}
}

func TestEveryRefusalNamesItsField(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*trigger.Subscription)
		bot  []BotVar
		want string
	}{
		"another trigger":      {func(s *trigger.Subscription) { s.TriggerID = "chat.other.event" }, nil, "trigger_id"},
		"board mode":           {func(s *trigger.Subscription) { s.Mode = bundle.ExecutionBoard }, nil, "unsupported"},
		"deliver mode":         {func(s *trigger.Subscription) { s.Mode = bundle.ExecutionDeliver }, nil, "unsupported"},
		"args_var":             {func(s *trigger.Subscription) { s.ArgsVar = "prompt" }, nil, "args_var"},
		"cron":                 {func(s *trigger.Subscription) { s.Cron = "* * * * *" }, nil, "cron"},
		"poller":               {func(s *trigger.Subscription) { s.Poller = "p1" }, nil, "poller"},
		"no_live_run":          {func(s *trigger.Subscription) { s.NoLiveRun = "queue" }, nil, "no_live_run"},
		"undeclared filter":    {func(s *trigger.Subscription) { s.Filters = map[string][]string{"ghost": {"x"}} }, nil, `filter "ghost"`},
		"enum outside options": {func(s *trigger.Subscription) { s.Filters = map[string][]string{"kind": {"edit"}} }, nil, "options"},
		"bool not a bool":      {func(s *trigger.Subscription) { s.Filters = map[string][]string{"bot": {"yes"}} }, nil, "true or false"},
		"when outside event":   {func(s *trigger.Subscription) { s.When = "payload.room == 'x'" }, nil, "when"},
		"var_map undeclared": {func(s *trigger.Subscription) {
			s.VarMap = map[string]string{"ghost": "event.prompt", "prompt": "event.prompt"}
		}, nil, "var_map ghost"},
		"var_map outside event": {func(s *trigger.Subscription) { s.VarMap = map[string]string{"prompt": "input.x"} }, nil, "var_map prompt"},
		"required unfilled":     {func(s *trigger.Subscription) {}, []BotVar{{Name: "repo", Required: true}}, `requires var "repo"`},
		"required unmapped":     {func(s *trigger.Subscription) { s.VarMap = map[string]string{"tone": "event.prompt"} }, nil, `requires var "prompt"`},
	} {
		s := validSub()
		tc.edit(&s)
		bot := tc.bot
		if bot == nil {
			bot = validateBot
		}
		if _, err := ValidateSubscription(s, validateTrigger, bot); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, tc.want)
		}
	}

	// A required var a constant fills, or a same-named output with no
	// var_map, is filled.
	s := validSub()
	s.Vars = map[string]string{"repo": "acme/app"}
	if _, err := ValidateSubscription(s, validateTrigger, []BotVar{{Name: "repo", Required: true}, {Name: "prompt", Required: true}}); err != nil {
		t.Errorf("a constant and a same-named output were refused: %v", err)
	}
}
