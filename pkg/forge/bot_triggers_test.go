package forge

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The error policy of a refused projection, and the server's refusal to
// hand-edit a spine-routed config, both ask whether ANY lane routes by the
// spine: a lane other than issues counts.
func TestAnyLaneRoutesSpine(t *testing.T) {
	for _, tc := range []struct {
		routing webhooks.ForgeRouting
		want    bool
	}{
		{webhooks.ForgeRouting{}, false},
		{webhooks.ForgeRouting{Issues: webhooks.RoutingLegacy, PullRequests: webhooks.RoutingShadow, Comments: webhooks.RoutingLegacy}, false},
		{webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}, true},
		{webhooks.ForgeRouting{PullRequests: webhooks.RoutingSpine}, true},
		{webhooks.ForgeRouting{Issues: webhooks.RoutingLegacy, Comments: webhooks.RoutingSpine}, true},
	} {
		if got := AnyLaneRoutesSpine(tc.routing); got != tc.want {
			t.Errorf("AnyLaneRoutesSpine(%+v) = %v, want %v", tc.routing, got, tc.want)
		}
	}
}

// The org gate parks a switch TO spine and nothing else.
func TestRoutingSwitchExpandsSurface(t *testing.T) {
	spine := webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}
	for _, tc := range []struct {
		name       string
		stored     webhooks.ForgeRouting
		lane, mode string
		want       bool
	}{
		{"a first flip to spine", webhooks.ForgeRouting{}, webhooks.LaneIssues, webhooks.RoutingSpine, true},
		{"legacy to spine", webhooks.ForgeRouting{Issues: webhooks.RoutingLegacy}, webhooks.LaneIssues, webhooks.RoutingSpine, true},
		{"shadow to spine", webhooks.ForgeRouting{Issues: webhooks.RoutingShadow}, webhooks.LaneIssues, webhooks.RoutingSpine, true},
		{"another lane to spine", spine, webhooks.LanePullRequests, webhooks.RoutingSpine, true},
		{"spine re-asserted", spine, webhooks.LaneIssues, webhooks.RoutingSpine, false},
		{"the rollback", spine, webhooks.LaneIssues, webhooks.RoutingLegacy, false},
		{"spine to shadow", spine, webhooks.LaneIssues, webhooks.RoutingShadow, false},
		{"legacy to shadow", webhooks.ForgeRouting{}, webhooks.LaneIssues, webhooks.RoutingShadow, false},
	} {
		if got := RoutingSwitchExpandsSurface(tc.stored, tc.lane, tc.mode); got != tc.want {
			t.Errorf("%s: expands = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// applyBotTriggers is the HoldLabels rule, per bot: a silent request keeps
// what is stored, an entry replaces that bot's whole value.
func TestApplyBotTriggers(t *testing.T) {
	stored := map[string]BotTrigger{
		"nestor":   {Labels: []string{"urgent"}, LaunchVars: map[string]string{"force": "true"}},
		"saboteur": {Residual: true},
	}
	if got := applyBotTriggers(stored, nil); !botTriggersEqual(got, stored) {
		t.Fatalf("a silent request changed the stored triggers: %+v", got)
	}
	if got := applyBotTriggers(nil, nil); got != nil {
		t.Fatalf("nothing stored, nothing asked: got %+v, want nil (the field stays off the document)", got)
	}

	got := applyBotTriggers(stored, map[string]BotTrigger{"nestor": {Labels: []string{}}, "feature-dev": {Enabled: boolPtr(true)}})
	want := map[string]BotTrigger{
		"nestor":      {Labels: []string{}},
		"saboteur":    {Residual: true},
		"feature-dev": {Enabled: boolPtr(true)},
	}
	if !botTriggersEqual(got, want) {
		t.Fatalf("applied = %+v, want %+v", got, want)
	}
	if got["nestor"].LaunchVars != nil {
		t.Fatalf("an entry MERGED into the stored one (%+v): it replaces the bot's whole value", got["nestor"])
	}
	if got["nestor"].Labels == nil {
		t.Fatal("the spoken-empty labels list (any label) became nil (the manifest's)")
	}

	// The result is the caller's own: editing it reaches neither argument.
	got["saboteur"] = BotTrigger{}
	kept := applyBotTriggers(stored, nil)
	kept["nestor"].Labels[0] = "edited"
	kept["nestor"].LaunchVars["force"] = "edited"
	if !stored["saboteur"].Residual || stored["nestor"].Labels[0] != "urgent" || stored["nestor"].LaunchVars["force"] != "true" {
		t.Fatalf("the result aliases the stored map: %+v", stored)
	}
}

func TestBotTriggersEqual(t *testing.T) {
	base := func() map[string]BotTrigger {
		return map[string]BotTrigger{"nestor": {Labels: []string{"a", "b"}, Enabled: boolPtr(true), Residual: true, LaunchVars: map[string]string{"k": "v"}}}
	}
	if !botTriggersEqual(base(), base()) {
		t.Fatal("two equal maps compare different")
	}
	if !botTriggersEqual(nil, map[string]BotTrigger{}) {
		t.Fatal("nil and empty maps compare different: both hold no entry")
	}
	for name, edit := range map[string]func(map[string]BotTrigger){
		"an entry more":               func(m map[string]BotTrigger) { m["saboteur"] = BotTrigger{} },
		"another bot under the entry": func(m map[string]BotTrigger) { m["saboteur"] = m["nestor"]; delete(m, "nestor") },
		"a label more":                func(m map[string]BotTrigger) { e := m["nestor"]; e.Labels = []string{"a", "b", "c"}; m["nestor"] = e },
		"labels in another order":     func(m map[string]BotTrigger) { e := m["nestor"]; e.Labels = []string{"b", "a"}; m["nestor"] = e },
		"labels back to the manifest": func(m map[string]BotTrigger) { e := m["nestor"]; e.Labels = nil; m["nestor"] = e },
		"enabled flipped":             func(m map[string]BotTrigger) { e := m["nestor"]; e.Enabled = boolPtr(false); m["nestor"] = e },
		"enabled back to the rule":    func(m map[string]BotTrigger) { e := m["nestor"]; e.Enabled = nil; m["nestor"] = e },
		"residual off":                func(m map[string]BotTrigger) { e := m["nestor"]; e.Residual = false; m["nestor"] = e },
		"a launch var changed": func(m map[string]BotTrigger) {
			e := m["nestor"]
			e.LaunchVars = map[string]string{"k": "w"}
			m["nestor"] = e
		},
		"the launch vars gone":         func(m map[string]BotTrigger) { e := m["nestor"]; e.LaunchVars = nil; m["nestor"] = e },
		"an empty map for another bot": func(m map[string]BotTrigger) { delete(m, "nestor"); m["x"] = BotTrigger{} },
	} {
		other := base()
		edit(other)
		if botTriggersEqual(base(), other) || botTriggersEqual(other, base()) {
			t.Errorf("%s: the two maps compare equal", name)
		}
	}
	// The one distinction a slice comparison alone would miss: nil follows the
	// manifest, empty claims any label.
	manifest := map[string]BotTrigger{"nestor": {}}
	anyLabel := map[string]BotTrigger{"nestor": {Labels: []string{}}}
	if botTriggersEqual(manifest, anyLabel) || botTriggersEqual(anyLabel, manifest) {
		t.Error("\"follow the manifest\" and \"any label\" compare equal: a write of one over the other would be dropped as a no-op")
	}
}

// BotTriggersExpandSurface is what the org-approval gate asks of a
// bot_triggers write: does it grow what starts a bot, or what its runs are
// told? It fails toward approval.
func TestBotTriggersExpandSurface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stored    map[string]BotTrigger
		requested map[string]BotTrigger
		want      bool
	}{
		{name: "a silent request", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, want: false},
		{name: "the stored entry, echoed", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}, Residual: true, Enabled: boolPtr(true), LaunchVars: map[string]string{"k": "v"}}},
			requested: map[string]BotTrigger{"nestor": {Labels: []string{"A "}, Residual: true, Enabled: boolPtr(true), LaunchVars: map[string]string{"k": "v"}}}, want: false},

		// labels
		{name: "widen: a label more", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"a", "b"}}}, want: true},
		{name: "widen: to any label", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{}}}, want: true},
		{name: "widen: a wildcard", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"*"}}}, want: true},
		{name: "widen: first explicit labels on a bot with no entry", requested: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, want: true},
		{name: "widen: first explicit labels on an adopted-off bot", stored: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, want: true},
		{name: "widen: back to the manifest's labels", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, requested: map[string]BotTrigger{"nestor": {}}, want: true},
		{name: "narrow: a label fewer", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a", "b"}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"b"}}}, want: false},
		{name: "narrow: from any label to one", stored: map[string]BotTrigger{"nestor": {Labels: []string{}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, want: false},
		{name: "narrow: from a wildcard to one", stored: map[string]BotTrigger{"nestor": {Labels: []string{"*"}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}}, want: false},
		{name: "same: any label stays any label", stored: map[string]BotTrigger{"nestor": {Labels: []string{}}}, requested: map[string]BotTrigger{"nestor": {Labels: []string{}}}, want: false},

		// enabled
		{name: "enable: on a bot the rule decided", requested: map[string]BotTrigger{"nestor": {Enabled: boolPtr(true)}}, want: true},
		{name: "enable: on a bot pinned off", stored: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, requested: map[string]BotTrigger{"nestor": {Enabled: boolPtr(true)}}, want: true},
		{name: "enable: dropping an explicit off", stored: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, requested: map[string]BotTrigger{"nestor": {}}, want: true},
		{name: "narrow: disable", stored: map[string]BotTrigger{"nestor": {Enabled: boolPtr(true)}}, requested: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, want: false},
		{name: "narrow: disable a bot with no entry", requested: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, want: false},
		{name: "narrow: dropping an explicit on", stored: map[string]BotTrigger{"nestor": {Enabled: boolPtr(true)}}, requested: map[string]BotTrigger{"nestor": {}}, want: false},

		// residual
		{name: "residual: turned on", requested: map[string]BotTrigger{"nestor": {Residual: true}}, want: true},
		{name: "narrow: residual turned off", stored: map[string]BotTrigger{"nestor": {Residual: true}}, requested: map[string]BotTrigger{"nestor": {}}, want: false},

		// launch vars
		{name: "vars: a var added", requested: map[string]BotTrigger{"nestor": {LaunchVars: map[string]string{"force": "true"}}}, want: true},
		{name: "vars: a var changed", stored: map[string]BotTrigger{"nestor": {LaunchVars: map[string]string{"force": "false"}}}, requested: map[string]BotTrigger{"nestor": {LaunchVars: map[string]string{"force": "true"}}}, want: true},
		{name: "vars: a var removed", stored: map[string]BotTrigger{"nestor": {LaunchVars: map[string]string{"force": "false"}}}, requested: map[string]BotTrigger{"nestor": {}}, want: true},

		// only the requested bots are judged
		{name: "another bot's stored entry is not a change", stored: map[string]BotTrigger{"saboteur": {Residual: true, Labels: []string{"x"}}}, requested: map[string]BotTrigger{"nestor": {Enabled: boolPtr(false)}}, want: false},
		{name: "one expanding entry among narrowing ones", stored: map[string]BotTrigger{"nestor": {Labels: []string{"a", "b"}}, "saboteur": {Labels: []string{"x"}}},
			requested: map[string]BotTrigger{"nestor": {Labels: []string{"a"}}, "saboteur": {Labels: []string{"x", "y"}}}, want: true},
	} {
		if got := BotTriggersExpandSurface(tc.stored, tc.requested); got != tc.want {
			t.Errorf("%s: expands = %v, want %v", tc.name, got, tc.want)
		}
	}
}
