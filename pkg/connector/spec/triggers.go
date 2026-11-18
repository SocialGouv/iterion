package spec

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	yaml "go.yaml.in/yaml/v2"
)

// TriggersSchemaVersion is the triggers.yaml document's OWN format version.
//
// Separate from the connector package's, for the reason the overlay's own
// constant records: triggers.yaml has its own fields and its own history, and
// borrowing the package constant would let a package-format bump silently
// widen what a trigger file may declare. A version this reader has no meaning
// for must reach the "upgrade iterion" diagnosis rather than the strict
// decoder's "unknown field".
const TriggersSchemaVersion = 1

// Trigger is one inbound event a connector package declares it can receive.
//
// DECLARED BY THE PACKAGE rather than by engine code, which is the same offer
// the outbound half makes: a vendor's event reaches a bot because a package
// says the event exists, so the second connector costs a declaration instead
// of an arm in a switch.
//
// CURATED, like overlay.yaml and unlike ops/: no vendor description states
// which of its webhooks an engine will believe. So Write does not emit it —
// which is also what makes a declaration survive an ordinary `iterion
// connectors gen`, since only ops/ is rewritten there.
//
// Beyond its id, event and description, a trigger declares how a delivery
// is read (ADR-168 §5): the outputs extracted from it, the filters an
// operator may choose, the loop guard that drops the bot's own events, and a
// recorded sample its declarations are proven against. The package's
// `ingress:` block is their only reader, so a trigger declaring any of them
// requires one.
type Trigger struct {
	// ID is how a workflow names this event: `<connector>.<resource>.<verb>`,
	// the SAME rule as an operation id (AddressableOperationID), because both
	// are addressed from authored text by the same reader.
	ID string `yaml:"id" json:"id"`
	// Event is the vendor's OWN name for it, exactly as a delivery carries it
	// — `issue_comment` in Forgejo's `X-Forgejo-Event`.
	//
	// Kept apart from ID because a vendor's spelling is not addressable (it
	// names no connector, and two vendors reuse the same word), and because
	// the day a vendor renames an event this is the only line that moves.
	Event string `yaml:"event" json:"event"`
	// Description is the one line an operator reads.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// DisplayName is the catalog's label for the event.
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	// Delivery is how the trigger's events reach iterion: webhook (empty,
	// matched by Event on an authenticated delivery) or poll (read by its
	// own poller through Poll).
	Delivery string `yaml:"delivery,omitempty" json:"delivery,omitempty"`
	// Poll is a polled trigger's query: the operation, its parameters, and
	// how items, identities and the checkpoint are read (poll.go).
	Poll *TriggerPoll `yaml:"poll,omitempty" json:"poll,omitempty"`
	// Outputs are the named values extracted from a delivery: what the
	// studio offers for mapping and what a run receives by default.
	Outputs []TriggerOutput `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	// Filters are the values an operator may filter deliveries on.
	Filters []TriggerFilter `yaml:"filters,omitempty" json:"filters,omitempty"`
	// Ignore drops a delivery before any subscription is read.
	Ignore string `yaml:"ignore,omitempty" json:"ignore,omitempty"`
	// Sample is a recorded delivery under samples/, read at load into
	// SampleBody.
	Sample     string `yaml:"sample,omitempty" json:"sample,omitempty"`
	SampleBody []byte `yaml:"-" json:"-"`
}

// triggersDoc is a parsed triggers.yaml.
type triggersDoc struct {
	SchemaVersion int `yaml:"schema_version"`
	// Connector must match the package it sits in, checked like every other
	// document's: a mis-copied file would otherwise contribute its events to
	// the wrong package.
	Connector string    `yaml:"connector"`
	Ingress   *Ingress  `yaml:"ingress,omitempty"`
	Triggers  []Trigger `yaml:"triggers"`
}

// WebhookTrigger is the one reading of "which trigger does this delivery's
// event name": the first webhook trigger declaring exactly that vendor event.
// A polled trigger is fed by its poller alone and is never a delivery's, and
// an empty name — a delivery whose event header is absent — names nothing,
// rather than every trigger that declares none.
func WebhookTrigger(triggers []Trigger, event string) (*Trigger, bool) {
	if event == "" {
		return nil, false
	}
	for i := range triggers {
		if !triggers[i].Polled() && triggers[i].Event == event {
			return &triggers[i], true
		}
	}
	return nil, false
}

// DeliverableTriggers are the triggers a delivery can reach: the webhook
// triggers of a package that declares its ingress. A polled trigger is fed
// by its poller and never by a delivery. The catalogue offers these and the
// ingress bind requires one, so the two cannot disagree on what is pluggable.
func (p *Package) DeliverableTriggers() []Trigger {
	if p.Ingress == nil {
		return nil
	}
	var out []Trigger
	for _, t := range p.Triggers {
		if !t.Polled() {
			out = append(out, t)
		}
	}
	return out
}

// readTriggers loads the package's declared events, if it declares any.
//
// An ABSENT file is not an error: most packages have no inbound half, and a
// format that demanded one would make every outbound-only package invalid.
// Anything else — unreadable, malformed, a version this build cannot read —
// is, for the same reason a tier that is present and unreadable is not an
// absent one: a declaration silently dropped is an event the engine will
// never believe, with nothing saying so.
func readTriggers(fsys fs.FS, dir string, p *Package) error {
	body, err := fs.ReadFile(fsys, path.Join(dir, TriggersFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("spec: read %s: %w", TriggersFile, err)
	}
	// The tolerant PRE-PASS first, strict decode second — the order the
	// package doc insists on, and the one pkg/plugin gets backwards.
	var probe versionProbe
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("spec: %s is not valid YAML: %w", TriggersFile, err)
	}
	if probe.SchemaVersion > TriggersSchemaVersion {
		return fmt.Errorf("spec: %s declares schema_version %d, newer than supported %d (upgrade iterion)", TriggersFile, probe.SchemaVersion, TriggersSchemaVersion)
	}
	if probe.SchemaVersion <= 0 {
		// REQUIRED, unlike the package documents that inherit the package's
		// version: this file's version is its own, so a document that does
		// not state one has no version to compare and the pre-pass above
		// would wave through anything a future format adds.
		return fmt.Errorf("spec: %s declares no schema_version (the current one is %d)", TriggersFile, TriggersSchemaVersion)
	}
	var doc triggersDoc
	if err := yaml.UnmarshalStrict(body, &doc); err != nil {
		return fmt.Errorf("spec: parse %s: %w", TriggersFile, err)
	}
	if err := checkDocConnector(TriggersFile, doc.Connector, p.Connector.ID); err != nil {
		return err
	}
	// A sample is read with the document, from the same tree, so every tier
	// that carries the package carries what its declarations are proven
	// against.
	for i, t := range doc.Triggers {
		if t.Sample == "" {
			continue
		}
		if !validSamplePath(t.Sample) {
			return fmt.Errorf("spec: %s: trigger %q sample %q is not a samples/<name>.json path inside the package", TriggersFile, t.ID, t.Sample)
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, t.Sample))
		if err != nil {
			return fmt.Errorf("spec: %s: trigger %q sample: %w", TriggersFile, t.ID, err)
		}
		doc.Triggers[i].SampleBody = b
	}
	p.Ingress = doc.Ingress
	p.Triggers = doc.Triggers
	return nil
}

// validateTriggers refuses a declaration no inbound half could honour.
//
// Each refusal states what it PREVENTS, because the inbound half is read by a
// delivery arriving from outside: a declaration that is merely ignored is an
// event an operator believes is wired.
func validateTriggers(p *Package) error {
	if err := validateIngress(p); err != nil {
		return err
	}
	seen := map[string]bool{}
	events := map[string]string{}
	for _, t := range p.Triggers {
		// Prevents: a declaration no workflow could ever name, which is the
		// silently-inert capability shape this repo calls a defect.
		if !AddressableOperationID(t.ID) {
			return fmt.Errorf("connector %q: trigger id %q is not addressable — it must be <connector>.<resource>.<verb>, lowercase segments",
				p.Connector.ID, t.ID)
		}
		// Prevents: a package declaring events in another package's
		// namespace. The catalog resolves an id by its FIRST segment, so such
		// a declaration would be looked for in the other package and found
		// nowhere.
		if first := strings.Split(t.ID, ".")[0]; first != p.Connector.ID {
			return fmt.Errorf("connector %q: trigger %q names connector %q — a package declares events in its own namespace only",
				p.Connector.ID, t.ID, first)
		}
		// Prevents: a delivery mode nothing serves, judged before anything
		// that depends on it so the refusal names the value at fault.
		if t.Delivery != "" && t.Delivery != DeliveryWebhook && t.Delivery != DeliveryPoll {
			return fmt.Errorf("connector %q: trigger %q: delivery %q is not %s or %s",
				p.Connector.ID, t.ID, t.Delivery, DeliveryWebhook, DeliveryPoll)
		}
		// Prevents: a webhook trigger iterion could never match to a
		// delivery. The vendor's event name is the only thing an inbound
		// request carries that says what happened. A polled trigger is named
		// by its poller instead, and validatePoll refuses it an event.
		if !t.Polled() && strings.TrimSpace(t.Event) == "" {
			return fmt.Errorf("connector %q: trigger %q declares no event — a delivery is matched by the vendor's own event name, so a trigger without one can never fire",
				p.Connector.ID, t.ID)
		}
		// Prevents: two declarations answering to one name, where which of
		// them a delivery reaches depends on file order.
		if seen[t.ID] {
			return fmt.Errorf("connector %q: duplicate trigger %q", p.Connector.ID, t.ID)
		}
		seen[t.ID] = true
		// Prevents: a trigger the catalogue offers and no delivery reaches.
		// A delivery is routed by its event name to ONE webhook trigger, so a
		// second trigger on the same event is pluggable and never fires; a
		// trigger that narrows the event says so with its filters.
		if !t.Polled() {
			if first, dup := events[t.Event]; dup {
				return fmt.Errorf("connector %q: triggers %q and %q both declare event %q — a delivery reaches one trigger, so the second would never fire; narrow one trigger with filters instead",
					p.Connector.ID, first, t.ID, t.Event)
			}
			events[t.Event] = t.ID
		}
		// Prevents: one id meaning two things. An operation is called and a
		// trigger is received, but both are named by authored text in this
		// package's namespace, and a format that allowed the collision could
		// never remove it afterwards — the lot that gives a `.bot` the syntax
		// to name a trigger would inherit the ambiguity.
		if _, ok := p.Operation(t.ID); ok {
			return fmt.Errorf("connector %q: trigger %q is also an operation id — one id is one thing", p.Connector.ID, t.ID)
		}
		if err := validatePoll(p, t); err != nil {
			return err
		}
		if err := validateTriggerExtraction(p, t); err != nil {
			return err
		}
	}
	return nil
}
