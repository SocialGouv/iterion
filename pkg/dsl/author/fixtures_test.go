package author

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The fixtures of the author schema (pkg/dsl/spec/testdata/author) are the
// converter's too: every document the schema accepts reads without an
// error, every document it refuses is refused here as well — by the
// converter or by the parser it hands the text to — and the lax ones, the
// documents the schema cannot see the defect of, are refused by the
// converter alone.

func fixtureDir(sub string) string {
	return filepath.Join("..", "spec", "testdata", "author", sub)
}

func fixtures(t *testing.T, sub string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(fixtureDir(sub), "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no %s fixtures: %v", sub, err)
	}
	sort.Strings(files)
	return files
}

func errorsOf(res *Result) []string {
	var out []string
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			out = append(out, d.Error())
		}
	}
	return out
}

func TestEveryValidFixtureReadsWithoutError(t *testing.T) {
	for _, path := range fixtures(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse(path, src)
			if errs := errorsOf(res); len(errs) > 0 {
				t.Fatalf("a valid document is refused:\n  %s\n--- spelled as:\n%s", strings.Join(errs, "\n  "), res.Text)
			}
			if len(res.File.Workflows) != 1 {
				t.Fatalf("the document has %d workflows, want 1", len(res.File.Workflows))
			}
		})
	}
}

func TestEveryInvalidFixtureIsRefused(t *testing.T) {
	for _, path := range fixtures(t, "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse(path, src)
			errs := errorsOf(res)
			if len(errs) == 0 {
				t.Fatalf("the document is accepted; its first line says why it is refused:\n%s\n--- spelled as:\n%s", firstLine(src), res.Text)
			}
			t.Logf("%s\n  refused: %s", firstLine(src), strings.Join(errs, "\n  "))
		})
	}
}

// laxCodeRe reads the code a lax fixture's first line names LAST: the
// converter's refusal, the one the fixture exists for (an earlier code on
// the line is the .bot's own, for the reader).
var laxCodeRe = regexp.MustCompile(`\((E0\d\d)\)`)

func TestEveryLaxFixtureIsRefusedByTheConverter(t *testing.T) {
	for _, path := range fixtures(t, "lax") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			codes := laxCodeRe.FindAllStringSubmatch(firstLine(src), -1)
			if len(codes) == 0 {
				t.Fatalf("the first line names no converter code (Exxx): %s", firstLine(src))
			}
			want := parser.DiagCode(codes[len(codes)-1][1])
			res := Parse(path, src)
			errs := errorsOf(res)
			if len(errs) == 0 {
				t.Fatalf("the schema cannot see this defect and the converter must:\n%s\n--- spelled as:\n%s", firstLine(src), res.Text)
			}
			// Refused BY THE GUARD the fixture names, not by a later reader
			// that happens to choke on the same construct: the guard is
			// the diagnostic the author gets, and the one a mutation of it
			// must redden.
			found := false
			for _, d := range res.Diagnostics {
				if d.Code == want && d.Severity == parser.SeverityError {
					found = true
				}
			}
			if !found {
				t.Fatalf("refused, but not with %s as the first line says:\n  %s", want, strings.Join(errs, "\n  "))
			}
			t.Logf("%s\n  refused: %s", firstLine(src), strings.Join(errs, "\n  "))
		})
	}
}

func firstLine(src []byte) string {
	s := string(src)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Every valid document round-trips: written back from its program, read
// again, it is the same program; and the writer's text is a fixpoint —
// written from the program it reads, it is written again identically.
func TestValidFixturesRoundTrip(t *testing.T) {
	for _, path := range fixtures(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse(path, src)
			if errs := errorsOf(res); len(errs) > 0 {
				t.Fatalf("refused: %s", strings.Join(errs, "; "))
			}
			out, err := Write(res.File)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			again := Parse(path+".yaml", out)
			if errs := errorsOf(again); len(errs) > 0 {
				t.Fatalf("the written document is refused:\n  %s\n--- written:\n%s\n--- spelled as:\n%s", strings.Join(errs, "\n  "), out, again.Text)
			}
			if why := sameProgramAST(res.File, again.File); why != "" {
				t.Fatalf("not the same program after the round trip: %s\n--- written:\n%s", why, out)
			}
			twice, err := Write(again.File)
			if err != nil {
				t.Fatalf("Write again: %v", err)
			}
			if string(twice) != string(out) {
				t.Fatalf("the writer's text is not a fixpoint:\n--- first:\n%s\n--- second:\n%s", out, twice)
			}
		})
	}
}

