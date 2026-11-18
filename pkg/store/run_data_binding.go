package store

import (
	"context"
	"errors"
	"fmt"
)

// RunDataProtocol selects data ownership independently of numeric value format.
// These declarations do not enable protocol-one birth on any store backend.
type RunDataProtocol int

const (
	HistoricalRunData  RunDataProtocol = 0
	IncarnationRunData RunDataProtocol = 1
)

var (
	ErrInvalidRunDataIdentity     = errors.New("store: invalid run data identity")
	ErrUnsupportedRunDataProtocol = errors.New("store: unsupported run data protocol")
	ErrRunDataIdentityChanged     = errors.New("store: run data identity changed")
	ErrRunDataBindingRequired     = errors.New("store: original run data binding required")
)

// RunDataHeader contains no execution payload or execution authority. Fresh
// admission may observe it; a delayed operation must keep its original binding.
type RunDataHeader struct {
	RunID    string
	Format   ValueFormat
	Protocol RunDataProtocol
	BirthID  string
}

func (h RunDataHeader) validate() (RunDataHeader, error) {
	if h.RunID == "" {
		return h, fmt.Errorf("%w: empty run ID", ErrInvalidRunDataIdentity)
	}
	format, err := NormalizeValueFormat(int(h.Format))
	if err != nil {
		return h, err
	}
	h.Format = format
	switch h.Protocol {
	case HistoricalRunData:
	case IncarnationRunData:
		if h.BirthID == "" {
			return h, fmt.Errorf("%w: incarnation data requires a birth identity", ErrInvalidRunDataIdentity)
		}
	default:
		return h, fmt.Errorf("%w: %d", ErrUnsupportedRunDataProtocol, h.Protocol)
	}
	return h, nil
}

// RunDataBinding is a copied original observation, not a grant to execute or
// modify a run. Its private state cannot change through a mutable Run/header.
// A zero value is not an observation of historical absence. tenantObserved
// tells an observed "" (a run that records no tenant) from a binding that
// carries no tenant at all (a header-only capture).
type RunDataBinding struct {
	header         RunDataHeader
	tenant         string
	tenantObserved bool
	observed       bool
}

type runDataBindingKey struct{}

// CaptureRunDataBinding captures a header only: the binding observes no tenant.
func CaptureRunDataBinding(header RunDataHeader) (RunDataBinding, error) {
	h, err := header.validate()
	if err != nil {
		return RunDataBinding{}, err
	}
	return RunDataBinding{header: h, observed: true}, nil
}

// CaptureRunDataObservation captures a present observation: its header and the
// tenant its store recorded, "" included. An absent parent has no header to
// bind and is refused, as is an unknown observation.
func CaptureRunDataObservation(o RunObservation) (RunDataBinding, error) {
	if !o.observed {
		return RunDataBinding{}, ErrRunDataBindingRequired
	}
	if !o.present {
		return RunDataBinding{}, fmt.Errorf("%w: an absent parent has no header to bind", ErrRunDataBindingRequired)
	}
	b, err := CaptureRunDataBinding(o.header)
	if err != nil {
		return RunDataBinding{}, err
	}
	b.tenant, b.tenantObserved = o.tenant, true
	return b, nil
}

// Header returns a value copy; mutating it cannot retarget the binding.
func (b RunDataBinding) Header() RunDataHeader { return b.header }

// TenantID returns the observed tenant and whether the binding carries one: a
// header-only binding answers ("", false), a run observed without a tenant
// ("", true).
func (b RunDataBinding) TenantID() (string, bool) { return b.tenant, b.tenantObserved }

// mergeRunDataBinding combines the binding ctx carries with an incoming one.
// The header is compared strictly. An observed tenant is kept whichever side
// carries it, so a header-only re-bind never drops it; two observed tenants
// that differ are another identity.
func mergeRunDataBinding(carried, incoming RunDataBinding) (RunDataBinding, error) {
	if carried.header != incoming.header || carried.observed != incoming.observed {
		return RunDataBinding{}, ErrRunDataIdentityChanged
	}
	if !incoming.tenantObserved {
		return carried, nil
	}
	if carried.tenantObserved && carried.tenant != incoming.tenant {
		return RunDataBinding{}, ErrRunDataIdentityChanged
	}
	return incoming, nil
}

