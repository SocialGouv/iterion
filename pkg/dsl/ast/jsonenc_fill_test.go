package ast

import (
	"fmt"
	"reflect"
	"testing"
)

// Every exported field of the AST is set to a distinct non-zero value, the
// file is marshalled and unmarshalled, and the result must equal the input.
// The field sweep proves a mirror FIELD exists; this proves the converters
// copy it — a field added to both structs and forgotten in toJSON/fromJSON
// passes the sweep and fails here. The corpus only covers what the shipped
// bots use; this covers every field whether a bot uses it or not.
func TestEveryFieldSurvivesTheTransport(t *testing.T) {
	f := &File{}
	n := 0
	fillValue(reflect.ValueOf(f).Elem(), &n, 0)
	legalise(f)
	data, err := MarshalFile(f)
	if err != nil {
		t.Fatal(err)
	}
	g, err := UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if d := firstDifference("File", reflect.ValueOf(f).Elem(), reflect.ValueOf(g).Elem()); d != "" {
		t.Fatalf("a field did not survive the transport: %s", d)
	}
}

// legalise clears the second half of each mutually exclusive pair the codec
// refuses when both are set: an edge's `when` is a condition OR an
// expression, a loop cap is a literal OR an expression.
func legalise(f *File) {
	var edges []*Edge
	for _, w := range f.Workflows {
		edges = append(edges, w.Edges...)
	}
	for _, g := range f.Groups {
		edges = append(edges, g.Edges...)
	}
	for _, e := range edges {
		if e.When != nil {
			e.When.Expr = ""
		}
		if e.Loop != nil {
			e.Loop.MaxIterationsExpr = ""
		}
	}
}

// transportlessTypes are position types: the JSON document is span-free by
// design (the studio canvas has no source positions to carry).
var transportlessTypes = map[reflect.Type]bool{
	reflect.TypeOf(Span{}): true,
	reflect.TypeOf(Pos{}):  true,
}

// fillDepth bounds the recursion: deep enough to allocate every pointer a
// group's nodes carry (File → Groups → GroupDecl → Computes → ComputeDecl →
// its blocks), shallow enough that a self-referential schema field ends.
const fillDepth = 9

// fillValue sets v to a distinct non-zero value, recursively. Slices and
// maps get one element; pointers are allocated; recursion is bounded so a
// self-referential type (a schema field's fields) terminates.
func fillValue(v reflect.Value, n *int, depth int) {
	if transportlessTypes[v.Type()] {
		return
	}
	switch v.Kind() {
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Type().PkgPath() != "" {
			// A named integer is an enum the codec writes by name: only a
			// value the tables know survives, and 1 is the first named one.
			v.SetInt(1)
			return
		}
		*n++
		v.SetInt(int64(*n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*n++
		v.SetUint(uint64(*n))
	case reflect.Float32, reflect.Float64:
		*n++
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Interface:
		*n++
		v.Set(reflect.ValueOf(fmt.Sprintf("i%d", *n)))
	case reflect.Pointer:
		if depth >= fillDepth {
			return
		}
		p := reflect.New(v.Type().Elem())
		fillValue(p.Elem(), n, depth+1)
		v.Set(p)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			fillValue(v.Field(i), n, depth)
		}
	case reflect.Slice:
		if depth >= fillDepth {
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillValue(s.Index(0), n, depth+1)
		v.Set(s)
	case reflect.Map:
		if depth >= fillDepth {
			return
		}
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillValue(k, n, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fillValue(e, n, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	}
}

// firstDifference names the first path at which want and got diverge.
func firstDifference(path string, want, got reflect.Value) string {
	if want.Type() != got.Type() {
		return path + ": type differs"
	}
	if transportlessTypes[want.Type()] {
		return ""
	}
	switch want.Kind() {
	case reflect.Pointer, reflect.Interface:
		if want.IsNil() != got.IsNil() {
			return fmt.Sprintf("%s: nil-ness differs (want nil=%v, got nil=%v)", path, want.IsNil(), got.IsNil())
		}
		if want.IsNil() {
			return ""
		}
		return firstDifference(path, want.Elem(), got.Elem())
	case reflect.Struct:
		for i := 0; i < want.NumField(); i++ {
			ft := want.Type().Field(i)
			if !ft.IsExported() {
				continue
			}
			if d := firstDifference(path+"."+ft.Name, want.Field(i), got.Field(i)); d != "" {
				return d
			}
		}
		return ""
	case reflect.Slice:
		if want.Len() != got.Len() {
			return fmt.Sprintf("%s: length %d became %d", path, want.Len(), got.Len())
		}
		for i := 0; i < want.Len(); i++ {
			if d := firstDifference(fmt.Sprintf("%s[%d]", path, i), want.Index(i), got.Index(i)); d != "" {
				return d
			}
		}
		return ""
	case reflect.Map:
		if want.Len() != got.Len() {
			return fmt.Sprintf("%s: %d entries became %d", path, want.Len(), got.Len())
		}
		iter := want.MapRange()
		for iter.Next() {
			g := got.MapIndex(iter.Key())
			if !g.IsValid() {
				return fmt.Sprintf("%s[%v]: missing after the round-trip", path, iter.Key())
			}
			if d := firstDifference(fmt.Sprintf("%s[%v]", path, iter.Key()), iter.Value(), g); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			return fmt.Sprintf("%s: %v became %v", path, want.Interface(), got.Interface())
		}
		return ""
	}
}
