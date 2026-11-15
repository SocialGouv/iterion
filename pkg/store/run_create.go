package store

import (
	"context"
	"fmt"
)

// RunCreateSpec describes one atomic birth. Empty Status chooses the backend's
// historical initial status. No global switch changes legacy creation methods.
type RunCreateSpec struct {
	ID           string
	WorkflowName string
	ParentRunID  string
	FilePath     string
	BotID        string
	Inputs       map[string]any
	Status       RunStatus
	Format       ValueFormat
	// DataProtocol is independent of numeric format. Incarnation admission
	// stays closed until every payload backend and bridge supports it.
	DataProtocol RunDataProtocol
}

// Normalize validates explicit birth choices without changing caller data.
func (s RunCreateSpec) Normalize(ctx context.Context) (RunCreateSpec, error) {
	if ctx == nil {
		return s, fmt.Errorf("%w: nil context", ErrInvalidValueFormat)
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if err := RequireRunDataStorage(s.DataProtocol); err != nil {
		return s, err
	}
	b, err := (RunValueFormat{RunID: s.ID, Format: s.Format}).validate()
	if err != nil {
		return s, err
	}
	s.Format = b.Format
	if s.Status != "" && !s.Status.CanStartExecution() {
		return s, fmt.Errorf("%w: invalid birth status %q", ErrInvalidValueFormat, s.Status)
	}
	if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); bound {
		f, err := ValueFormatForWrite(ctx, s.ID)
		if err != nil {
			return s, err
		}
		if f != s.Format {
			return s, fmt.Errorf("%w: birth differs from binding", ErrValueFormatChanged)
		}
	}
	return s, nil
}

// RunValueFormatCreator persists format, parent and initial scheduling fields
// in the first exclusive write. Implementations must reject unsupported formats
// before effects; creating legacy then saving format two violates this contract.
type RunValueFormatCreator interface {
	CreateRunWithValueFormat(context.Context, RunCreateSpec) (*Run, error)
}

func AsRunValueFormatCreator(s RunStore) RunValueFormatCreator {
	return capability[RunValueFormatCreator](s)
}

// CreateRunWithValueFormat uses the common birth capability. Legacy simple
// callers can retain the old API; an exact request never degrades to CreateRun
// plus SaveRun, or silently drops its scheduling/parent fields.
func CreateRunWithValueFormat(ctx context.Context, s RunStore, spec RunCreateSpec) (*Run, error) {
	spec, err := spec.Normalize(ctx)
	if err != nil {
		return nil, err
	}
	if cap := AsRunValueFormatCreator(s); cap != nil {
		return cap.CreateRunWithValueFormat(ctx, spec)
	}
	if spec.Format != LegacyValueFormat || spec.Status != "" || spec.FilePath != "" || spec.BotID != "" || spec.ParentRunID != "" || s == nil {
		return nil, ErrValueFormatCapability
	}
	return s.CreateRun(ctx, spec.ID, spec.WorkflowName, spec.Inputs)
}
