package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// writeTriggers puts a triggers.yaml beside a package written to dir.
func writeTriggers(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, spec.TriggersFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestATriggersFileIsReadAsPartOfThePackage.
//
// `triggers.yaml` was named in this package's own layout doc and read by
// nothing — a format announced to package authors and ignored by the loader,
// which is the worst of the two positions. The declaration now arrives on the
// Package every loader returns.
func TestATriggersFileIsReadAsPartOfThePackage(t *testing.T) {
	dir := writePackageDir(t)
	writeTriggers(t, dir, `schema_version: 1
connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
    description: a comment happened
`)
	p, err := spec.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(p.Triggers) != 1 {
		t.Fatalf("triggers = %+v, want the one the package declares", p.Triggers)
	}
	got := p.Triggers[0]
	if got.ID != "probe.issue.commented" || got.Event != "issue_comment" || got.Description != "a comment happened" {
		t.Errorf("trigger = %+v, want the id, the vendor's event name and the description", got)
	}

	// An ABSENT file is a valid package: most connectors have no inbound half,
	// and a format that demanded one would invalidate every outbound-only
	// package in a catalog.
	bare, err := spec.Load(writePackageDir(t))
	if err != nil {
		t.Fatalf("a package with no triggers.yaml must load: %v", err)
	}
	if len(bare.Triggers) != 0 {
		t.Errorf("triggers = %+v, want none", bare.Triggers)
	}
}

// TestATriggersFileFromANewerIterionIsDiagnosedAsOne is the load-bearing case.
//
// The version is read by a TOLERANT pre-pass before the strict decode. With
// the order reversed — pkg/plugin's manifest, whose version diagnostic is
// unreachable for exactly this reason — a file from a newer iterion fails on
// whatever field it added, and an operator reads "unknown field" where the
// answer is "upgrade iterion".
func TestATriggersFileFromANewerIterionIsDiagnosedAsOne(t *testing.T) {
	dir := writePackageDir(t)
	// A newer version AND a field this build does not know, which is what a
	// future format looks like: the version must win.
	writeTriggers(t, dir, `schema_version: 99
connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
    filter:
      action: created
`)
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("a triggers.yaml from a newer format must be refused")
	}
	if !strings.Contains(err.Error(), "upgrade iterion") {
		t.Errorf("error = %v, want the version diagnosis", err)
	}
	if strings.Contains(err.Error(), "field") {
		t.Errorf("error = %v, want the version diagnosis rather than the strict decoder's unknown-field complaint", err)
	}
}

// TestATriggersFileSaysWhichFormatItIs.
//
// This file's version is its OWN (the overlay's precedent), so a document that
// states none has nothing to compare: the pre-pass above would wave through
// anything a future format adds, and the strict decoder would then refuse it
// by naming a field instead of a version.
func TestATriggersFileSaysWhichFormatItIs(t *testing.T) {
	dir := writePackageDir(t)
	writeTriggers(t, dir, `connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
`)
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("a triggers.yaml with no schema_version must be refused")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error = %v, want it to name the missing version", err)
	}
}

// TestATriggersFieldThisBuildDoesNotKnowIsRefused: the decode is STRICT once
// the version says this reader understands the document. A field silently
// ignored is a declaration an operator believes is honoured.
func TestATriggersFieldThisBuildDoesNotKnowIsRefused(t *testing.T) {
	dir := writePackageDir(t)
	writeTriggers(t, dir, `schema_version: 1
connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
    filter:
      action: created
`)
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("a field this format does not carry must be refused")
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("error = %v, want it to name the field it does not know", err)
	}
}

// TestATriggersFileBelongingToAnotherPackageIsRefused: each document carries
// the connector it is part of, and a mis-copied file would contribute its
// events to the wrong package.
func TestATriggersFileBelongingToAnotherPackageIsRefused(t *testing.T) {
	dir := writePackageDir(t)
	writeTriggers(t, dir, `schema_version: 1
connector: other
triggers:
  - id: probe.issue.commented
    event: issue_comment
`)
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("a triggers.yaml naming another connector must be refused")
	}
	if !strings.Contains(err.Error(), "belongs to connector") {
		t.Errorf("error = %v, want the document-ownership refusal", err)
	}
}

