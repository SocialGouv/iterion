package store

import (
	"context"
	"fmt"
)

// RunObservation is a copied data-origin observation within its owner's store.
// It is routing provenance, not authorization or execution ownership. The zero
// value is unknown; an observed missing parent is a separate value.
type RunObservation struct {
	header   RunDataHeader
	tenant   string
	present  bool
	observed bool
}

// CaptureRunObservation copies a caller-supplied observation. It does not read
// storage or establish that a backend supports this represented state.
func CaptureRunObservation(header RunDataHeader, tenant string, present bool) (RunObservation, error) {
	h, err := header.validate()
	if err != nil {
		return RunObservation{}, err
	}
	if !present && (h.Protocol != HistoricalRunData || h.BirthID != "") {
		return RunObservation{}, fmt.Errorf("%w: absent parent has a birth", ErrInvalidRunDataIdentity)
	}
	return RunObservation{header: h, tenant: tenant, present: present, observed: true}, nil
}

func (o RunObservation) Header() RunDataHeader { return o.header }
func (o RunObservation) TenantID() string      { return o.tenant }
func (o RunObservation) ParentPresent() bool   { return o.present }
func (o RunObservation) Observed() bool        { return o.observed }

// BindRunObservation carries a present observation's header and format as the
// original read intent of ctx. A binding or format ctx already carries is
// compared, never replaced: a successor is refused, not adopted. Only header
// and format travel — tenant and presence are not compared by this binding,
// and the caller's transport identity is left unchanged.
func BindRunObservation(ctx context.Context, o RunObservation) (context.Context, error) {
	if ctx == nil || !o.observed {
		return nil, ErrRunDataBindingRequired
	}
	if !o.present {
		return nil, fmt.Errorf("%w: an absent parent has no header to bind", ErrRunDataCapability)
	}
	binding, err := CaptureRunDataBinding(o.header)
	if err != nil {
		return nil, err
	}
	// WithRunDataBinding compares an existing binding AND an explicit format
	// before this adds the missing format intent.
	bound, err := WithRunDataBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return WithRunValueFormat(bound, RunValueFormat{RunID: o.header.RunID, Format: o.header.Format})
}

// EventObservation returns a value copy. Owned decoders clear provenance;
// retained readers attach their own observation after decoding and validation.
func (e Event) EventObservation() (RunObservation, bool) {
	return e.observation, e.observation.observed
}

// WithEventObservation copies the event and replaces any previous observation.
// Callers supply the retained or committed original, never a later metadata read.
// It does not resolve metadata, confer access or clone mutable Data.
func WithEventObservation(e Event, o RunObservation) (Event, error) {
	if !o.observed {
		return Event{}, ErrRunDataBindingRequired
	}
	if e.RunID != o.header.RunID {
		return Event{}, ErrRunDataIdentityChanged
	}
	e.observation = o
	return e, nil
}
