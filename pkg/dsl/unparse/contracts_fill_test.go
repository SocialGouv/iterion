package unparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every exported field of the contract's declarations is set to a distinct
// value the text can carry, the file is written and read back, and the
// document must be the same. The spelling sweep proves a field is READ by
// the writer; this proves the VALUE survives — a field dropped, or written
// under another meaning, fails here. The compiled program cannot be the
// oracle: it does not carry the contract.
func TestEveryContractFieldSurvivesTheWriter(t *testing.T) {
	f := &ast.File{Contracts: []*ast.ContractDecl{{}}}
	n := 0
	fillContract(t, reflect.ValueOf(f.Contracts[0]).Elem(), &n)
	if left := zeroField("ContractDecl", reflect.ValueOf(f.Contracts[0]).Elem()); left != "" {
		t.Fatalf("the filler left %s at its zero value: a drop of it would pass unseen", left)
	}
	out := Unparse(f)
	back := parser.Parse("x.bot", out)
	for _, d := range back.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("the written text does not parse: %s\n%s", d.Error(), out)
		}
	}
	want, err := ast.MarshalFile(&ast.File{Contracts: f.Contracts})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ast.MarshalFile(&ast.File{Contracts: back.File.Contracts})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("a field did not survive the writer: %s\n%s", FirstJSONDifference(want, got), out)
	}
	if err := Verify(f, out); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

var (
	rawJSONType = reflect.TypeOf(json.RawMessage{})
	spanType    = reflect.TypeOf(ast.Span{})
	commentType = reflect.TypeOf(ast.Comment{})
)

// fillContract sets every exported field to a distinct value the text can
// write: identifiers for strings (a name, a type, a kind, a producer are
// identifiers; a quoted value reads back as any text), a JSON object for a
// raw value, positive integers, true for booleans; one element per list.
func fillContract(t *testing.T, v reflect.Value, n *int) {
	t.Helper()
	switch {
	case v.Type() == spanType:
		return
	case v.Type() == commentType:
		// A comment's address is meaningful only against the document:
		// filled with a counter it would name a property the contract
		// does not have, and the writer would put it at the end of the
		// block instead — a real behaviour, held by its own test. Given a
		// real address here, the comparison proves the stronger thing:
		// the comment comes back exactly where it was written.
		*n++
		v.Set(reflect.ValueOf(ast.Comment{Text: fmt.Sprintf("v%d", *n), Anchor: "display_name", Place: ast.CommentTrailing}))
		return
	case v.Type() == rawJSONType:
		*n++
		v.SetBytes([]byte(fmt.Sprintf(`{"v":%d}`, *n)))
		return
	}
	switch v.Kind() {
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("v%d", *n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		*n++
		v.SetInt(int64(*n))
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillContract(t, p.Elem(), n)
		v.Set(p)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillContract(t, v.Field(i), n)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillContract(t, s.Index(0), n)
		v.Set(s)
	default:
		t.Fatalf("the filler does not know how to fill a %s (%s)", v.Kind(), v.Type())
	}
}

// zeroField names the first exported field the filler left at its zero
// value, "" when none.
func zeroField(path string, v reflect.Value) string {
	if v.Type() == spanType {
		return ""
	}
	if v.Type() == commentType {
		// The filler gives a comment a real address, not a counter; its
		// span never travels.
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return path
		}
		return zeroField(path, v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if d := zeroField(path+"."+f.Name, v.Field(i)); d != "" {
				return d
			}
		}
		return ""
	case reflect.Slice:
		if v.Len() == 0 {
			return path
		}
		for i := 0; i < v.Len(); i++ {
			if d := zeroField(fmt.Sprintf("%s[%d]", path, i), v.Index(i)); d != "" {
				return d
			}
		}
		return ""
	default:
		if v.IsZero() {
			return path
		}
		return ""
	}
}
