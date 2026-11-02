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
