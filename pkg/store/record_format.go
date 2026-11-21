package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go.mongodb.org/mongo-driver/v2/bson"
	"io"
	"strings"
)

// RunValueFormatFromJSON reads only the format envelope, before payload decode
// or read-side healing. Duplicate/case-folded headers cannot select a second
// interpretation. Absence is the explicitly supported legacy format.
func RunValueFormatFromJSON(ctx context.Context, raw []byte) (ValueFormat, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if tok != json.Delim('{') {
		return 0, fmt.Errorf("%w: run object required", ErrInvalidValueFormat)
	}
	seen := false
	format := 0
	for dec.More() {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		tok, err = dec.Token()
		if err != nil {
			return 0, err
		}
		key, ok := tok.(string)
		if !ok {
			return 0, ErrInvalidValueFormat
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return 0, err
		}
		if strings.EqualFold(key, "format_version") {
			if seen || key != "format_version" {
				return 0, fmt.Errorf("%w: duplicate or noncanonical format header", ErrInvalidValueFormat)
			}
			seen = true
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return 0, fmt.Errorf("%w: null format header", ErrInvalidValueFormat)
			}
			if err = json.Unmarshal(value, &format); err != nil {
				return 0, fmt.Errorf("%w: %w", ErrInvalidValueFormat, err)
			}
		}
	}
	if _, err = dec.Token(); err != nil {
		return 0, err
	}
	if _, err = dec.Token(); err != io.EOF {
		if err == nil {
			err = ErrInvalidValueFormat
		}
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	return NormalizeValueFormat(format)
}

// RunValueFormatFromBSON never decodes payload values through the legacy
// registry before choosing a format. BSON int32/int64 headers are equivalent.
func RunValueFormatFromBSON(ctx context.Context, raw bson.Raw) (ValueFormat, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	elements, err := raw.Elements()
	if err != nil {
		return 0, err
	}
	seen := false
	format := int64(0)
	for _, el := range elements {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		if !strings.EqualFold(el.Key(), "format_version") && strings.ToLower(el.Key()) != "format_version" {
			continue
		}
		if el.Key() != "format_version" {
			return 0, fmt.Errorf("%w: noncanonical format header", ErrInvalidValueFormat)
		}
		if seen {
			return 0, fmt.Errorf("%w: duplicate format header", ErrInvalidValueFormat)
		}
		seen = true
		v := el.Value()
		switch v.Type {
		case bson.TypeInt32:
			format = int64(v.Int32())
		case bson.TypeInt64:
			format = v.Int64()
		default:
			return 0, fmt.Errorf("%w: noninteger format header", ErrInvalidValueFormat)
		}
	}
	switch format {
	case 0, 1:
		return LegacyValueFormat, nil
	case 2:
		return ExactValueFormat, nil
	default:
		return 0, fmt.Errorf("%w: %d", ErrUnsupportedValueFormat, format)
	}
}

// EncodeValueRecordJSON selects an explicit record plan. It never infers a
// mode from a value/frame. Legacy encoding keeps encoding/json's conventions.
func EncodeValueRecordJSON(ctx context.Context, f ValueFormat, kind ExactRecordKind, src any) ([]byte, error) {
	f, err := NormalizeValueFormat(int(f))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if f == ExactValueFormat {
		codec, err := ExactRecordCodec(kind)
		if err != nil {
			return nil, err
		}
		return codec.EncodeJSON(ctx, src)
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
func DecodeValueRecordJSON(ctx context.Context, f ValueFormat, kind ExactRecordKind, raw []byte, dst any) error {
	f, err := NormalizeValueFormat(int(f))
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if f == ExactValueFormat {
		codec, err := ExactRecordCodec(kind)
		if err != nil {
			return err
		}
		return codec.DecodeJSON(ctx, raw, dst)
	}
	if dest, ok := dst.(*Event); kind == ExactEvent && ok && dest != nil {
		var value Event
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		*dest = value
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// EncodeValueRecordBSON leaves legacy marshaling unchanged. Legacy decoding
// remains the Mongo caller's responsibility so its historical registry stays
// in use; exact decoding goes through ExactRecordCodec after header validation.
func EncodeValueRecordBSON(ctx context.Context, f ValueFormat, kind ExactRecordKind, src any) ([]byte, error) {
	f, err := NormalizeValueFormat(int(f))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if f == ExactValueFormat {
		codec, err := ExactRecordCodec(kind)
		if err != nil {
			return nil, err
		}
		return codec.EncodeBSON(ctx, src)
	}
	raw, err := bson.Marshal(src)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
