package author

import (
	"os"
	"path/filepath"
	"testing"
)

// The written form of the schema's every-kind fixture is the writer's
// canonical text: every key in the registry's order, nodes in the
// document's order, YAML's native values. The golden under testdata/ is
// what a `fmt --to yaml` of that program writes; `UPDATE_GOLDEN=1 go test`
// rewrites it after a deliberate change of the writer.
func TestWrittenEveryKindIsTheGolden(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "every-kind-v2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("every-kind-v2.yaml", src)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("refused: %v", errs)
	}
	out, err := Write(res.File)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "every-kind-v2.written.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read the golden (UPDATE_GOLDEN=1 writes it): %v", err)
	}
	if string(want) != string(out) {
		t.Fatalf("the written document differs from the golden %s (UPDATE_GOLDEN=1 rewrites it after a deliberate change):\n--- written:\n%s", golden, out)
	}
}
