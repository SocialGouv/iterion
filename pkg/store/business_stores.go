package store

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

type businessStoreKey struct{ kind, namespace, id string }

func businessKey(ref ContextRef) businessStoreKey {
	return businessStoreKey{ref.Kind, ref.Namespace, ref.ID}
}

func compareBusinessStores(a, b ContextRef) int {
	return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.ID, b.ID), strings.Compare(a.Revision, b.Revision))
}

func normalizeBusinessStores(refs []ContextRef) ([]ContextRef, error) {
	out := slices.Clone(refs)
	seen := make(map[businessStoreKey]struct{}, len(out))
	for i, ref := range out {
		if strings.TrimSpace(ref.ID) == "" {
			return nil, fmt.Errorf("store: execution context business_stores[%d].id is required", i)
		}
		if strings.TrimSpace(ref.Kind) == "" {
			return nil, fmt.Errorf("store: execution context business_stores[%d].kind is required", i)
		}
		key := businessKey(ref)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("store: duplicate execution context business store %q", ref.ID)
		}
		seen[key] = struct{}{}
	}
	slices.SortFunc(out, compareBusinessStores)
	return out, nil
}

// MergeBusinessStores normalizes each declaration source before combining it.
// Identical inherited declarations are idempotent; conflicting requirements or
// revisions are refused, and duplicates inside one source remain invalid.
// Inputs are never sorted or extended in place.
func MergeBusinessStores(sources ...[]ContextRef) ([]ContextRef, error) {
	var out []ContextRef
	seen := make(map[businessStoreKey]ContextRef)
	for _, source := range sources {
		normalized, err := normalizeBusinessStores(source)
		if err != nil {
			return nil, err
		}
		for _, ref := range normalized {
			key := businessKey(ref)
			if previous, ok := seen[key]; ok {
				if previous != ref {
					return nil, fmt.Errorf("store: conflicting execution context business store %q", ref.ID)
				}
				continue
			}
			seen[key] = ref
			out = append(out, ref)
		}
	}
	slices.SortFunc(out, compareBusinessStores)
	return out, nil
}