// sameProgramAST compares two files as programs: positions zeroed, since
// the two came from different texts. It returns "" when equal, else the
// first field that differs.
func sameProgramAST(a, b *ast.File) string {
	ca, cb := zeroPositions(a), zeroPositions(b)
	if reflect.DeepEqual(ca, cb) {
		return ""
	}
	return firstDifference(reflect.ValueOf(ca), reflect.ValueOf(cb), "File")
}

// zeroPositions returns a deep copy of f with every position zeroed and
// its profile made effective.
func zeroPositions(f *ast.File) *ast.File {
	cp := deepCopy(reflect.ValueOf(f)).Interface().(*ast.File)
	zero := &mapper{}
	zero.walkZero(reflect.ValueOf(cp))
	cp.Profile = f.EffectiveProfile()
	return cp
}

func (m *mapper) walkZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			m.walkZero(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == posType {
			if v.CanSet() {
				v.Set(reflect.Zero(posType))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				m.walkZero(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := 0; i < v.Len(); i++ {
			m.walkZero(v.Index(i))
		}
	}
}

func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		cp := reflect.New(v.Type().Elem())
		cp.Elem().Set(deepCopy(v.Elem()))
		return cp
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		cp := deepCopy(v.Elem())
		out := reflect.New(v.Type()).Elem()
		out.Set(cp)
		return out
	case reflect.Struct:
		cp := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				cp.Field(i).Set(deepCopy(v.Field(i)))
			}
		}
		return cp
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			cp.Index(i).Set(deepCopy(v.Index(i)))
		}
		return cp
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		cp := reflect.MakeMapWithSize(v.Type(), v.Len())
		for _, k := range v.MapKeys() {
			cp.SetMapIndex(k, deepCopy(v.MapIndex(k)))
		}
		return cp
	}
	return v
}

// firstDifference names the first path where two values differ.
func firstDifference(a, b reflect.Value, path string) string {
	if a.Kind() != b.Kind() {
		return path + ": kinds differ"
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() != b.IsNil() {
			return path + ": one side is nil"
		}
		if a.IsNil() {
			return ""
		}
		return firstDifference(a.Elem(), b.Elem(), path)
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			f := a.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if why := firstDifference(a.Field(i), b.Field(i), path+"."+f.Name); why != "" {
				return why
			}
		}
		return ""
	case reflect.Slice, reflect.Array:
		if a.Len() != b.Len() {
			return path + ": " + itoa(a.Len()) + " vs " + itoa(b.Len()) + " elements"
		}
		if a.Kind() == reflect.Slice && a.IsNil() != b.IsNil() {
			return path + ": nil vs empty"
		}
		for i := 0; i < a.Len(); i++ {
			if why := firstDifference(a.Index(i), b.Index(i), path+"["+itoa(i)+"]"); why != "" {
				return why
			}
		}
		return ""
	case reflect.Map:
		if !reflect.DeepEqual(a.Interface(), b.Interface()) {
			return path + ": maps differ"
		}
		return ""
	}
	if !reflect.DeepEqual(a.Interface(), b.Interface()) {
		return path + ": " + describeValue(a) + " vs " + describeValue(b)
	}
	return ""
}

func describeValue(v reflect.Value) string {
	if v.Kind() == reflect.String {
		return "\"" + v.String() + "\""
	}
	return reflect.ValueOf(v.Interface()).String()
}

func itoa(i int) string { return strconv.Itoa(i) }
