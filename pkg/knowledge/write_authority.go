package knowledge

import (
	"context"
	"errors"
	"fmt"
)

// Instance-wide write authority for the `global` memory space.
//
// VisibilityGlobal is the one space that deliberately crosses the tenant
// boundary: every org reads it back through the same ref. A mutation there is
// therefore a PLATFORM act, not a tenant act — and it is authorized by one
// predicate every writer passes through (AuthorizeSpaceWrite, called by each
// MemoryStore adapter at the top of its mutating methods), never by a guard
// repeated per surface.
//
// The authority is a fact of the CONTEXT, granted by whoever checked it: a
// SpaceRef carries no principal, so the ref cannot answer the question. The
// default is fail-closed — a context with no grant cannot mutate the global
// space. Grant sites are deliberately few and greppable
// (WithPlatformWriteAuthority):
//
//   - the REST surface, once requireMemoryWriteAuth has proven the caller's
//     platform authority stands (pkg/server/memory_routes.go);
//   - the local `iterion memory` CLI — the operator at their own console, on
//     their own filesystem store (cmd/iterion/memory.go).
//
// A RUN holds no grant. That is the hole this gate closes: a member of any
// team used to write the instance-wide catalogue, unmetered, by declaring
// `memory: {enabled: true, visibility: global, write: true}` on a claw node,
// because the super-admin gate existed only on REST. The capability is kept,
// not removed — the DSL still accepts the combination (C319 warns the author)
// and the stores still serve it for a granted context. Carrying a super-admin
// run's authority through to the engine inserts HERE, at this one named
// grant, when the run message carries the launching principal's platform
// flag.
type platformWriteAuthorityKey struct{}

// ErrGlobalWriteForbidden is the typed refusal of a global-space mutation by
// a context holding no platform write authority. Callers compare with
// errors.Is; the REST boundary maps it to 403.
var ErrGlobalWriteForbidden = errors.New("knowledge: the global memory space is instance-wide — mutating it requires platform authority")

// WithPlatformWriteAuthority returns a child context authorized to mutate the
// instance-wide `global` space. Call it only where the caller's platform
// authority has just been PROVEN: it is the escape hatch this gate keeps, so
// every call site is part of the security surface.
func WithPlatformWriteAuthority(parent context.Context) context.Context {
	return context.WithValue(parent, platformWriteAuthorityKey{}, true)
}

// PlatformWriteAuthorized reports whether ctx carries the grant.
func PlatformWriteAuthorized(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	granted, _ := ctx.Value(platformWriteAuthorityKey{}).(bool)
	return granted
}

// AuthorizeSpaceWrite is the single gate on a memory MUTATION. Every
// MemoryStore adapter calls it first in WriteDocument and DeleteDocument, so
// the rule holds for every writer — REST, CLI, archive import, and the
// engine's memory_write tool — and the FS and cloud adapters cannot drift
// apart. Tenant-scoped spaces reach only the caller's own org and need no
// grant.
func AuthorizeSpaceWrite(ctx context.Context, ref SpaceRef) error {
	if ref.Visibility != VisibilityGlobal {
		return nil
	}
	if PlatformWriteAuthorized(ctx) {
		return nil
	}
	return fmt.Errorf("%w (space %q)", ErrGlobalWriteForbidden, ref.Name)
}

// InstanceUsageID is the reserved aggregate-counter id under which the
// instance-wide `global` space is metered. The global ref carries no tenant,
// so its bytes met no aggregate ceiling at all in the cloud adapter — only
// the per-document cap — while the FS adapter has always charged its
// whole-tree aggregate. This id is that missing counter's key: a tenant-less
// row in the SAME collection as the per-org aggregates, so one mechanism caps
// both. It cannot collide with a real tenant id (teams are UUIDs, the
// platform scopes are "platform:").
const InstanceUsageID = "__instance__"
