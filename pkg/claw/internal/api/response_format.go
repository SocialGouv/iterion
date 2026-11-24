package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Validate checks the native format envelope, not the model's JSON Schema
// subset or generated output. A nil receiver leaves formatting at its default.
func (f *ResponseFormat) Validate() error {
	if f == nil {
		return nil
	}
	invalid := func(field, reason string) error {
		return &RequestValidationError{Field: "ResponseFormat" + field, Reason: reason}
	}
	switch f.Type {
	case "text", "json_object":
		if f.JSONSchema != nil {
			return invalid(".JSONSchema", "must be absent for "+f.Type)
		}
	case "json_schema":
		if f.JSONSchema == nil {
			return invalid(".JSONSchema", "descriptor is required for json_schema")
		}
		if strings.TrimSpace(f.JSONSchema.Name) == "" {
			return invalid(".JSONSchema.Name", "must not be empty")
		}
		raw := bytes.TrimSpace(f.JSONSchema.Schema)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return invalid(".JSONSchema.Schema", "must contain a non-null JSON schema")
		}
		if !utf8.Valid(raw) {
			return invalid(".JSONSchema.Schema", "must be valid UTF-8")
		}
		if !json.Valid(raw) {
			return invalid(".JSONSchema.Schema", "must be valid JSON")
		}
	default:
		return invalid(".Type", "must be text, json_schema or json_object")
	}
	return nil
}

// Normalized validates and copies the descriptor, applying the shared schema
// policy without mutating the caller (requests may be reused concurrently).
func (f *ResponseFormat) Normalized() (*ResponseFormat, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, nil
	}
	out := *f
	if f.JSONSchema != nil {
		descriptor := *f.JSONSchema
		raw, err := cleanSchemaJSON(descriptor.Schema)
		if err != nil {
			return nil, &RequestValidationError{Field: "ResponseFormat.JSONSchema.Schema", Reason: err.Error()}
		}
		descriptor.Schema = raw
		out.JSONSchema = &descriptor
	}
	return &out, nil
}
