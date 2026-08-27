package botsource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Materialize is ALL-OR-NOTHING: a single unsafe path aborts and removes
// everything already written, so no caller can ever operate on a silently
// partial bundle tree.
func TestMaterialize_AllOrNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	err := Materialize(dir, map[string]string{
		"main.bot":        "workflow main:\n",
		"skills/probe.md": "# probe",
		"../escape.txt":   "outside",
	})
	if err == nil {
		t.Fatal("traversal key must error")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Fatalf("partial tree must be removed on error, stat = %v", serr)
	}
}

func TestMaterialize_WritesTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	files := map[string]string{
		"main.bot":        "workflow main:\n",
		"manifest.yaml":   "name: x\n",
		"skills/probe.md": "# probe",
		"devbox.json":     "{}",
	}
	if err := Materialize(dir, files); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", rel, got, err)
		}
	}
}

// Digest is order-independent over the map and collision-safe across
// (path, content) boundaries.
func TestDigest(t *testing.T) {
	a := map[string]string{"a": "1", "b": "2"}
	b := map[string]string{"b": "2", "a": "1"}
	if Digest(a) != Digest(b) {
		t.Error("digest must be map-order independent")
	}
	// "ab"+"c" vs "a"+"bc" must not collide (length-prefixing).
	if Digest(map[string]string{"ab": "c"}) == Digest(map[string]string{"a": "bc"}) {
		t.Error("digest must not collide across path/content boundaries")
	}
	if Digest(a) == Digest(map[string]string{"a": "1"}) {
		t.Error("digest must reflect every entry")
	}
}

// The bundle limits reject at Validate time, before any store write.
func TestValidate_Limits(t *testing.T) {
	base := BotSource{TenantID: "t1", Slug: "big", Files: map[string]string{
		MainBotFile:   "workflow main:\n",
		"skills/x.md": strings.Repeat("x", MaxBundleBytes),
	}}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversize bundle must fail naming the byte limit, got %v", err)
	}
	many := BotSource{TenantID: "t1", Slug: "many", Files: map[string]string{MainBotFile: "workflow main:\n"}}
	for i := 0; i <= MaxBundleFiles; i++ {
		many.Files[fmt.Sprintf("skills/s%04d.md", i)] = "s"
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("too many files must fail naming the file limit, got %v", err)
	}
}

func TestIsPlatform(t *testing.T) {
	if !IsPlatform(PlatformTenantID) || IsPlatform("team-1") || IsPlatform("") {
		t.Fatal("IsPlatform must match exactly the sentinel")
	}
}
