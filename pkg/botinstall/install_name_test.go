package botinstall

import (
	"encoding/json"
	"strings"
	"testing"
)

// An install name is a routable bot identity: the lock key botlock validates,
// the catalogue's assignee cell, the directory under .botz/ and a value this
// package's callers print. The charset is botlock's own; anything outside it
// (a newline, a path separator, a leading dash) cannot be a bot anyway, and a
// newline in particular would let a pulled bundle's manifest write a line of
// the installing command's output.
func TestValidateInstallName_HoldsTheBotNameCharset(t *testing.T) {
	for _, name := range []string{"nexie", "feature_dev", "whats-next", "a.b.c", "A1"} {
		if err := validateInstallName(name); err != nil {
			t.Errorf("validateInstallName(%q) = %v, want accepted", name, err)
		}
	}
	for _, name := range []string{
		"", " ", ".", "..", "a/b", `a\b`, " spaced", "café", "-lead",
		"evil\n  ! catalog not refreshed: nothing, all good",
	} {
		if err := validateInstallName(name); err == nil {
			t.Errorf("validateInstallName(%q) = nil, want refused", name)
		}
	}
}

// TestRemoveResult_CrossesNoWire: the doc comment says the type crosses no
// wire — each consumer renders the cause in its own shape — and the tag
// enforces it: a future json.Marshal emits no key no consumer reads.
func TestRemoveResult_CrossesNoWire(t *testing.T) {
	b, err := json.Marshal(RemoveResult{CatalogError: "botregistry: stat catalog template: too many levels of symbolic links"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Fatalf("RemoveResult leaked to JSON as %s; no consumer parses that shape", b)
	}
}

// TestInstallResult_KeepsTheCatalogErrorOnTheWire: the sibling field MUST
// cross — this pins the difference the two tags state.
func TestInstallResult_KeepsTheCatalogErrorOnTheWire(t *testing.T) {
	b, err := json.Marshal(Result{Name: "n", CatalogError: "cause"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"catalog_error":"cause"`) {
		t.Fatalf("Result.CatalogError must stay on the wire (the install surfaces rely on it): %s", b)
	}
}
