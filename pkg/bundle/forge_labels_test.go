package bundle

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// forgeInvocationManifest is a manifest whose one invocation is a kind=forge
// on event, carrying the `labels:` line given (empty for none).
func forgeInvocationManifest(event, labelsLine string) string {
	body := "name: probe\ninvocations:\n  - kind: forge\n    forge:\n      event: " + event + "\n"
	if labelsLine != "" {
		body += "      " + labelsLine + "\n"
	}
	return body
}

// A label starts a bot on a labelled issue and on no other forge event:
// forge.labels is read on issue_labeled, and refused — typed, naming the
// event — everywhere else, where nothing would ever read it.
func TestForgeLabels_OnlyOnIssueLabeled(t *testing.T) {
	m, err := DecodeManifest([]byte(forgeInvocationManifest(ForgeEventIssueLabeled, `labels: [nestor, "*-chaos", "*"]`)), "probe")
	if err != nil {
		t.Fatalf("labels on %s are refused: %v", ForgeEventIssueLabeled, err)
	}
	labels, aliased := m.Invocations[0].ForgeTriggerLabels()
	if want := []string{"nestor", "*-chaos", "*"}; !reflect.DeepEqual(labels, want) || aliased {
		t.Fatalf("ForgeTriggerLabels = %v, aliased %v; want %v read from forge.labels itself", labels, aliased, want)
	}
	// The labels handed out are a copy: a caller that reorders them does not
	// reorder the manifest every other reader shares.
	labels[0] = "rewritten"
	if got := m.Invocations[0].Forge.Labels[0]; got != "nestor" {
		t.Fatalf("ForgeTriggerLabels handed out the manifest's own slice: the manifest now reads %q", got)
	}

	var others []string
	for event := range KnownForgeEvents {
		if event != ForgeEventIssueLabeled {
			others = append(others, event)
		}
	}
	sort.Strings(others)
	if len(others) == 0 {
		t.Fatal("no forge event other than issue_labeled is known: the refusal below runs on nothing")
	}
	for _, event := range others {
		t.Run("refused on "+event, func(t *testing.T) {
			_, err := DecodeManifest([]byte(forgeInvocationManifest(event, "labels: [nestor]")), "probe")
			if err == nil {
				t.Fatalf("labels on %s were accepted: nothing reads them there", event)
			}
			var refused *ForgeLabelsError
			if !errors.As(err, &refused) || !errors.Is(err, ErrForgeLabels) {
				t.Fatalf("the refusal is not a *ForgeLabelsError of class ErrForgeLabels: %v", err)
			}
			if refused.Event != event || refused.Path != "invocations[0].forge.labels" {
				t.Fatalf("the refusal names event %q at %q, want %q at invocations[0].forge.labels", refused.Event, refused.Path, event)
			}
			for _, want := range []string{event, ForgeEventIssueLabeled, "invocations[0].forge.labels"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
		})
		// A list that declares nothing is not a claim: the event keeps
		// loading with it.
		t.Run("an empty list on "+event, func(t *testing.T) {
			if _, err := DecodeManifest([]byte(forgeInvocationManifest(event, "labels: []")), "probe"); err != nil {
				t.Fatalf("an empty labels list on %s is refused: %v", event, err)
			}
		})
	}
}

// The label grammar is the webhook matcher's — "*", a literal, a leading "*"
// for a suffix — and the one thing it cannot read is an empty pattern, which
// matches no label: refused where it is written, on an invocation and on the
// `forge:` block alike. Each site has its own case, so neither refusal can
// stand in for the other.
func TestForgeLabels_ABlankPatternIsRefused(t *testing.T) {
	blockManifest := func(labelsLine string) string {
		return "name: probe\nforge:\n  events: [issue_labeled]\n  " + labelsLine + "\n"
	}
	for name, tc := range map[string]struct {
		body, path string
	}{
		"empty, on an invocation": {forgeInvocationManifest(ForgeEventIssueLabeled, `labels: [nestor, ""]`), "invocations[0].forge.labels[1]"},
		"blank, on an invocation": {forgeInvocationManifest(ForgeEventIssueLabeled, `labels: ["   "]`), "invocations[0].forge.labels[0]"},
		"empty, on the block":     {blockManifest(`labels: [nestor, ""]`), "forge.labels[1]"},
		"blank, on the block":     {blockManifest(`labels: [" \t "]`), "forge.labels[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeManifest([]byte(tc.body), "probe")
			var refused *ForgeLabelsError
			if !errors.As(err, &refused) {
				t.Fatalf("a pattern that matches no label was not refused as a *ForgeLabelsError: %v", err)
			}
			if refused.Path != tc.path || refused.Event != "" {
				t.Fatalf("the refusal names %q (event %q), want %q and no event", refused.Path, refused.Event, tc.path)
			}
		})
	}
	// Every shape the grammar does read loads, on both sites: nothing here
	// narrows it.
	for _, labelsLine := range []string{`labels: ["*"]`, `labels: [nestor]`, `labels: ["*-chaos"]`, `labels: ["  Spaced  "]`, `labels: ["chaos-*"]`} {
		if _, err := DecodeManifest([]byte(forgeInvocationManifest(ForgeEventIssueLabeled, labelsLine)), "probe"); err != nil {
			t.Errorf("an invocation's %s is refused: %v", labelsLine, err)
		}
		m, err := DecodeManifest([]byte(blockManifest(labelsLine)), "probe")
		if err != nil {
			t.Errorf("the block's %s is refused: %v", labelsLine, err)
			continue
		}
		if len(m.Forge.Labels) != 1 {
			t.Errorf("the block's %s decoded as %v", labelsLine, m.Forge.Labels)
		}
	}
}

