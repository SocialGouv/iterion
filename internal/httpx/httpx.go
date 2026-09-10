// Package httpx provides the shared JSON request/response helpers used by
// iterion's HTTP handlers.
package httpx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

// maxBody bounds request bodies read by DecodeJSON (10 MB).
const maxBody = 10 << 20

// WriteJSON writes v as an application/json response with the given status
// code. Content-Type is set before WriteHeader; encoding uses json.NewEncoder
// defaults (HTML escaping on, trailing newline), and encode errors are
// discarded — the status line is already on the wire.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// EncodeJSON writes v as an application/json body WITHOUT touching the status
// code: callers either rely on the implicit 200 from the first body write or
// have already called WriteHeader themselves (in which case the Content-Type
// set here is a no-op, matching the historical helper's behavior).
func EncodeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON reads at most 10 MB of the request body and unmarshals it into
// dst.
func DecodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// DecodeJSONStrict is DecodeJSON with unknown fields REFUSED instead of
// dropped, and the accepted names put in the error.
//
// A silently ignored field is indistinguishable, from the client, from one
// that was honoured: the request is accepted, the parameter does nothing,
// and the caller learns it from the behaviour of whatever it started rather
// than from the answer it got. Measured 2026-09-08: a launch sent its
// parameter under a name the request struct does not declare, the field was
// dropped, the run took the workflow's own default for it — the opposite of
// what was asked — and ten hours were spent before anyone read the value
// back out of the run instead of out of the payload.
//
// The names are listed because a suggestion by string distance would not
// have helped that case: the two names were semantically related and
// textually unrelated. The list is.
func DecodeJSONStrict(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if names := acceptedFields(dst); strings.Contains(err.Error(), "unknown field") && names != "" {
			return fmt.Errorf("%w; accepted fields: %s", err, names)
		}
		return err
	}
	return nil
}

// acceptedFields lists the JSON names dst declares, comma-separated and
// sorted. Embedded structs are walked so an inherited field is offered like
// any other; a field tagged "-" is not a name a caller may send.
func acceptedFields(dst any) string {
	t := reflect.TypeOf(dst)
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice) {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return ""
	}
	var names []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if f.Anonymous && tag == "" && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if tag == "" {
				tag = f.Name
			}
			if f.IsExported() {
				names = append(names, tag)
			}
		}
	}
	walk(t)
	sort.Strings(names)
	return strings.Join(names, ", ")
}
