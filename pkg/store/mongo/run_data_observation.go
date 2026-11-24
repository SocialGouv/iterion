package mongo

import (
	"context"
	"regexp"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// tenantHeaderAliasPattern matches every key a decoder can read as tenant_id:
// JSON EqualFold and BSON ToLower (dotted capital I). The server-side
// projection and the whole-document parse share it, so both count the same
// keys. It ends in `\z`, never `$`: Mongo's PCRE `$` also matches before a
// final newline, which Go's RE2 `$` and the key comparisons do not.
const tenantHeaderAliasPattern = `^[tT][eE][nN][aA][nN][tT]_[iIİ][dD]\z`

var tenantHeaderAlias = regexp.MustCompile(tenantHeaderAliasPattern)

// tenantHeaderEntries projects every tenant_id alias of the run document with
// its cardinality, for retainedCanonicalTenant.
func tenantHeaderEntries() bson.M {
	return bson.M{"$filter": bson.M{
		"input": bson.M{"$objectToArray": "$$ROOT"}, "as": "header",
		"cond": bson.M{"$regexMatch": bson.M{"input": "$$header.k", "regex": tenantHeaderAliasPattern}},
	}}
}

type tenantHeader struct {
	key   string
	value bson.RawValue
}

// canonicalTenant is the strict tenant rule over the alias entries of one run
// document: none is "", exactly one exact "tenant_id" string is that tenant,
// anything else — a duplicate, a case alias, a non-string — is an invalid
// identity, never read leniently.
func canonicalTenant(headers []tenantHeader) (string, error) {
	switch len(headers) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", store.ErrInvalidRunDataIdentity
	}
	if headers[0].key != "tenant_id" {
		return "", store.ErrInvalidRunDataIdentity
	}
	tenant, ok := headers[0].value.StringValueOK()
	if !ok {
		return "", store.ErrInvalidRunDataIdentity
	}
	return tenant, nil
}

// documentCanonicalTenant parses the canonical tenant of a whole run document,
// counting the keys the server-side projection counts.
func documentCanonicalTenant(raw bson.Raw) (string, error) {
	elements, err := raw.Elements()
	if err != nil {
		return "", store.ErrInvalidRunDataIdentity
	}
	var headers []tenantHeader
	for _, e := range elements {
		if key := e.Key(); tenantHeaderAlias.MatchString(key) {
			headers = append(headers, tenantHeader{key: key, value: e.Value()})
		}
	}
	return canonicalTenant(headers)
}

// observesTenant reports whether ctx's binding carries an observed tenant: a
// native read then compares it with the canonical tenant of the document whose
// header it compared. A header-only binding and an unbound ctx read as before.
func observesTenant(ctx context.Context) bool {
	b, bound := store.RunDataBindingFrom(ctx)
	if !bound {
		return false
	}
	_, observed := b.TenantID()
	return observed
}

// checkRunDocumentTenant compares the tenant ctx's binding observed with the
// canonical tenant of the whole run document raw, whose header the caller
// compared (CheckRunDataBinding).
func checkRunDocumentTenant(ctx context.Context, raw bson.Raw) error {
	if !observesTenant(ctx) {
		return nil
	}
	tenant, err := documentCanonicalTenant(raw)
	if err != nil {
		return err
	}
	return store.CheckRunObservedTenant(ctx, tenant)
}
