package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The persisted and wire values of a binding and of a projected row's
// origin: stored rows and the forge router both spell them.
func TestForgeBindingsAndOrigin(t *testing.T) {
	if got := ForgeBindingForIntegration("int-1"); got != "integration:int-1" {
		t.Errorf("ForgeBindingForIntegration = %q, want integration:int-1", got)
	}
	if got := ForgeBindingForWebhook("wh-1"); got != "webhook:wh-1" {
		t.Errorf("ForgeBindingForWebhook = %q, want webhook:wh-1", got)
	}
	if got := ForgeOrigin("int-1"); got != "forge:int-1" {
		t.Errorf("ForgeOrigin = %q, want forge:int-1", got)
	}
	if ForgeBindingForIntegration("x") == ForgeBindingForWebhook("x") {
		t.Error("an integration and a webhook of one id share a binding")
	}
}

// A forge row's id is derived from the tenant, the bot, the invocation key
// and the integration, under the integration's origin: it is shared with no
// manifest-derived row and no connector row, whatever their fields.
func TestDerivedForgeSubscriptionID(t *testing.T) {
	const key = "forge:issue_labeled|a:|l:b730f724817d1dc8"
	id := DerivedForgeSubscriptionID("t1", "nestor", key, "int-1")
	if !IsDerivedID(id) {
		t.Fatalf("%q is not a derived id", id)
	}
	if again := DerivedForgeSubscriptionID("t1", "nestor", key, "int-1"); again != id {
		t.Fatalf("the id is not stable: %q then %q", id, again)
	}
	if want := DerivedSubscriptionID("t1", "nestor", bundle.InvocationKindForge, key, "forge:int-1"); id != want {
		t.Fatalf("id = %q, want the derived id whose fifth field is the integration's origin (%q)", id, want)
	}
	for name, other := range map[string]string{
		"another tenant":      DerivedForgeSubscriptionID("t2", "nestor", key, "int-1"),
		"another bot":         DerivedForgeSubscriptionID("t1", "saboteur", key, "int-1"),
		"another invocation":  DerivedForgeSubscriptionID("t1", "nestor", "legacy:issue_labeled", "int-1"),
		"another integration": DerivedForgeSubscriptionID("t1", "nestor", key, "int-2"),
		"a manifest-derived row of the same invocation": DerivedSubscriptionID("t1", "nestor", bundle.InvocationKindForge, key, ""),
		"a connection named like the integration":       DerivedSubscriptionID("t1", "nestor", bundle.InvocationKindForge, key, "int-1"),
	} {
		if other == id {
			t.Errorf("%s shares the id %q", name, id)
		}
	}
}

// issueRoutes reads a bot's routing invocations from its manifest the way
// the planner does.
func issueRoutes(t *testing.T, fr *bundle.ForgeRequirements, invs ...bundle.Invocation) []bundle.ForgeRoutingInvocation {
	t.Helper()
	routes, warnings := bundle.ForgeIssueInvocations(fr, invs)
	if len(warnings) != 0 {
		t.Fatalf("the fixture manifest warns: %v", warnings)
	}
	return routes
}

func declaredIssueInvocation(mode bundle.ExecutionMode, labels ...string) bundle.Invocation {
	return bundle.Invocation{
		Kind: bundle.InvocationKindForge, Mode: mode, ArgsVar: "task",
		ContextVars: map[string]string{"force": "true"},
		Forge:       &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Actions: []string{"labeled", "opened"}, Labels: labels},
	}
}

