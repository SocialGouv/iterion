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
// What a trigger does NOT carry, deliberately: no filter, no payload mapping,
// no polling cursor. Those are the trigger-ingress family that
// docs/adr/098-connector-catalog.md defers to a later lot of #1072, and a
// field invented before its reader is a format nobody can change afterwards.
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
}

// triggersDoc is a parsed triggers.yaml.
type triggersDoc struct {
	SchemaVersion int `yaml:"schema_version"`
	// Connector must match the package it sits in, checked like every other
	// document's: a mis-copied file would otherwise contribute its events to
	// the wrong package.
	Connector string    `yaml:"connector"`
	Triggers  []Trigger `yaml:"triggers"`
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
	p.Triggers = doc.Triggers
	return nil
}

// validateTriggers refuses a declaration no inbound half could honour.
//
// Each refusal states what it PREVENTS, because the inbound half is read by a
// delivery arriving from outside: a declaration that is merely ignored is an
// event an operator believes is wired.
func validateTriggers(p *Package) error {
	seen := map[string]bool{}
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
		// Prevents: a trigger iterion could never match to a delivery. The
		// vendor's event name is the only thing an inbound request carries
		// that says what happened.
		if strings.TrimSpace(t.Event) == "" {
			return fmt.Errorf("connector %q: trigger %q declares no event — a delivery is matched by the vendor's own event name, so a trigger without one can never fire",
				p.Connector.ID, t.ID)
		}
		// Prevents: two declarations answering to one name, where which of
		// them a delivery reaches depends on file order.
		if seen[t.ID] {
			return fmt.Errorf("connector %q: duplicate trigger %q", p.Connector.ID, t.ID)
		}
		seen[t.ID] = true
		// Prevents: one id meaning two things. An operation is called and a
		// trigger is received, but both are named by authored text in this
		// package's namespace, and a format that allowed the collision could
		// never remove it afterwards — the lot that gives a `.bot` the syntax
		// to name a trigger would inherit the ambiguity.
		if _, ok := p.Operation(t.ID); ok {
			return fmt.Errorf("connector %q: trigger %q is also an operation id — one id is one thing", p.Connector.ID, t.ID)
		}
	}
	return nil
}