// TestValidateTriggerRefusals — one case per refusal, each stating what it
// PREVENTS, and each mutating ONE thing so no guard can cover another.
func TestValidateTriggerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger spec.Trigger
		wantMsg string
	}{
		{
			// Prevents: a trigger iterion could never match to a delivery. The
			// vendor's event name is the only thing an inbound request carries
			// that says what happened.
			name:    "a trigger naming no event",
			trigger: spec.Trigger{ID: "probe.issue.commented"},
			wantMsg: "declares no event",
		},
		{
			// Prevents: a declaration no workflow could ever name — the
			// silently-inert capability shape.
			name:    "an id no workflow could name",
			trigger: spec.Trigger{ID: "probe.commented", Event: "issue_comment"},
			wantMsg: "not addressable",
		},
		{
			// Prevents: a package declaring events in another package's
			// namespace. The catalog resolves an id by its first segment, so
			// the declaration would be looked for in the other package.
			name:    "an event in another package's namespace",
			trigger: spec.Trigger{ID: "other.issue.commented", Event: "issue_comment"},
			wantMsg: "declares events in its own namespace only",
		},
		{
			// Prevents: one id meaning two things. An operation is called and
			// a trigger is received, but both are named by authored text in
			// one namespace, and a collision this format allowed could never
			// be removed afterwards.
			name:    "an id that is also an operation's",
			trigger: spec.Trigger{ID: "probe.issue.get", Event: "issue_comment"},
			wantMsg: "one id is one thing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPackage()
			p.Triggers = []spec.Trigger{tc.trigger}
			err := p.Validate()
			if err == nil {
				t.Fatal("must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantMsg)
			}
		})
	}

	// The duplicate case needs TWO entries, so it gets its own scenario
	// rather than an arm of the table above.
	t.Run("two triggers sharing an id", func(t *testing.T) {
		p := validPackage()
		p.Triggers = []spec.Trigger{
			{ID: "probe.issue.commented", Event: "issue_comment"},
			{ID: "probe.issue.commented", Event: "issues"},
		}
		err := p.Validate()
		if err == nil {
			t.Fatal("two declarations answering to one name must be refused")
		}
		if !strings.Contains(err.Error(), "duplicate trigger") {
			t.Errorf("error = %v, want the duplicate refusal", err)
		}
	})

	// The falsifier: the valid declaration the refusals surround must pass,
	// or the table would prove only that Validate refuses everything.
	t.Run("a declaration that is honourable", func(t *testing.T) {
		p := validPackage()
		p.Triggers = []spec.Trigger{{ID: "probe.issue.commented", Event: "issue_comment"}}
		if err := p.Validate(); err != nil {
			t.Fatalf("must validate: %v", err)
		}
	})
}

// TestTheShippedForgejoPackageDeclaresItsEvent.
//
// Against the REAL package through the REAL loader — `overlay.LoadPackage` is
// what a catalog serves a launch, generated half plus authored half, ending in
// the complete `Validate`. A format whose only passing example is the one its
// own test writes has not been shown to describe anything, and this is what
// makes `connectors/forgejo/triggers.yaml` part of the gate.
func TestTheShippedForgejoPackageDeclaresItsEvent(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "connectors", "forgejo"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, spec.ConnectorFile)); err != nil {
		t.Skipf("the shipped Forgejo package is not present: %v", err)
	}
	p, err := overlay.LoadPackage(dir)
	if err != nil {
		t.Fatalf("load the shipped package: %v", err)
	}
	if len(p.Triggers) == 0 {
		t.Fatal("the shipped package must declare its inbound event")
	}
	if p.Triggers[0].Event != "issue_comment" {
		t.Errorf("event = %q, want the vendor's own name for it", p.Triggers[0].Event)
	}

	// And the gate JUDGES that declaration rather than carrying it: the same
	// real package with its event blanked must be refused. Without this the
	// assertions above would pass on a loader that read triggers.yaml and
	// handed it to nobody.
	p.Triggers[0].Event = ""
	if err := p.Validate(); err == nil {
		t.Error("the shipped package with its event blanked must be refused")
	}
}
