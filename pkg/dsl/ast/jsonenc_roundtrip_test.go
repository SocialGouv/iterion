package ast

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

// TestEveryASTFieldSurvivesTheJSONTransport is the converter-level half of
// TestEveryASTFieldHasAJSONCounterpart, which only proves a mirror FIELD
// exists: a field added to both structs and forgotten in toJSON or fromJSON
// passes that sweep, and the transport drops it in silence. The corpus test
// does not close the gap either — it can only see constructs some committed
// .bot happens to use, and most of the language is not in the corpus (an MCP
// `auth:` block, a `recovery:` block, an attachment's accept_mime: zero
// files each). Measured: dropping MCPAuthDecl.RevokeURL from the encoder
// passes the field sweep, the corpus transport test, the group/use/foreach
// fixture and the null test, and fails only here.
//
// So: build a File with EVERY field set to a distinct non-zero value, send
// it through the transport, and require it back unchanged.
//
// What it deliberately does not cover: a field of a NAMED string or integer
// type is left at its zero value, because those are the language's enums and
// an arbitrary value is not one of them (the decoder refuses it, as it
// should). Their conversion is covered by the corpus and by the per-construct
// fixtures.
func TestEveryASTFieldSurvivesTheJSONTransport(t *testing.T) {
	seed := 0
	sent := &File{}
	fillDistinct(reflect.ValueOf(sent).Elem(), &seed, 0)
	if seed < 200 {
		t.Fatalf("only %d fields were populated — the walk is broken", seed)
	}
	dropSpans(reflect.ValueOf(sent).Elem(), 0)

	raw, err := MarshalFile(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalFile(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dropSpans(reflect.ValueOf(back).Elem(), 0)

	if why := firstFieldDifference("document", reflect.ValueOf(sent), reflect.ValueOf(back)); why != "" {
		t.Errorf("the JSON transport did not carry the whole AST: %s", why)
	}
}

// maxFillDepth bounds the walk. The AST has no recursive type, so the bound
// is only a safety net; it must stay above the deepest nesting (File →
// workflow → edge → clause) or a pointer is left nil and the round-trip
// compares two zero values, proving nothing.
const maxFillDepth = 14

// fillDistinct sets every exported field reachable from v to a distinct
// non-zero value, so a converter that drops one shows up as a zero on the
// far side. seed counts the scalars written — the test asserts on it, since
// a walk that silently stopped early would pass vacuously.
func fillDistinct(v reflect.Value, seed *int, depth int) {
	if depth > maxFillDepth || !v.CanSet() {
		return
	}
	t := v.Type()
	switch v.Kind() {
	case reflect.Pointer:
		if isSpan(t.Elem()) {
			return
		}
		v.Set(reflect.New(t.Elem()))
		fillDistinct(v.Elem(), seed, depth+1)
	case reflect.Struct:
		if isSpan(t) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			switch {
			case !f.IsExported(), f.Name == "Span":
				continue
			case t.Name() == "WhenClause" && f.Name == "Expr":
				// The decoder refuses a when clause carrying both forms;
				// Condition is the one filled.
				continue
			}
			fillDistinct(v.Field(i), seed, depth+1)
		}
	case reflect.Slice:
		e := reflect.New(t.Elem()).Elem()
		fillDistinct(e, seed, depth+1)
		v.Set(reflect.Append(reflect.MakeSlice(t, 0, 1), e))
	case reflect.Map:
		k := reflect.New(t.Key()).Elem()
		fillDistinct(k, seed, depth+1)
		e := reflect.New(t.Elem()).Elem()
		fillDistinct(e, seed, depth+1)
		m := reflect.MakeMap(t)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		if t.PkgPath() != "" {
			return // a named string type is an enum; only its own values are legal
		}
		*seed++
		v.SetString("value-" + strconv.Itoa(*seed))
	case reflect.Int, reflect.Int64:
		if t.PkgPath() != "" {
			return // likewise for a named integer type
		}
		*seed++
		v.SetInt(int64(*seed))
	case reflect.Float64:
		*seed++
		v.SetFloat(float64(*seed) + 0.5)
	case reflect.Bool:
		*seed++
		v.SetBool(true)
	}
}

func isSpan(t reflect.Type) bool { return t.Name() == "Span" || t.Name() == "Pos" }

// dropSpans zeroes every source position, which never travels — so the
// comparison is about the program, not about where it was read from.
func dropSpans(v reflect.Value, depth int) {
	if depth > maxFillDepth+4 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			dropSpans(v.Elem(), depth+1)
		}
	case reflect.Struct:
		t := v.Type()
		if isSpan(t) {
			if v.CanSet() {
				v.Set(reflect.Zero(t))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).IsExported() {
				dropSpans(v.Field(i), depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			dropSpans(v.Index(i), depth+1)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			dropSpans(e, depth+1)
			v.SetMapIndex(k, e)
		}
	}
}

// firstFieldDifference names the field path at which two ASTs diverge, so a
// failure points at the converter to fix instead of at two pointers.
func firstFieldDifference(path string, a, b reflect.Value) string {
	if a.Kind() != b.Kind() {
		return path + ": kinds differ"
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		switch {
		case a.IsNil() && b.IsNil():
			return ""
		case a.IsNil() != b.IsNil():
			return fmt.Sprintf("%s: %v became %v", path, !a.IsNil(), !b.IsNil())
		}
		return firstFieldDifference(path, a.Elem(), b.Elem())
	case reflect.Struct:
		t := a.Type()
		for i := 0; i < t.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			if why := firstFieldDifference(path+"."+t.Field(i).Name, a.Field(i), b.Field(i)); why != "" {
				return why
			}
		}
		return ""
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return fmt.Sprintf("%s: %d entries became %d", path, a.Len(), b.Len())
		}
		for i := 0; i < a.Len(); i++ {
			if why := firstFieldDifference(fmt.Sprintf("%s[%d]", path, i), a.Index(i), b.Index(i)); why != "" {
				return why
			}
		}
		return ""
	case reflect.Map:
		if a.Len() != b.Len() {
			return fmt.Sprintf("%s: %d keys became %d", path, a.Len(), b.Len())
		}
		for _, k := range a.MapKeys() {
			bv := b.MapIndex(k)
			if !bv.IsValid() {
				return fmt.Sprintf("%s[%v]: the key was dropped", path, k)
			}
			if why := firstFieldDifference(fmt.Sprintf("%s[%v]", path, k), a.MapIndex(k), bv); why != "" {
				return why
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			return fmt.Sprintf("%s: %v became %v", path, a.Interface(), b.Interface())
		}
		return ""
	}
}