// The row a labelled issue starts a bot through: bound to its integration,
// under the integration's origin, matched on the labels the planner gives
// it, carrying the invocation's mode and the planner's vars.
func TestFromForgeIssueInvocation(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	target := ForgeTarget{TenantID: "t1", IntegrationID: "int-1", WebhookID: "wh-1", Repo: "acme/api"}

	t.Run("a declared invocation", func(t *testing.T) {
		routes := issueRoutes(t, nil,
			bundle.Invocation{Kind: bundle.InvocationKindCommand, ArgsVar: "prompt", Command: &bundle.InvocationCommand{Name: "nestor"}},
			declaredIssueInvocation("", "nestor"))
		if len(routes) != 1 {
			t.Fatalf("routes = %+v, want one", routes)
		}
		ri := routes[0]
		id := DerivedForgeSubscriptionID("t1", "nestor", ri.Key, "int-1")
		labels, exclude := []string{"nestor"}, []string{"saboteur"}
		vars := map[string]string{"force": "true", "tier": "demo"}
		sub, ok := FromForgeIssueInvocation(id, target, "nestor", ri, labels, exclude, vars, true, "not shown on an enabled row", "digest-1", now)
		if !ok {
			t.Fatal("no row derived")
		}
		want := Subscription{
			ID: id, TenantID: "t1", Repo: "acme/api", BotID: "nestor",
			Invocation: bundle.InvocationKindForge,
			// The invocation declares no mode: it launches directly.
			Mode: bundle.ExecutionDirect,
			Match: Matcher{
				Sources: []Source{SourceForge}, Kinds: []string{"issue_labeled"}, Actions: []string{"labeled"}, Repos: []string{"acme/api"},
				AddedLabels: []string{"nestor"}, ExcludeAddedLabels: []string{"saboteur"},
			},
			ArgsVar: "task", OpensMR: false,
			Vars:             map[string]string{"force": "true", "tier": "demo"},
			ForgeBinding:     "integration:int-1",
			Origin:           "forge:int-1",
			ProjectionDigest: "digest-1",
			Enabled:          true,
			CreatedAt:        now, UpdatedAt: now,
		}
		if !reflect.DeepEqual(sub, want) {
			t.Fatalf("row =\n%+v\nwant\n%+v", sub, want)
		}

		// The row owns its lists and its vars: the planner reusing its own
		// leaves the row as built.
		labels[0], exclude[0], vars["force"] = "changed", "changed", "changed"
		if sub.Match.AddedLabels[0] != "nestor" || sub.Match.ExcludeAddedLabels[0] != "saboteur" || sub.Vars["force"] != "true" {
			t.Fatalf("the row shares memory with its inputs: %+v vars %v", sub.Match, sub.Vars)
		}

		// The row claims the router's event for its label, whatever the case
		// of the repository, and is written, bound and off the bus.
		ev := labelEvent("t1", "nestor")
		ev.Repo = "Acme/API"
		if ok, failed := sub.Match.Explain(ev); !ok {
			t.Fatalf("the row does not claim its own label: %s", failed)
		}
		if sub.Match.Match(labelEvent("t1", "saboteur")) || sub.Match.Match(labelEvent("t1", "bug")) {
			t.Fatal("the row claims a label it excludes or does not want")
		}
		st := NewMemorySubscriptionStore()
		if err := st.Create(context.Background(), sub); err != nil {
			t.Fatalf("the store refuses the built row: %v", err)
		}
		if got := boundIDs(t, st, "t1", ForgeBindingForIntegration("int-1")); !reflect.DeepEqual(got, []string{id}) {
			t.Fatalf("the integration's binding holds %v, want the row", got)
		}
		if got := candidateIDs(t, st, labelEvent("t1", "nestor")); len(got) != 0 {
			t.Fatalf("the built row is a bus candidate: %v", got)
		}
	})

	t.Run("the block's own route", func(t *testing.T) {
		routes := issueRoutes(t, &bundle.ForgeRequirements{Events: []string{bundle.ForgeEventIssueLabeled}, Labels: []string{"feature"}})
		if len(routes) != 1 || !routes[0].Synthetic {
			t.Fatalf("routes = %+v, want the block's synthetic one", routes)
		}
		sub, ok := FromForgeIssueInvocation("drv-x", target, "feature-dev", routes[0], nil, nil, nil, true, "", "digest-2", now)
		if !ok {
			t.Fatal("no row derived")
		}
		if sub.Mode != bundle.ExecutionBoard || sub.ArgsVar != bundle.ForgeIssueDefaultArgsVar || !sub.OpensMR {
			t.Fatalf("mode %q args var %q opens_mr %t, want the label route: board, %s, true", sub.Mode, sub.ArgsVar, sub.OpensMR, bundle.ForgeIssueDefaultArgsVar)
		}
		if sub.Match.AddedLabels != nil || sub.Match.ExcludeAddedLabels != nil || sub.Vars != nil {
			t.Fatalf("no label and no var were given, the row carries %+v vars %v", sub.Match, sub.Vars)
		}
	})

	t.Run("board and direct are the invocation's", func(t *testing.T) {
		for mode, want := range map[bundle.ExecutionMode]bundle.ExecutionMode{
			"":                     bundle.ExecutionDirect,
			bundle.ExecutionDirect: bundle.ExecutionDirect,
			bundle.ExecutionBoard:  bundle.ExecutionBoard,
		} {
			routes := issueRoutes(t, nil, declaredIssueInvocation(mode, "nestor"))
			sub, ok := FromForgeIssueInvocation("drv-x", target, "nestor", routes[0], []string{"nestor"}, nil, nil, true, "", "", now)
			if !ok || sub.Mode != want {
				t.Errorf("mode %q derived %q (ok=%t), want %q", mode, sub.Mode, ok, want)
			}
		}
	})

	t.Run("a disabled row says why", func(t *testing.T) {
		ri := issueRoutes(t, nil, declaredIssueInvocation("", "nestor"))[0]
		off, ok := FromForgeIssueInvocation("drv-x", target, "nestor", ri, nil, nil, nil, false, "shares acme/api with 2 bots", "", now)
		if !ok || off.Enabled || off.DisabledReason != "shares acme/api with 2 bots" {
			t.Fatalf("disabled row = %+v (ok=%t)", off, ok)
		}
		on, ok := FromForgeIssueInvocation("drv-x", target, "nestor", ri, nil, nil, nil, true, "shares acme/api with 2 bots", "", now)
		if !ok || !on.Enabled || on.DisabledReason != "" {
			t.Fatalf("an enabled row carries enabled=%t reason %q", on.Enabled, on.DisabledReason)
		}
	})

	t.Run("a hand-created webhook's row", func(t *testing.T) {
		ri := issueRoutes(t, nil, declaredIssueInvocation("", "nestor"))[0]
		sub, ok := FromForgeIssueInvocation("hook-row", ForgeTarget{TenantID: "t1", WebhookID: "wh-1"}, "nestor", ri, []string{"nestor"}, nil, nil, true, "", "", now)
		if !ok {
			t.Fatal("no row derived")
		}
		if sub.ForgeBinding != "webhook:wh-1" || sub.Origin != "webhook:wh-1" {
			t.Fatalf("binding %q origin %q, want webhook:wh-1 for both", sub.ForgeBinding, sub.Origin)
		}
		// No repository on the target: the dimension is off, not pinned to
		// an event that names none.
		if sub.Repo != "" || sub.Match.Repos != nil {
			t.Fatalf("repo %q match repos %q, want none", sub.Repo, sub.Match.Repos)
		}
		if !sub.Match.Match(labelEvent("t1", "nestor")) {
			t.Fatal("a row with no repository does not claim an event of a repository")
		}
		if err := NewMemorySubscriptionStore().Create(context.Background(), sub); err != nil {
			t.Fatalf("the store refuses the built row: %v", err)
		}
	})

	t.Run("nothing is derived", func(t *testing.T) {
		good := issueRoutes(t, nil, declaredIssueInvocation("", "nestor"))[0]
		with := func(edit func(*bundle.Invocation)) bundle.ForgeRoutingInvocation {
			ri := good
			inv := ri.Inv
			forge := *inv.Forge
			inv.Forge = &forge
			edit(&inv)
			ri.Inv = inv
			return ri
		}
		for name, tc := range map[string]struct {
			target ForgeTarget
			ri     bundle.ForgeRoutingInvocation
		}{
			"a target bound to nothing": {target: ForgeTarget{TenantID: "t1", Repo: "acme/api"}, ri: good},
			"a mode no effect honours":  {target: target, ri: with(func(inv *bundle.Invocation) { inv.Mode = bundle.ExecutionDeliver })},
			"an unknown mode":           {target: target, ri: with(func(inv *bundle.Invocation) { inv.Mode = "launch" })},
			"another forge event":       {target: target, ri: with(func(inv *bundle.Invocation) { inv.Forge.Event = "pull_request" })},
			"another invocation kind":   {target: target, ri: with(func(inv *bundle.Invocation) { inv.Kind = bundle.InvocationKindBoard })},
			"no forge block":            {target: target, ri: with(func(inv *bundle.Invocation) { inv.Forge = nil })},
		} {
			if sub, ok := FromForgeIssueInvocation("drv-x", tc.target, "nestor", tc.ri, []string{"nestor"}, nil, nil, true, "", "", now); ok {
				t.Errorf("%s derived %+v", name, sub)
			}
		}
		if _, ok := FromForgeIssueInvocation("drv-x", target, "nestor", good, []string{"nestor"}, nil, nil, true, "", "", now); !ok {
			t.Error("the unedited control derived nothing")
		}
	})
}

