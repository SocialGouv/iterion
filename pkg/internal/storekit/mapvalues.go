package storekit

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
