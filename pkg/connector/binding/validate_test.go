package binding

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
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

// polledTrigger is validateTrigger read by a poller with a typed query.
func polledTrigger() spec.Trigger {
	t := validateTrigger
	t.Delivery = spec.DeliveryPoll
	t.Poll = &spec.TriggerPoll{Operation: "chat.message.search", Query: []spec.PollQueryField{
		{ID: "jql", Type: "string", Required: true, NotMatching: `(?i)\border\s+by\b`},
		{ID: "limit", Type: "int"},
		{ID: "archived", Type: "bool"},
	}}
	return t
}

// A subscription to a polled trigger names its query, and the poller it
// listens to is derived from it — from the connection's tenant, the
// connection, the trigger and the values — never taken from the request.
func TestAPolledSubscriptionListensToThePollerItsQueryDerives(t *testing.T) {
	sub := validSub()
	sub.PollQuery = map[string]string{"jql": "project = ENG", "limit": "50", "archived": ""}
	got, err := ValidateSubscription(sub, polledTrigger(), validateBot)
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := map[string]string{"jql": "project = ENG", "limit": "50"}
	if !reflect.DeepEqual(got.PollQuery, wantQuery) {
		t.Errorf("poll_query = %v, want %v: an empty value is an unset one", got.PollQuery, wantQuery)
	}
	if want := poller.ID(ConnectionTenant(""), "conn-a", "chat.message.posted", wantQuery); got.Poller != want {
		t.Errorf("poller = %q, want the one the query derives (%q)", got.Poller, want)
	}
	// A poller the subscription held — another query's, a stale one — is
	// re-derived, never kept.
	stale := sub
	stale.Poller = "pl-stale"
	if again, err := ValidateSubscription(stale, polledTrigger(), validateBot); err != nil || again.Poller != got.Poller {
		t.Errorf("a stale poller = (%q, %v), want the derived %q", again.Poller, err, got.Poller)
	}
	webhookStale := validSub()
	webhookStale.Poller = "pl-stale"
	if again, err := ValidateSubscription(webhookStale, validateTrigger, validateBot); err != nil || again.Poller != "" {
		t.Errorf("a webhook subscription holding a poller = (%q, %v), want none", again.Poller, err)
	}
	cloud := sub
	cloud.TenantID = "team-1"
	gotCloud, err := ValidateSubscription(cloud, polledTrigger(), validateBot)
	if err != nil || gotCloud.Poller == got.Poller {
		t.Errorf("a cloud tenant's poller = (%q, %v), want one of its own", gotCloud.Poller, err)
	}
	if got, err := ValidateSubscription(validSub(), validateTrigger, validateBot); err != nil || got.Poller != "" || got.PollQuery != nil {
		t.Errorf("a webhook subscription = (poller %q, query %v, %v), want neither", got.Poller, got.PollQuery, err)
	}
}

func TestAPolledSubscriptionsQueryIsTheOneItsTriggerDeclares(t *testing.T) {
	for name, tc := range map[string]struct {
		trig  spec.Trigger
		query map[string]string
		edit  func(*trigger.Subscription)
		want  string
	}{
		"a required field unset":    {polledTrigger(), map[string]string{"limit": "5"}, nil, "poll_query jql: trigger chat.message.posted requires it"},
		"a required field emptied":  {polledTrigger(), map[string]string{"jql": ""}, nil, "requires it"},
		"an undeclared field":       {polledTrigger(), map[string]string{"jql": "x", "jq": "y"}, nil, `declares no query field "jq"`},
		"an int that is not":        {polledTrigger(), map[string]string{"jql": "x", "limit": "5.5"}, nil, "not an integer"},
		"a bool that is not":        {polledTrigger(), map[string]string{"jql": "x", "archived": "yes"}, nil, "not true or false"},
		"a blank required field":    {polledTrigger(), map[string]string{"jql": "   "}, nil, "requires it"},
		"a value its field refuses": {polledTrigger(), map[string]string{"jql": "project = OPS order  by created DESC"}, nil, "which this field refuses"},
		"a negative backfill": {polledTrigger(), map[string]string{"jql": "x"}, func(s *trigger.Subscription) {
			s.Backfill = -1
		}, "backfill -1"},
		"a query on a webhook trigger":    {validateTrigger, map[string]string{"jql": "x"}, nil, "poll_query applies to a polled trigger"},
		"a backfill on a webhook trigger": {validateTrigger, nil, func(s *trigger.Subscription) { s.Backfill = 3 }, "backfill applies to a polled trigger"},
	} {
		t.Run(name, func(t *testing.T) {
			sub := validSub()
			sub.PollQuery = tc.query
			if tc.edit != nil {
				tc.edit(&sub)
			}
			if _, err := ValidateSubscription(sub, tc.trig, validateBot); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateSubscription = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A tick hands its params the query values in the type their field
// declares, the one the package's params were evaluated against at load.
func TestPollQueryValuesAreTyped(t *testing.T) {
	got, err := PollQueryValues(polledTrigger(), map[string]string{"jql": "x", "limit": "50", "archived": "true"})
	want := map[string]any{"jql": "x", "limit": int64(50), "archived": true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("PollQueryValues = (%#v, %v), want %#v", got, err, want)
	}
	if _, err := PollQueryValues(polledTrigger(), map[string]string{"limit": "x"}); err == nil {
		t.Fatal("a stored value its field's type no longer reads was handed to the params")
	}
}
