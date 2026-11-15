package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RunDataHeaderFromJSON reads only canonical admission metadata, preserving
// duplicate-key evidence and never decoding Inputs or other execution payloads.
// It is a parser, not an observation capability or an activation switch.
func RunDataHeaderFromJSON(ctx context.Context, raw []byte) (RunDataHeader, error) {
	var h RunDataHeader
	if ctx == nil {
		return h, ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return h, err
	}
	if token != json.Delim('{') {
		return h, fmt.Errorf("%w: run object required", ErrInvalidRunDataIdentity)
	}
	seen := map[string]bool{}
	for d.More() {
		if err := ctx.Err(); err != nil {
			return h, err
		}
		token, err = d.Token()
		if err != nil {
			return h, err
		}
		key, ok := token.(string)
		if !ok {
			return h, ErrInvalidRunDataIdentity
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return h, err
		}
		canonical := canonicalRunDataHeaderKey(key, "id")
		switch canonical {
		case "id", "format_version", "data_protocol", "birth_id":
			if key != canonical || seen[canonical] {
				return h, fmt.Errorf("%w: duplicate or noncanonical %s", ErrInvalidRunDataIdentity, canonical)
			}
			seen[canonical] = true
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return h, fmt.Errorf("%w: null %s", ErrInvalidRunDataIdentity, key)
			}
		default:
			continue
		}
		switch key {
		case "id":
			err = json.Unmarshal(value, &h.RunID)
		case "birth_id":
			err = json.Unmarshal(value, &h.BirthID)
		case "format_version":
			err = json.Unmarshal(value, &h.Format)
		case "data_protocol":
			err = json.Unmarshal(value, &h.Protocol)
		}
		if err != nil {
			return h, fmt.Errorf("%w: %s: %w", ErrInvalidRunDataIdentity, key, err)
		}
	}
	if _, err = d.Token(); err != nil {
		return h, err
	}
	if _, err = d.Token(); err != io.EOF {
		if err == nil {
			err = ErrInvalidRunDataIdentity
		}
		return h, err
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	return h.validate()
}

// RunDataHeaderFromBSON uses raw elements so duplicate headers and integer
// types remain visible. Payload bytes never pass through a BSON registry.
func RunDataHeaderFromBSON(ctx context.Context, raw bson.Raw) (RunDataHeader, error) {
	var h RunDataHeader
	if ctx == nil {
		return h, ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	elements, err := raw.Elements()
	if err != nil {
		return h, err
	}
	seen := map[string]bool{}
	for _, el := range elements {
		if err := ctx.Err(); err != nil {
			return h, err
		}
		key := el.Key()
		canonical := canonicalRunDataHeaderKey(key, "_id")
		switch canonical {
		case "_id", "format_version", "data_protocol", "birth_id":
			if key != canonical || seen[canonical] {
				return h, fmt.Errorf("%w: duplicate or noncanonical %s", ErrInvalidRunDataIdentity, canonical)
			}
			seen[canonical] = true
		default:
			continue
		}
		value := el.Value()
		switch key {
		case "_id", "birth_id":
			if value.Type != bson.TypeString {
				return h, fmt.Errorf("%w: nonstring %s", ErrInvalidRunDataIdentity, key)
			}
			if key == "_id" {
				h.RunID = value.StringValue()
			} else {
				h.BirthID = value.StringValue()
			}
		case "format_version", "data_protocol":
			var number int64
			switch value.Type {
			case bson.TypeInt32:
				number = int64(value.Int32())
			case bson.TypeInt64:
				number = value.Int64()
			default:
				return h, fmt.Errorf("%w: noninteger %s", ErrInvalidRunDataIdentity, key)
			}
			// Reject unknown values before converting to machine-sized integers.
			if key == "data_protocol" {
				if number != 0 && number != 1 {
					return h, fmt.Errorf("%w: %d", ErrUnsupportedRunDataProtocol, number)
				}
				h.Protocol = RunDataProtocol(number)
			} else {
				if number < 0 || number > 2 {
					return h, fmt.Errorf("%w: %d", ErrUnsupportedValueFormat, number)
				}
				h.Format = ValueFormat(number)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	return h.validate()
}

// encoding/json uses Unicode simple folding, not strings.ToLower. In
// particular long-s must not select a different format in the payload decoder.
// BSON also accepts ToLower aliases such as dotted-I, so retain the union.
func canonicalRunDataHeaderKey(key, idKey string) string {
	lower := strings.ToLower(key)
	for _, canonical := range []string{idKey, "format_version", "data_protocol", "birth_id"} {
		if strings.EqualFold(key, canonical) || lower == canonical {
			return canonical
		}
	}
	return ""
}
