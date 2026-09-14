package ast

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
)

// MarshalFileWithProvenance is MarshalFile with each declaration's file of
// origin on it — "file", a slash path from root (the file's own name when
// it does not lie under root) — on every declaration, on the keyed blocks
// and their entries, on the comments: what a save by provenance needs to
// write each declaration back where it came from, when the document is a
// unit merged from several files. MarshalFile itself, the transport,
// carries no such key and is byte-identical to what it always was.
func MarshalFileWithProvenance(f *File, root string) ([]byte, error) {
	jf := toJSON(f)
	stampProvenance(reflect.ValueOf(f), reflect.ValueOf(jf), func(name string) string {
		return provenanceRel(root, name)
	})
	return json.MarshalIndent(jf, "", "  ")
}

// provenanceRel names a file by its slash path from root, or by its own
// name (slashed) when there is no root or the file lies outside it.
func provenanceRel(root, name string) string {
	if root == "" {
		return filepath.ToSlash(name)
	}
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(name)
	}
	return filepath.ToSlash(rel)
}

var spanType = reflect.TypeOf(Span{})

// stampProvenance walks the AST and its JSON mirror side by side — the
// mirror is built element for element, in the AST's order, under the
// AST's field names — and writes on every mirror that has a File field
// the file its AST carrier's span starts in. Done by reflection, so a
// declaration kind added to the AST later carries its provenance without
// anyone remembering to list it: its mirror needs the File field, nothing
// else.
func stampProvenance(av, jv reflect.Value, rel func(string) string) {
	av, jv = derefValue(av), derefValue(jv)
	if !av.IsValid() || !jv.IsValid() {
		return
	}
	switch av.Kind() {
	case reflect.Struct:
		if jv.Kind() != reflect.Struct {
			return
		}
		if span := av.FieldByName("Span"); span.IsValid() && span.Type() == spanType {
			if ff := jv.FieldByName("File"); ff.IsValid() && ff.Kind() == reflect.String && ff.CanSet() {
				if name := span.Interface().(Span).Start.File; name != "" {
					ff.SetString(rel(name))
				}
			}
		}
		at := av.Type()
		for i := 0; i < av.NumField(); i++ {
			f := at.Field(i)
			if !f.IsExported() || f.Name == "Span" {
				continue
			}
			if jf := jv.FieldByName(f.Name); jf.IsValid() {
				stampProvenance(av.Field(i), jf, rel)
			}
		}
	case reflect.Slice:
		if jv.Kind() != reflect.Slice || av.Len() != jv.Len() {
			return
		}
		for i := 0; i < av.Len(); i++ {
			stampProvenance(av.Index(i), jv.Index(i), rel)
		}
	}
}

// readProvenance is stampProvenance's inverse: a mirror's File, when set,
// becomes the file of its AST carrier's span — start and end, the whole
// span lies in one file — so a document that came with provenance parses
// back into an AST a save can route file by file.
func readProvenance(jv, av reflect.Value) {
	jv, av = derefValue(jv), derefValue(av)
	if !jv.IsValid() || !av.IsValid() {
		return
	}
	switch av.Kind() {
	case reflect.Struct:
		if jv.Kind() != reflect.Struct {
			return
		}
		if ff := jv.FieldByName("File"); ff.IsValid() && ff.Kind() == reflect.String && ff.String() != "" {
			if span := av.FieldByName("Span"); span.IsValid() && span.Type() == spanType && span.CanSet() {
				s := span.Interface().(Span)
				s.Start.File, s.End.File = ff.String(), ff.String()
				span.Set(reflect.ValueOf(s))
			}
		}
		at := av.Type()
		for i := 0; i < av.NumField(); i++ {
			f := at.Field(i)
			if !f.IsExported() || f.Name == "Span" {
				continue
			}
			if jf := jv.FieldByName(f.Name); jf.IsValid() {
				readProvenance(jf, av.Field(i))
			}
		}
	case reflect.Slice:
		if jv.Kind() != reflect.Slice || av.Len() != jv.Len() {
			return
		}
		for i := 0; i < av.Len(); i++ {
			readProvenance(jv.Index(i), av.Index(i))
		}
	}
}

// derefValue follows pointers and interfaces; a nil yields an invalid value.
func derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}
