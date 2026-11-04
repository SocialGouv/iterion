package storekit

import "strings"

// PinNameAddressable reports whether a stored pin name can be addressed as
// a Mongo path ("<field>.<name>"). A name holding a dot reads as a nested
// path, so the targeted write matches nothing stored under the literal key;
// a $ names an operator or a field update paths refuse; the empty name
// makes a path with an empty trailing component ("<field>."), which Mongo
// refuses outright — the clear would error instead of clearing. The routes
// refuse all three at write (validateCredentialPinKeys); the Mongo clears
// rewrite the subdocument for names stored before that guard instead of
// addressing a path that cannot match.
func PinNameAddressable(name string) bool {
	return name != "" && !strings.ContainsAny(name, ".$")
}

// DropStringMapValues removes every entry of *m whose value equals dead,
// reporting whether it removed any. A credential pin binds the id it carries
// — the map key (a provider, a workflow secret name) is display, not
// identity — so the pin-bearing stores' clear operations (webhooks
// ConfigStore, trigger SubscriptionStore) share this value-scoped drop
// rather than one wholesale-set helper per store.
func DropStringMapValues(m *map[string]string, dead string) bool {
	dropped := false
	for k, v := range *m {
		if v == dead {
			delete(*m, k)
			dropped = true
		}
	}
	return dropped
}
