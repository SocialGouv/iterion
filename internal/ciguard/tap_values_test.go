package ciguard

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guarantee scripts/ci/tap_values_only.py carries, checked as a property
// over the repository's real formula and cask rather than one scenario per
// rule: every change that update-brew-tap.sh would not write is refused —
// a byte added, removed or moved on any line, a line duplicated or dropped,
// a key swapped, a non-canonical value, a line ending rewritten — and what
// update-brew-tap.sh does write is accepted.

const tapValuesOnly = "../../scripts/ci/tap_values_only.py"

func valuesOnly(t *testing.T, before, after []byte) bool {
	t.Helper()
	dir := t.TempDir()
	b, a := filepath.Join(dir, "before"), filepath.Join(dir, "after")
	if err := os.WriteFile(b, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, after, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(tapValuesOnly)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", script, b, a)
	cmd.Env = scriptEnv(t)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
		return false
	}
	t.Fatalf("tap_values_only.py: %v\n%s", err, out)
	return false
}

func TestTapValuesOnlyAcceptsWhatUpdateBrewTapWrites(t *testing.T) {
	root, artefacts := tapTree(t, "1.0.0", "1.0.0")
	before := map[string][]byte{}
	for _, rel := range []string{"Formula/iterion.rb", "Cask/iterion-desktop.rb"} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = b
	}
	updateTap(t, root, artefacts, "1.1.0")
	for rel, b := range before {
		a, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(a, b) {
			t.Fatalf("update-brew-tap.sh left %s unchanged — the fixture proves nothing", rel)
		}
		if !valuesOnly(t, b, a) {
			t.Errorf("update-brew-tap.sh's own rewrite of %s is refused", rel)
		}
	}
}

func TestTapValuesOnlyRefusesEveryOtherChange(t *testing.T) {
	for _, rel := range []string{"Formula/iterion.rb", "Cask/iterion-desktop.rb"} {
		base, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(base, []byte("\n"))
		join := func(ls [][]byte) []byte { return bytes.Join(ls, []byte("\n")) }
		with := func(i int, line []byte) []byte {
			ls := append([][]byte{}, lines...)
			ls[i] = line
			return join(ls)
		}
		variants := map[string][]byte{
			"the whole file in CRLF":    bytes.ReplaceAll(base, []byte("\n"), []byte("\r\n")),
			"the whole file in lone CR": bytes.ReplaceAll(base, []byte("\n"), []byte("\r")),
			"the final newline dropped": bytes.TrimSuffix(base, []byte("\n")),
		}
		for i, line := range lines {
			variants[fmt.Sprintf("line %d: a CR appended", i+1)] = with(i, append(append([]byte{}, line...), '\r'))
			variants[fmt.Sprintf("line %d: a space appended", i+1)] = with(i, append(append([]byte{}, line...), ' '))
			variants[fmt.Sprintf("line %d: a tab prepended", i+1)] = with(i, append([]byte{'\t'}, line...))
			dup := append(append([][]byte{}, lines[:i+1]...), lines[i:]...)
			variants[fmt.Sprintf("line %d: duplicated", i+1)] = join(dup)
			drop := append(append([][]byte{}, lines[:i]...), lines[i+1:]...)
			variants[fmt.Sprintf("line %d: dropped", i+1)] = join(drop)
			m := regexpTapValueLine.FindSubmatch(line)
			if m == nil {
				continue
			}
			key, value := string(m[2]), string(m[4])
			other := map[string]string{"version": "sha256", "sha256": "version"}[key]
			variants[fmt.Sprintf("line %d: the key swapped", i+1)] = with(i, []byte(string(m[1])+other+string(m[3])+value))
			variants[fmt.Sprintf("line %d: the key and value spaced apart", i+1)] = with(i, []byte(string(m[1])+key+"  "+value))
			for name, bad := range map[string]string{
				"an interpolation":             `"#{%x(id)}1.2.3"`,
				"an empty value":               `""`,
				"an uppercase hex":             `"` + strings.Repeat("A", 64) + `"`,
				"a 63-hex digest":              `"` + strings.Repeat("a", 63) + `"`,
				"a 64-character non-hex":       `"` + strings.Repeat("g", 64) + `"`,
				"a short hex":                  `"abc"`,
				"a pre-release":                `"1.2.3-rc1"`,
				"a path":                       `"1.2.3/../../evil"`,
				"a slash version":              `"1/2"`,
				"an empty version component":   `"1..2"`,
				"a hex version":                `"a.b"`,
				"a trailing remark":            value + ` # by hand`,
				"code glued to the value":      value + `.tap { system("id") }`,
				"a comment glued to the value": value + `#x`,
			} {
				variants[fmt.Sprintf("line %d: %s", i+1, name)] = with(i, []byte(string(m[1])+key+string(m[3])+bad))
			}
		}
		// The control rides the same batch: a harness that refused everything
		// would make every variant above look refused.
		const control = "control: the version moved to another canonical value"
		variants[control] = regexpVersionValue.ReplaceAll(base, []byte(`${1}"9.9.9"`))
		names := make([]string, 0, len(variants))
		for name, after := range variants {
			if bytes.Equal(after, base) {
				t.Fatalf("%s: variant %q equals the original — it proves nothing", rel, name)
			}
			names = append(names, name)
		}
		accepted := valuesOnlyBatch(t, base, names, variants)
		sawControl := false
		for _, name := range accepted {
			if name == control {
				sawControl = true
				continue
			}
			t.Errorf("%s: %s is accepted as a tap update", rel, name)
		}
		if !sawControl {
			t.Errorf("%s: the control — a canonical version change — was refused: the batch proves nothing", rel)
		}
	}
}

// valuesOnlyBatch judges every variant against base in one python process,
// calling the real program's values_only on the files it would read, and
// returns the names of the variants it accepted.
func valuesOnlyBatch(t *testing.T, base []byte, names []string, variants map[string][]byte) []string {
	t.Helper()
	dir := t.TempDir()
	before := filepath.Join(dir, "before")
	if err := os.WriteFile(before, base, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-I", "-B", "-c", valuesOnlyHarness}
	script, err := filepath.Abs(tapValuesOnly)
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, script, before)
	for i, name := range names {
		after := filepath.Join(dir, fmt.Sprintf("after-%d", i))
		if err := os.WriteFile(after, variants[name], 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, after)
	}
	cmd := exec.Command("python3", args...)
	cmd.Env = scriptEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("batch harness: %v\n%s", err, out)
	}
	// -B above keeps the import from writing scripts/ci/__pycache__.
	verdicts := strings.Fields(string(out))
	if len(verdicts) != len(names) {
		t.Fatalf("batch harness answered %d verdicts for %d variants:\n%s", len(verdicts), len(names), out)
	}
	var accepted []string
	for i, v := range verdicts {
		if v == "accept" {
			accepted = append(accepted, names[i])
		}
	}
	return accepted
}

// valuesOnlyHarness loads tap_values_only.py from its path and prints one
// verdict per AFTER file, in order.
const valuesOnlyHarness = `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("tap_values_only", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
before = m.lines(sys.argv[2])
for after in sys.argv[3:]:
    print("accept" if m.values_only(before, m.lines(after)) else "refuse")
`
