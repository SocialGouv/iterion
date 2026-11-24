package store

import (
	"context"
	"errors"
)

// checkRunJSONObservation compares the tenant ctx's binding observed with the
// canonical tenant of the same run.json bytes whose header the caller compared
// (CheckRunDataBinding). The tenant comes from the strict parser: a duplicate
// or case-alias tenant_id is an invalid identity, never read leniently. An
// unbound ctx and a header-only binding read as before, without a second parse.
func checkRunJSONObservation(ctx context.Context, raw []byte) error {
	b, bound := RunDataBindingFrom(ctx)
	if !bound {
		return nil
	}
	if _, observed := b.TenantID(); !observed {
		return nil
	}
	tenant, err := streamCanonicalTenant(ctx, raw)
	if err != nil {
		if filesystemContextErrorOnly(err) {
			return err
		}
		return errors.Join(ErrInvalidRunDataIdentity, err, ctx.Err())
	}
	return CheckRunObservedTenant(ctx, tenant)
}