// WithRunDataBinding retains a first observation or merges it with the one
// already carried (mergeRunDataBinding). It never adopts a successor or
// implicitly switches children. Child receipt rebinding belongs to the
// owned-birth preparation seam.
func WithRunDataBinding(ctx context.Context, b RunDataBinding) (context.Context, error) {
	if ctx == nil || !b.observed {
		return nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.header.validate(); err != nil {
		return nil, err
	}
	merged := b
	if old, ok := ctx.Value(runDataBindingKey{}).(RunDataBinding); ok {
		var err error
		if merged, err = mergeRunDataBinding(old, b); err != nil {
			return nil, err
		}
	}
	if format, ok := ctx.Value(runValueFormatKey{}).(RunValueFormat); ok {
		if format.RunID != b.header.RunID || format.Format != b.header.Format {
			return nil, ErrRunDataIdentityChanged
		}
	}
	return context.WithValue(ctx, runDataBindingKey{}, merged), nil
}

// WithoutRunDataBinding returns ctx without the run data binding and value
// format intent it carries, its identity and deadline kept: a read of
// whatever run holds the ID now (a log tailer labelling its buffer with the
// run it tails), never a read whose answer is served to the bound reader.
func WithoutRunDataBinding(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, runDataBindingKey{}, nil), runValueFormatKey{}, nil)
}

// RunDataBindingFrom exposes a value copy for propagation, never a mutable row.
func RunDataBindingFrom(ctx context.Context) (RunDataBinding, bool) {
	if ctx == nil {
		return RunDataBinding{}, false
	}
	b, ok := ctx.Value(runDataBindingKey{}).(RunDataBinding)
	return b, ok && b.observed
}

// CheckRunDataBinding compares a durable header under its owner's lock/CAS.
// Protocol one refuses unbound callers; observed protocol-zero absence remains
// distinct from an unbound caller. Store adapters must call this at publication
// and descriptor opening; this pure predicate alone does not protect payloads.
func CheckRunDataBinding(ctx context.Context, current RunDataHeader) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h, err := current.validate()
	if err != nil {
		return err
	}
	b, bound := RunDataBindingFrom(ctx)
	if !bound {
		if h.Protocol == IncarnationRunData {
			return ErrRunDataBindingRequired
		}
		return nil
	}
	if b.header != h {
		return ErrRunDataIdentityChanged
	}
	// A format-only rebind must not silently retain an incompatible data scope.
	if f, ok := ctx.Value(runValueFormatKey{}).(RunValueFormat); ok && (f.RunID != h.RunID || f.Format != h.Format) {
		return ErrRunDataIdentityChanged
	}
	return nil
}

// CheckRunObservedTenant compares the tenant ctx's binding observed with the
// canonical tenant a store parsed from the same record whose header it
// compared with CheckRunDataBinding. An unbound ctx and a header-only binding
// keep the legacy answer. "" observed is a value: a binding that observed ""
// refuses a record of "T", and the reverse.
func CheckRunObservedTenant(ctx context.Context, canonical string) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, bound := RunDataBindingFrom(ctx)
	if !bound {
		return nil
	}
	if tenant, observed := b.TenantID(); observed && tenant != canonical {
		return fmt.Errorf("%w: run %s records another tenant than the one its binding observed", ErrRunDataIdentityChanged, b.header.RunID)
	}
	return nil
}

// CheckMissingRunDataParent compares the requested orphan namespace with a
// retained observation. Historical unbound and same-ID absent-birth calls
// remain compatible; a foreign or retired birth never becomes an orphan.
func CheckMissingRunDataParent(ctx context.Context, runID string) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b, ok := RunDataBindingFrom(ctx); ok && b.header.Protocol == IncarnationRunData {
		return ErrRunNotFound
	}
	return CheckRunDataBinding(ctx, RunDataHeader{RunID: runID, Format: LegacyValueFormat})
}
