package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrRunMetadataUndecodable marks run metadata the store read but cannot
// decode: corrupt or truncated JSON, a BSON document that does not decode, a
// schema version this binary does not know. Reading the same document again
// answers the same, so a caller treats it as final, never as a transient
// failure to retry.
var ErrRunMetadataUndecodable = errors.New("store: run metadata undecodable")

// RunMetadataUndecodable wraps err, a failure to decode the run metadata
// document the store already read, with ErrRunMetadataUndecodable. A context
// error is returned as it is: the read was cut short, not refused.
func RunMetadataUndecodable(id string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("store: run %s metadata: %w: %w", id, ErrRunMetadataUndecodable, err)
}

// runJSONUndecodable wraps a JSON decode failure of run.json that a re-read
// cannot change: a syntax or type error, or a non-empty document that ends
// early (io.EOF or io.ErrUnexpectedEOF from the decoder — run.json is
// replaced by rename, so a cut document is corruption, not a write in
// progress). An empty document keeps its bare io.EOF, and every other error
// is returned as it is.
func runJSONUndecodable(id string, data []byte, err error) error {
	if err == nil {
		return err
	}
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &typed), errors.Is(err, io.ErrUnexpectedEOF):
		return RunMetadataUndecodable(id, err)
	case errors.Is(err, io.EOF) && len(bytes.TrimSpace(data)) > 0:
		return RunMetadataUndecodable(id, err)
	}
	return err
}