// A manifest reconciliation leaves a forge row alone: it neither removes it
// as a derived row no invocation wants, nor rewrites a field of it — while
// it does retire the manifest-derived rows of the same bot.
func TestReconcileManifests_KeepsForgeBoundRows(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		forgeInv := declaredIssueInvocation("", "nestor")
		ri := issueRoutes(t, nil, forgeInv)[0]
		target := ForgeTarget{TenantID: "t1", IntegrationID: "int-1", Repo: "acme/api"}
		id := DerivedForgeSubscriptionID("t1", "nestor", ri.Key, "int-1")
		row, ok := FromForgeIssueInvocation(id, target, "nestor", ri, []string{"nestor"}, []string{"saboteur"},
			map[string]string{"force": "true"}, false, "parked by its operator", "digest-1", now)
		if !ok {
			t.Fatal("no forge row derived")
		}
		if err := st.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
		// The row is one the removal loop reaches: a derived id, a bot the
		// discovery returned, an invocation the reconciliation derives no
		// row for.
		if !IsDerivedID(row.ID) {
			t.Fatalf("the fixture row's id %q is not derived", row.ID)
		}
		before, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		// The bot's manifest holds the forge invocation and a schedule.
		rep := reconcile(t, st, reconcileBot("nestor", forgeInv, scheduleInv("0 7 * * 1")))
		if rep.Created != 1 || rep.Removed != 0 {
			t.Fatalf("first reconciliation = %+v, want the schedule created and nothing removed", rep)
		}
		// The schedule changes: its row is retired, which proves the removal
		// loop walked this bot's derived rows.
		rep = reconcile(t, st, reconcileBot("nestor", forgeInv, scheduleInv("30 6 * * 1")))
		if rep.Created != 1 || rep.Removed != 1 {
			t.Fatalf("second reconciliation = %+v, want one created and one removed (the old schedule)", rep)
		}
		if _, err := st.Get(ctx, scheduleID("nestor", "0 7 * * 1")); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("the old schedule's row stayed: %v", err)
		}
		// The forge invocation leaves the manifest: its row is the
		// integration's, and stays.
		rep = reconcile(t, st, reconcileBot("nestor", scheduleInv("30 6 * * 1")))
		if rep != (ReconcileReport{}) {
			t.Fatalf("third reconciliation = %+v, want no write", rep)
		}

		after, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("the forge row is gone: %v", err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("the reconciliation rewrote the forge row:\n%+v\nwas\n%+v", after, before)
		}
		if got := boundIDs(t, st, "t1", ForgeBindingForIntegration("int-1")); !reflect.DeepEqual(got, []string{id}) {
			t.Fatalf("the binding holds %v, want the forge row", got)
		}
	})
}

