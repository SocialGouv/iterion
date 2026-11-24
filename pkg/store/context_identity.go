package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const contextIdentityPrefix = "store-v1:"

// ContextIdentityStore declares the logical run-store scope visible to ctx.
// Implementations return identities only: never credentials or a connection
// URI. Fields must be valid UTF-8 so JSON/BSON transport preserves them.
// Encode native path bytes reversibly before constructing a reference.
// A decorator inherits the capability through Unwrapper.
type ContextIdentityStore interface {
	ContextIdentity(context.Context) (ContextRef, error)
}

// AsContextIdentityStore finds the store's optional identity capability.
func AsContextIdentityStore(s RunStore) ContextIdentityStore {
	return capability[ContextIdentityStore](s)
}

// NewStoreContextRef gives an authority-derived identity a distinguishable
// encoding. Historical %T/Root references remain readable but unvalidated;
// their old Mongo constant must never count as evidence of a matching store.
// Kinds are extensible: another store implements the capability, not an engine
// branch. The execution-context wire version stays compatible with old readers.
func NewStoreContextRef(kind, namespace string) ContextRef {
	// Namespaces can be filesystem paths: whitespace and arbitrary bytes are
	// significant. Length-prefix the kind so the two components cannot alias.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(kind), kind, namespace)))
	return ContextRef{
		ID:        fmt.Sprintf("%s%x", contextIdentityPrefix, digest),
		Kind:      kind,
		Namespace: namespace,
		Required:  true,
	}
}

// HasStoreContextIdentity distinguishes the authority encoding from legacy
// references. It does not establish equality with the effective store.
func HasStoreContextIdentity(ref ContextRef) bool {
	return strings.HasPrefix(ref.ID, contextIdentityPrefix)
}

// SameContextRefIdentity compares the declared identity, excluding Required.
// Equality does not certify that either reference came from a store capability:
// an explicit legacy declaration may waive a requirement without being verified.
func SameContextRefIdentity(a, b ContextRef) bool {
	return a.ID != "" && a.Kind != "" && a.ID == b.ID && a.Kind == b.Kind &&
		a.Namespace == b.Namespace && a.Revision == b.Revision
}

// ResolveStoreContextRef reads the identity through the common capability
// seam. Unknown stores retain the legacy representation for compatibility,
// without claiming that a type name and an empty Root identify a backend.
func ResolveStoreContextRef(ctx context.Context, s RunStore) (ContextRef, error) {
	if identity := AsContextIdentityStore(s); identity != nil {
		ref, err := identity.ContextIdentity(ctx)
		if err != nil {
			return ContextRef{}, err
		}
		for _, field := range []struct{ name, value string }{
			{"ID", ref.ID}, {"kind", ref.Kind}, {"namespace", ref.Namespace}, {"revision", ref.Revision},
		} {
			if !utf8.ValidString(field.value) {
				return ContextRef{}, fmt.Errorf("store: identity capability %s must be valid UTF-8", field.name)
			}
		}
		if ref.Kind == "" || !HasStoreContextIdentity(ref) {
			return ContextRef{}, fmt.Errorf("store: identity capability must return a versioned store reference")
		}
		ref.Required = true
		return ref, nil
	}
	kind, namespace := fmt.Sprintf("%T", s), ""
	if s != nil {
		namespace = s.Root()
	}
	// Keep the historical ID over the raw Root, but carry its namespace without
	// JSON replacing native non-UTF-8 bytes and breaking an explicit opt-out.
	return ContextRef{ID: StableContextID(kind, namespace), Kind: kind, Namespace: pathContextNamespace(namespace), Required: true}, nil
}

// pathContextNamespace is reversible and escapes literal percent signs too:
// a native byte 0xff must stay distinct from a directory literally named %FF.
func pathContextNamespace(path string) string {
	return (&url.URL{Path: path}).EscapedPath()
}

// ContextIdentity names the effective filesystem root, independent of a
// relative spelling or a symlink alias. Resolution errors cannot certify a
// namespace that this store cannot currently address.
func (s *FilesystemRunStore) ContextIdentity(_ context.Context) (ContextRef, error) {
	root, err := filepath.Abs(s.root)
	if err != nil {
		return ContextRef{}, fmt.Errorf("store: resolve identity root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return ContextRef{}, fmt.Errorf("store: resolve identity root: %w", err)
	}
	return NewStoreContextRef("filesystem", pathContextNamespace(root)), nil
}