// A forge invocation's identity is what it says — its event, its actions and
// the labels that start it — read the way the matcher reads them: any-of
// sets, labels case-insensitive and trimmed. A cosmetic rewrite keeps the
// key; a change of event, actions or labels moves it.
func TestInvocationKey_Forge_StableUnderOrderAndCase(t *testing.T) {
	forge := func(event string, actions, labels []string) []Invocation {
		return []Invocation{{Kind: InvocationKindForge, Forge: &InvocationForge{Event: event, Actions: actions, Labels: labels}}}
	}
	base := forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"Nestor", "*-chaos"})
	key := InvocationKey(base, 0)
	if !regexp.MustCompile(`^forge:issue_labeled\|a:labeled,opened\|l:[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("forge key = %q, want forge:<event>|a:<sorted actions>|l:<16 hex>", key)
	}
	for name, same := range map[string][]Invocation{
		"actions and labels reordered": forge(ForgeEventIssueLabeled, []string{"labeled", "opened"}, []string{"*-chaos", "Nestor"}),
		"labels in another case":       forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"NESTOR", "*-Chaos"}),
		"labels padded":                forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{" nestor ", "*-chaos\t"}),
		"a label repeated":             forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"nestor", "*-chaos", "Nestor"}),
	} {
		if got := InvocationKey(same, 0); got != key {
			t.Errorf("%s: key %q, want the same identity %q", name, got, key)
		}
	}
	for name, other := range map[string][]Invocation{
		"another label":          forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"saboteur", "*-chaos"}),
		"one label fewer":        forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"nestor"}),
		"no label":               forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, nil),
		"another action":         forge(ForgeEventIssueLabeled, []string{"opened"}, []string{"Nestor", "*-chaos"}),
		"another event":          forge(ForgeEventPullRequest, []string{"opened", "labeled"}, []string{"Nestor", "*-chaos"}),
		"labels joined into one": forge(ForgeEventIssueLabeled, []string{"opened", "labeled"}, []string{"*-chaos,nestor"}),
	} {
		if got := InvocationKey(other, 0); got == key {
			t.Errorf("%s: kept the identity %q", name, key)
		}
	}
	// A forge invocation that declares neither actions nor labels still has
	// an identity, apart from the one that declares a label.
	bare := InvocationKey(forge(ForgeEventPullRequest, nil, nil), 0)
	if !regexp.MustCompile(`^forge:pull_request\|a:\|l:[0-9a-f]{16}$`).MatchString(bare) {
		t.Errorf("a bare forge key = %q, want its three parts", bare)
	}
	if got := InvocationKey([]Invocation{{Kind: InvocationKindForge}}, 0); got != "" {
		t.Errorf("a forge invocation with no forge block keyed %q: it derives nothing", got)
	}
	// The key is computed on copies: the manifest's lists keep their order.
	if base[0].Forge.Actions[0] != "opened" || base[0].Forge.Labels[0] != "Nestor" {
		t.Fatalf("the key reordered or rewrote the manifest in place: %+v", base[0].Forge)
	}
}

// Two forge invocations that derive one key load: a manifest already stored
// may declare such a pair, and refusing it would stop its bot from loading.
// The pair is warned, the later invocation named, and it derives once. Every
// other kind keeps its refusal — the tolerance is the forge kind's alone.
func TestForgeDuplicateKey_WarnsAndStillParses(t *testing.T) {
	body := `name: probe
invocations:
  - kind: forge
    mode: direct
    forge: { event: pull_request, actions: [opened, reopened] }
  - kind: forge
    mode: direct
    forge: { event: issue_labeled, labels: [nestor, "*-chaos"] }
  - kind: forge
    mode: board
    forge: { event: pull_request, actions: [reopened, opened] }
  - kind: forge
    mode: board
    forge: { event: issue_labeled, labels: ["*-CHAOS", Nestor] }
`
	m, err := DecodeManifest([]byte(body), "probe")
	if err != nil {
		t.Fatalf("a manifest with two forge invocations of one key no longer parses: %v", err)
	}
	if len(m.Invocations) != 4 {
		t.Fatalf("decoded %d invocations, want the 4 declared", len(m.Invocations))
	}
	warnings := ForgeDuplicateKeyWarnings(m.Invocations)
	if len(warnings) != 2 {
		t.Fatalf("got %d duplicate warnings, want one per repeated key (2): %q", len(warnings), warnings)
	}
	for i, want := range []struct{ later, first, event string }{
		{"invocations[2]", "invocations[0]", ForgeEventPullRequest},
		{"invocations[3]", "invocations[1]", ForgeEventIssueLabeled},
	} {
		for _, part := range []string{want.later + ":", "two forge invocations derive the same key: the second is ignored", want.first, want.event} {
			if !strings.Contains(warnings[i], part) {
				t.Errorf("warning %d = %q, want it to name %q", i, warnings[i], part)
			}
		}
	}
	// The pair derives once, and the first declared answers: direct, not
	// the board mode of the invocation that repeats it.
	derived, routeWarnings := ForgeIssueInvocations(m.Forge, m.Invocations)
	if len(derived) != 1 {
		t.Fatalf("the duplicated issue_labeled key derived %d routes, want 1: %+v", len(derived), derived)
	}
	if mode, err := derived[0].Inv.EffectiveMode(); err != nil || mode != ExecutionDirect {
		t.Errorf("the derived route runs %q (%v), want the first declared invocation's direct", mode, err)
	}
	if len(routeWarnings) != 1 || !strings.Contains(routeWarnings[0], "invocations[3]:") || !strings.Contains(routeWarnings[0], "the second is ignored") {
		t.Errorf("the route warnings do not name the ignored invocation: %q", routeWarnings)
	}

	// A manifest with no repeated key is told nothing.
	clean, err := DecodeManifest([]byte(forgeInvocationManifest(ForgeEventIssueLabeled, "labels: [nestor]")), "probe")
	if err != nil {
		t.Fatal(err)
	}
	if got := ForgeDuplicateKeyWarnings(clean.Invocations); len(got) != 0 {
		t.Errorf("a manifest with one forge invocation is warned: %q", got)
	}

	// The tolerance is the forge kind's alone: a repeated schedule is still
	// refused at parse time.
	_, err = DecodeManifest([]byte("name: probe\ninvocations:\n  - kind: schedule\n    schedule: { suggested_cron: \"0 7 * * 1\" }\n  - kind: schedule\n    schedule: { suggested_cron: \"0  7 * * 1\" }\n"), "probe")
	if err == nil || !strings.Contains(err.Error(), "derives the same subscription") {
		t.Fatalf("two schedule invocations of one cron were not refused: %v", err)
	}
}

// The labels reach the studio under the key the manifest spells, and a forge
// invocation that declares none keeps the wire shape it has without them.
func TestForgeLabels_JSONWireShape(t *testing.T) {
	with, err := json.Marshal(Invocation{Kind: InvocationKindForge, Forge: &InvocationForge{Event: ForgeEventIssueLabeled, Labels: []string{"nestor"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"forge","forge":{"event":"issue_labeled","labels":["nestor"]}}`; string(with) != want {
		t.Fatalf("wire JSON = %s, want %s", with, want)
	}
	without, err := json.Marshal(Invocation{Kind: InvocationKindForge, Forge: &InvocationForge{Event: ForgeEventPullRequest, Actions: []string{"opened"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"forge","forge":{"event":"pull_request","actions":["opened"]}}`; string(without) != want {
		t.Fatalf("wire JSON of an invocation without labels = %s, want it unchanged: %s", without, want)
	}
}