// The labels an event added travel with it wherever it is stored or sent:
// the effect outbox keeps the event in BSON, the bus and the file outbox in
// JSON. An event that added none encodes no key.
func TestEventBSONRoundTrip_AddedLabels(t *testing.T) {
	ev := labelEvent("t1", "Nestor", "run-chaos")
	ev.Labels = []string{"bug", "Nestor", "run-chaos"}
	ev.OccurredAt = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	t.Run("bson", func(t *testing.T) {
		raw, err := bson.Marshal(EffectRow{ID: "row", TenantID: "t1", Event: ev})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Event bson.M `bson:"event"`
		}
		if err := bson.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		stored, ok := doc.Event["added_labels"].(bson.A)
		if !ok || len(stored) != 2 || stored[0] != "Nestor" || stored[1] != "run-chaos" {
			t.Fatalf("the stored event holds added_labels = %#v, want [Nestor run-chaos] under that key", doc.Event["added_labels"])
		}
		var back EffectRow
		if err := bson.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.Event.AddedLabels, ev.AddedLabels) {
			t.Fatalf("added labels round-tripped to %v, want %v", back.Event.AddedLabels, ev.AddedLabels)
		}
		if !reflect.DeepEqual(back.Event.Labels, ev.Labels) {
			t.Fatalf("the subject's labels round-tripped to %v, want %v", back.Event.Labels, ev.Labels)
		}
		// The forge's own event name travels under the payload key every
		// reader of a stored event spells.
		if got, _ := back.Event.Payload["forge_kind"].(string); got != "issues" {
			t.Fatalf("payload forge_kind round-tripped to %#v, want issues (the event stores it under %q)", back.Event.Payload["forge_kind"], PayloadForgeKind)
		}
		// A decoded event is matched like the one that was sent.
		m := Matcher{AddedLabels: []string{"*-chaos"}}
		if !m.Match(back.Event) || MatchedAddedLabel(m, back.Event) != "run-chaos" {
			t.Fatalf("the decoded event is not matched on its added labels: %v", back.Event.AddedLabels)
		}

		none := ev
		none.AddedLabels = nil
		raw, err = bson.Marshal(none)
		if err != nil {
			t.Fatal(err)
		}
		var bare bson.M
		if err := bson.Unmarshal(raw, &bare); err != nil {
			t.Fatal(err)
		}
		if _, present := bare["added_labels"]; present {
			t.Fatalf("an event that added no label encodes the key: %#v", bare["added_labels"])
		}
	})

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if got, ok := doc["added_labels"].([]any); !ok || len(got) != 2 || got[0] != "Nestor" {
			t.Fatalf("the JSON event holds added_labels = %#v, want [Nestor run-chaos] under that key", doc["added_labels"])
		}
		var back Event
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.AddedLabels, ev.AddedLabels) {
			t.Fatalf("added labels round-tripped to %v, want %v", back.AddedLabels, ev.AddedLabels)
		}
	})
}

