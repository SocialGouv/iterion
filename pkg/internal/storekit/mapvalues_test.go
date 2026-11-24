package storekit

import "testing"

// The pin-bearing stores' clear operations share this drop: a pin binds the
// id it carries, so the removal is by VALUE whatever the entry is called,
// and the report tells the caller whether anything actually moved.
func TestDropStringMapValues_DropsByValueAndReports(t *testing.T) {
	m := map[string]string{"anthropic": "key-dead", "openai": "key-alive", "other": "key-dead"}
	if !DropStringMapValues(&m, "key-dead") {
		t.Fatal("a drop that removed entries reported false")
	}
	if len(m) != 1 || m["openai"] != "key-alive" {
		t.Fatalf("m = %v, want only the entry naming no dead record", m)
	}
	if DropStringMapValues(&m, "key-dead") {
		t.Fatal("a drop that removed nothing reported true")
	}
}

// PinNameAddressable is the ONE definition of the name shape every
// targeted pin write addresses: both Mongo clears' $unset arm and the
// routes' write-time guard share it, so a shape accepted here is a shape
// the sweep can address. The table names the boundary, not examples: a dot
// reads as a path separator, a $ anywhere names an operator or a field the
// update operators refuse, and the empty name is an empty trailing path
// component Mongo refuses — a row written before the name guard (the
// validator once checked the tenant only) can hold it.
func TestPinNameAddressable_IsTheShapeTargetedPinWritesAddress(t *testing.T) {
	for _, c := range []struct {
		name        string
		addressable bool
	}{
		{"forge_token", true},
		{"anthropic", true},
		{"", false}, // "<field>." has an empty trailing component; the $unset arm would error, not clear
		{"dead.name", false},
		{".leading", false},
		{"trailing.", false},
		{"$rooted", false},
		{"mid$name", false},
	} {
		if got := PinNameAddressable(c.name); got != c.addressable {
			t.Errorf("PinNameAddressable(%q) = %v, want %v", c.name, got, c.addressable)
		}
	}
}
