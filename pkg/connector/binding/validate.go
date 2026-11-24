package binding

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// BotVar is a bot's declared var as the save validation reads it.
type BotVar struct {
	Name string
	// Required: the var declares no default, so a launch that leaves it
	// unset fails.
	Required bool
}

// ValidateSubscription holds a connector subscription to the trigger it
// listens to and the bot it launches, at save (ADR-168 §6, §9) — what the
// matcher and the launch would otherwise discover one event at a time:
//
//   - the mode is one a connector run honours (direct), and no field of
//     another kind is set (args_var, cron);
//   - a polled trigger's query values are the ones it declares — the
//     required ones set, each of its type — and the poller is derived from
//     them; a webhook trigger takes no query, poller or backfill;
//   - every chosen filter is declared, an enum's values are its options, a
//     bool's are true/false, a string's and a list's are any text (a list
//     filter's values are the elements it matches); a declared default
//     fills a filter left unset;
//   - `when` and every var_map expression read the event namespace only;
//   - every var_map target is a var the bot declares, and every var the bot
//     requires is filled — by the var_map, else by a same-named output, or
//     by a constant.
//
// It answers the subscription as it will be stored: the defaults filled,
// the invocation kind set, and the match the server owns.
func ValidateSubscription(sub trigger.Subscription, trig spec.Trigger, bot []BotVar) (trigger.Subscription, error) {
	if sub.TriggerID != trig.ID {
		return sub, fmt.Errorf("trigger_id %q is not the declared trigger %q", sub.TriggerID, trig.ID)
	}
	// ResolveMode refuses board on a connector, and deliver and any unknown
	// mode on every kind: what passes is direct.
	if _, err := bundle.ResolveMode(bundle.InvocationKindConnector, sub.Mode, "a connector subscription"); err != nil {
		return sub, err
	}
	switch {
	case sub.ArgsVar != "":
		return sub, fmt.Errorf("args_var does not apply to a connector subscription: its var_map names what each var reads")
	case sub.Cron != "":
		return sub, fmt.Errorf("cron does not apply to a connector subscription")
	case sub.Backfill < 0:
		return sub, fmt.Errorf("backfill %d: want a number of past items, 0 or more", sub.Backfill)
	}
	query, err := checkPollQuery(sub, trig)
	if err != nil {
		return sub, err
	}
	switch sub.NoLiveRun {
	case "", "launch", "drop":
	default:
		return sub, fmt.Errorf("no_live_run %q is not one of launch, drop", sub.NoLiveRun)
	}

	filters := make(map[string][]string, len(sub.Filters))
	for id, values := range sub.Filters {
		f, ok := filterOf(trig, id)
		if !ok {
			return sub, fmt.Errorf("filter %q is not declared by trigger %s", id, trig.ID)
		}
		for _, v := range values {
			switch f.Type {
			case spec.FilterEnum:
				if !slices.Contains(f.Options, v) {
					return sub, fmt.Errorf("filter %s: %q is not one of its options %q", id, v, f.Options)
				}
			case spec.FilterBool:
				if v != "true" && v != "false" {
					return sub, fmt.Errorf("filter %s: %q is not true or false", id, v)
				}
			}
		}
		filters[id] = append([]string(nil), values...)
	}
	for _, f := range trig.Filters {
		if _, chosen := filters[f.ID]; !chosen && len(f.Default) > 0 {
			filters[f.ID] = append([]string(nil), f.Default...)
		}
	}
	if len(filters) == 0 {
		filters = nil
	}

	if sub.When != "" {
		if err := CheckEventExpr(sub.When); err != nil {
			return sub, fmt.Errorf("when: %w", err)
		}
	}
	declared := map[string]BotVar{}
	for _, v := range bot {
		declared[v.Name] = v
	}
	targets := make([]string, 0, len(sub.VarMap))
	for name := range sub.VarMap {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	for _, name := range targets {
		if _, ok := declared[name]; !ok {
			return sub, fmt.Errorf("var_map %s: the bot declares no var %q", name, name)
		}
		if err := CheckEventExpr(sub.VarMap[name]); err != nil {
			return sub, fmt.Errorf("var_map %s: %w", name, err)
		}
	}
	outputs := map[string]bool{}
	for _, o := range trig.Outputs {
		outputs[o.Name] = true
	}
	for _, v := range bot {
		if !v.Required {
			continue
		}
		_, constant := sub.Vars[v.Name]
		filled := constant
		if len(sub.VarMap) > 0 {
			_, mapped := sub.VarMap[v.Name]
			filled = filled || mapped
		} else {
			filled = filled || outputs[v.Name]
		}
		if !filled {
			return sub, fmt.Errorf("the bot requires var %q and nothing fills it: map it in var_map, or set it in vars", v.Name)
		}
	}

	sub.Invocation = bundle.InvocationKindConnector
	sub.ConnectorKey = trigger.ConnectorKey(sub.ConnectionID, sub.TriggerID)
	sub.PollQuery = query
	sub.Poller = ""
	if trig.Polled() {
		sub.Poller = poller.ID(ConnectionTenant(sub.TenantID), sub.ConnectionID, sub.TriggerID, query)
	}
	sub.Filters = filters
	sub.Match = trigger.Matcher{Sources: []trigger.Source{trigger.SourceConnector}, Kinds: []string{trig.ID}}
	return sub, nil
}

// checkPollQuery holds a subscription's query values to its trigger: a
// polled trigger's declared fields only, the required ones set, an int an
// integer and a bool true or false; a webhook trigger takes none, nor a
// backfill. The poller is re-derived from them on every save, whatever the
// subscription held — the query is its identity.
func checkPollQuery(sub trigger.Subscription, trig spec.Trigger) (map[string]string, error) {
	if !trig.Polled() {
		switch {
		case len(sub.PollQuery) > 0:
			return nil, fmt.Errorf("poll_query applies to a polled trigger, and %s is delivered by webhook", trig.ID)
		case sub.Backfill > 0:
			return nil, fmt.Errorf("backfill applies to a polled trigger, and %s is delivered by webhook", trig.ID)
		}
		return nil, nil
	}
	declared := map[string]spec.PollQueryField{}
	for _, f := range trig.Poll.Query {
		declared[f.ID] = f
	}
	ids := make([]string, 0, len(sub.PollQuery))
	for id := range sub.PollQuery {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := map[string]string{}
	for _, id := range ids {
		f, ok := declared[id]
		if !ok {
			return nil, fmt.Errorf("poll_query %s: trigger %s declares no query field %q", id, trig.ID, id)
		}
		v := sub.PollQuery[id]
		if strings.TrimSpace(v) == "" {
			// A blank value is an unset one: kept out of the identity, so a
			// field cleared and a field never typed are one poller.
			continue
		}
		if why := f.Refuses(v); why != "" {
			return nil, fmt.Errorf("poll_query %s: %q %s", id, v, why)
		}
		switch f.Type {
		case "int":
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return nil, fmt.Errorf("poll_query %s: %q is not an integer", id, v)
			}
		case "bool":
			if v != "true" && v != "false" {
				return nil, fmt.Errorf("poll_query %s: %q is not true or false", id, v)
			}
		}
		out[id] = v
	}
	for _, f := range trig.Poll.Query {
		if _, set := out[f.ID]; f.Required && !set {
			return nil, fmt.Errorf("poll_query %s: trigger %s requires it", f.ID, trig.ID)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, nil
}

// PollQueryValues renders stored query values as a poll's params read them
// (spec.NewPollParamsContext): an int as an int64, a bool as a bool, a
// string as typed. The values were checked at save (checkPollQuery); one
// that no longer parses — its field's type changed since — is refused.
func PollQueryValues(trig spec.Trigger, query map[string]string) (map[string]any, error) {
	out := map[string]any{}
	if trig.Poll == nil {
		return out, nil
	}
	for _, f := range trig.Poll.Query {
		v, set := query[f.ID]
		if !set {
			continue
		}
		switch f.Type {
		case "int":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("query %s: %q is not an integer", f.ID, v)
			}
			out[f.ID] = n
		case "bool":
			out[f.ID] = v == "true"
		default:
			out[f.ID] = v
		}
	}
	return out, nil
}
