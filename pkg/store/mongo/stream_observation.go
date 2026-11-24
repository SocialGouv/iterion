package mongo

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// retainedCanonicalTenant parses the canonical tenant from the
// retained_tenant_headers projection (tenantHeaderEntries).
func retainedCanonicalTenant(raw bson.Raw) (string, error) {
	a, ok := raw.Lookup("retained_tenant_headers").ArrayOK()
	if !ok {
		return "", store.ErrInvalidRunDataIdentity
	}
	entries, err := a.Values()
	if err != nil {
		return "", store.ErrInvalidRunDataIdentity
	}
	headers := make([]tenantHeader, 0, len(entries))
	for _, entry := range entries {
		d, ok := entry.DocumentOK()
		if !ok {
			return "", store.ErrInvalidRunDataIdentity
		}
		key, ok := d.Lookup("k").StringValueOK()
		if !ok {
			return "", store.ErrInvalidRunDataIdentity
		}
		headers = append(headers, tenantHeader{key: key, value: d.Lookup("v")})
	}
	return canonicalTenant(headers)
}
func (n *StreamSelection) CheckEventObservationSelection(actual any) error {
	if n == nil || actual != n {
		return store.ErrRunDataCapability
	}
	return nil
}
func (n *StreamSelection) EventStreamObservation() store.RunObservation { return n.observation }