// A forge row's own fields are stored and served under their keys, and a
// row that carries none of them encodes none: a subscription written before
// they existed reads as it did.
func TestSubscriptionRoundTrip_ForgeFields(t *testing.T) {
	row := forgeRow("drv-1", "t1", "nestor", "int-1", "nestor")
	row.Match.ExcludeAddedLabels = []string{"saboteur"}
	row.OpensMR = true
	row.Enabled = false
	row.DisabledReason = "parked"
	row.ProjectionDigest = "digest-1"
	keys := []string{"forge_binding", "opens_mr", "disabled_reason", "projection_digest"}
	matchKeys := []string{"added_labels", "exclude_added_labels"}

	t.Run("bson", func(t *testing.T) {
		raw, err := bson.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var doc bson.M
		if err := bson.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if _, ok := doc[k]; !ok {
				t.Errorf("the stored row has no %q key: %v", k, doc)
			}
		}
		match, _ := doc["match"].(bson.D)
		stored := map[string]bool{}
		for _, e := range match {
			stored[e.Key] = true
		}
		for _, k := range matchKeys {
			if !stored[k] {
				t.Errorf("the stored matcher has no %q key: %v", k, match)
			}
		}
		var back Subscription
		if err := bson.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, row) {
			t.Fatalf("row round-tripped to\n%+v\nwant\n%+v", back, row)
		}

		raw, err = bson.Marshal(Subscription{ID: "plain", TenantID: "t1", BotID: "b", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		var plain bson.M
		if err := bson.Unmarshal(raw, &plain); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if _, present := plain[k]; present {
				t.Errorf("a row with no forge field encodes %q: it is stored as a row written before the field existed", k)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if _, ok := doc[k]; !ok {
				t.Errorf("the served row has no %q key: %s", k, raw)
			}
		}
		match, _ := doc["match"].(map[string]any)
		for _, k := range matchKeys {
			if _, ok := match[k]; !ok {
				t.Errorf("the served matcher has no %q key: %s", k, raw)
			}
		}
		var back Subscription
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, row) {
			t.Fatalf("row round-tripped to\n%+v\nwant\n%+v", back, row)
		}
	})
}
