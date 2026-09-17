package secrets

import (
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// TestOAuthRecordUpsertCanClearEveryFieldItCanSet guards the whole struct
// against one bug, paid twice already.
//
// Upsert is a FULL replace: the connect/paste path builds the record from the
// blob it was just handed and stores the whole thing. But MongoOAuthStore
// commits it through `$set`, where an omitted key leaves the OLD value in
// place — so a bson `omitempty` makes that field one-way: writable when
// non-zero, never clearable. The memory twin replaces the map entry outright
// and so clears correctly, which is exactly why no in-memory test can see it.
//
// It cost an AccountLabel that reported cleared and kept the stale name, then
// a NotRefreshable that could be set but never unset — self-sustaining, since
// the only writer able to clear it is a successful refresh, which the flag
// itself makes the worker skip. A platform credential was left with no way to
// be marked refreshable again and expired with nothing able to renew it.
//
// This asserts the PROPERTY rather than a list of field names: every key the
// record writes when populated must still be written when that field is at its
// zero value. A field added later with `omitempty` fails here on its own.
func TestOAuthRecordUpsertCanClearEveryFieldItCanSet(t *testing.T) {
	var full OAuthRecord
	populated := populateEveryField(t, reflect.ValueOf(&full).Elem())

	fullBody, err := mongoutil.SetBodyWithoutID(full, "secrets: oauth")
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	zeroBody, err := mongoutil.SetBodyWithoutID(OAuthRecord{}, "secrets: oauth")
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}

	// Guard the guard: if the walk silently skipped a field, the populated
	// body would be missing its key and the comparison below would pass by
	// covering less, not more. _id is the one key SetBodyWithoutID drops.
	if want := populated - 1; len(fullBody) != want {
		t.Fatalf("populated body has %d keys, want %d — the walk missed a field, so this test covers less than it claims", len(fullBody), want)
	}

	for key := range fullBody {
		if _, ok := zeroBody[key]; !ok {
			t.Errorf("%q is written when set but OMITTED when zero: an Upsert can never clear it, and Mongo keeps the previous value. Drop `omitempty` from its bson tag.", key)
		}
	}
}

// populateEveryField sets every field of the struct to a non-zero value and
// returns how many it set, so the caller can prove the walk was exhaustive.
func populateEveryField(t *testing.T, v reflect.Value) int {
	t.Helper()
	when := time.Date(2026, 9, 14, 21, 27, 4, 0, time.UTC)
	n := 0
	for i := range v.NumField() {
		f := v.Field(i)
		if !f.CanSet() {
			// Outside the property's domain, not a hole in the walk: bson
			// never marshals an unexported field, so it has no key for $set
			// to carry or drop. An EXPORTED field the walk cannot set would
			// be a hole, and still stops it.
			if v.Type().Field(i).IsExported() {
				t.Fatalf("exported field %q could not be set: this walk cannot cover it", v.Type().Field(i).Name)
			}
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
			elem := f.Index(0)
			if elem.Kind() == reflect.String {
				elem.SetString("x")
			} else {
				elem.SetUint(1)
			}
		case reflect.Pointer:
			p := reflect.New(f.Type().Elem())
			if _, ok := p.Interface().(*time.Time); ok {
				p.Elem().Set(reflect.ValueOf(when))
			}
			f.Set(p)
		case reflect.Struct:
			if _, ok := f.Interface().(time.Time); ok {
				f.Set(reflect.ValueOf(when))
				break
			}
			t.Fatalf("field %q is an unhandled struct type %s", v.Type().Field(i).Name, f.Type())
		default:
			t.Fatalf("field %q has unhandled kind %s — extend this walk", v.Type().Field(i).Name, f.Kind())
		}
		n++
	}
	return n
}
