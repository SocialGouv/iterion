package mongo

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func retainedCanonicalTenant(raw bson.Raw) (string, error) {
	a, ok := raw.Lookup("retained_tenant_headers").ArrayOK()
	if !ok {
		return "", store.ErrInvalidRunDataIdentity
	}
	entries, err := a.Values()
	if err != nil {
		return "", store.ErrInvalidRunDataIdentity
	}
	if len(entries) == 0 {
		return "", nil
	}
	if len(entries) != 1 {
		return "", store.ErrInvalidRunDataIdentity
	}
	d, ok := entries[0].DocumentOK()
	if !ok {
		return "", store.ErrInvalidRunDataIdentity
	}
	key, ok := d.Lookup("k").StringValueOK()
	if !ok || key != "tenant_id" {
		return "", store.ErrInvalidRunDataIdentity
	}
	tenant, ok := d.Lookup("v").StringValueOK()
	if !ok {
		return "", store.ErrInvalidRunDataIdentity
	}
	return tenant, nil
}
func (n *StreamSelection) CheckEventObservationSelection(actual any) error {
	if n == nil || actual != n {
		return store.ErrRunDataCapability
	}
	return nil
}
func (n *StreamSelection) EventStreamObservation() store.RunObservation { return n.observation }
