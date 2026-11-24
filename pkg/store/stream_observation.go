package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// EventObservationSelection explicitly endorses the actual outer selection.
// The observation is immutable after acquisition and confers no authorization.
type EventObservationSelection interface {
	CheckEventObservationSelection(actual any) error
	EventStreamObservation() RunObservation
}

// ObservedFilesystemEventDecoder preserves the legacy decoder's contract while
// normalizing an accepted blank run ID only for observed stream delivery.
type ObservedFilesystemEventDecoder interface {
	DecodeObservedEvent(context.Context, []byte) (*Event, error)
}

// CaptureEventStreamObservation never unwraps or reloads a selection. Partial
// opt-in and accidental method promotion must not silently downgrade to legacy.
func CaptureEventStreamObservation(actual any) (RunObservation, bool, error) {
	if actual == nil {
		return RunObservation{}, false, ErrRunDataCapability
	}
	v := reflect.ValueOf(actual)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return RunObservation{}, false, ErrRunDataCapability
		}
	}
	c, ok := actual.(EventObservationSelection)
	if !ok {
		_, getter := actual.(interface{ EventStreamObservation() RunObservation })
		_, checker := actual.(interface{ CheckEventObservationSelection(any) error })
		_, decoder := actual.(ObservedFilesystemEventDecoder)
		if getter || checker || decoder {
			return RunObservation{}, false, ErrRunDataCapability
		}
		return RunObservation{}, false, nil
	}
	if err := c.CheckEventObservationSelection(actual); err != nil {
		return RunObservation{}, false, err
	}
	o := c.EventStreamObservation()
	if !o.Observed() {
		return RunObservation{}, false, ErrRunDataCapability
	}
	return o, true, nil
}

// streamCanonicalTenant reads top-level metadata only. In particular no business
// payload number is converted, and duplicate/case-alias evidence is preserved.
func streamCanonicalTenant(ctx context.Context, raw []byte) (string, error) {
	if ctx == nil {
		return "", ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return "", err
	}
	if token != json.Delim('{') {
		return "", ErrInvalidRunDataIdentity
	}
	tenant := ""
	seen := false
	for d.More() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err = d.Token()
		if err != nil {
			return "", err
		}
		key, ok := token.(string)
		if !ok {
			return "", ErrInvalidRunDataIdentity
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return "", err
		}
		if !strings.EqualFold(key, "tenant_id") && strings.ToLower(key) != "tenant_id" {
			continue
		}
		if seen || key != "tenant_id" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return "", ErrInvalidRunDataIdentity
		}
		seen = true
		if err = json.Unmarshal(value, &tenant); err != nil {
			return "", ErrInvalidRunDataIdentity
		}
	}
	if _, err = d.Token(); err != nil {
		return "", err
	}
	if _, err = d.Token(); err != io.EOF {
		return "", ErrInvalidRunDataIdentity
	}
	return tenant, ctx.Err()
}
