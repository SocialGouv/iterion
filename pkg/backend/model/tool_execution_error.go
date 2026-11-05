package model

// ToolExecutionError keeps a direct tool failure distinct from a provider
// refusal even when stderr contains HTTP statuses or provider error names.
type ToolExecutionError struct{ Cause error }

func (e *ToolExecutionError) Error() string { return e.Cause.Error() }
func (e *ToolExecutionError) Unwrap() error { return e.Cause }
