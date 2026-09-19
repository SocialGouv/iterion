package operatormcp

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mapFixture writes a tiny module into the server's working directory so
// the graph tools have a tree to describe.
func mapFixture(t *testing.T, root string) {
	t.Helper()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/demo\n\ngo 1.26\n")
	write("pkg/seam/seam.go", "package seam\n\n// Store is the seam.\ntype Store interface{ Put(k string) error }\n")
	write("pkg/user/user.go", `package user

import "example.test/demo/pkg/seam"

// Adapter holds the seam.
type Adapter struct{ S seam.Store }
`)
}

func TestLocalMapFindReturnsIdsTheOtherToolsTake(t *testing.T) {
	s := newTestServer(t)
	mapFixture(t, s.WorkDir)

	out, isErr := call(t, s, "local_map_find", `{"query":"Store"}`)
	if isErr {
		t.Fatalf("local_map_find reported an error: %s", out)
	}
	var hits []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(hits) == 0 {
		t.Fatal("no hit for a symbol that exists")
	}
	if hits[0].ID != "sym:pkg/seam.Store" {
		t.Fatalf("first hit is %q, want the exact match sym:pkg/seam.Store", hits[0].ID)
	}
	if hits[0].Path == "" {
		t.Error("a hit carries no file to open next")
	}

	// The id must be the handle the other tools take — a find that
	// returned display strings would make the set unusable as a chain.
	nb, isErr := call(t, s, "local_map_neighbours", `{"id":"`+hits[0].ID+`"}`)
	if isErr {
		t.Fatalf("local_map_neighbours rejected the id local_map_find returned: %s", nb)
	}
}

// A seam is referenced, never called. This is the question the graph
// exists to answer, so it is the one the tool has to answer.
func TestLocalMapImpactFindsWhoHoldsASeam(t *testing.T) {
	s := newTestServer(t)
	mapFixture(t, s.WorkDir)

	out, isErr := call(t, s, "local_map_impact", `{"id":"sym:pkg/seam.Store","depth":2}`)
	if isErr {
		t.Fatalf("local_map_impact reported an error: %s", out)
	}
	if !strings.Contains(out, "pkg/user") {
		t.Fatalf("the package that holds the seam is missing from its impact:\n%s", out)
	}
}

// Direction is a fact, not a formality: a path that ran both ways would
// claim the seam depends on its user.
func TestLocalMapPathIsDirected(t *testing.T) {
	s := newTestServer(t)
	mapFixture(t, s.WorkDir)

	forward, isErr := call(t, s, "local_map_path", `{"from":"pkg:pkg/user","to":"pkg:pkg/seam"}`)
	if isErr {
		t.Fatalf("local_map_path reported an error: %s", forward)
	}
	if !strings.Contains(forward, "pkg:pkg/seam") {
		t.Fatalf("no path from the importer to the imported:\n%s", forward)
	}
	backward, _ := call(t, s, "local_map_path", `{"from":"pkg:pkg/seam","to":"pkg:pkg/user"}`)
	if !strings.Contains(backward, "no directed path") {
		t.Fatalf("a path was found against the direction of the import:\n%s", backward)
	}
}

// A tool annotated ReadOnly must create NOTHING on disk — not in the
// store, not in the working directory. The two tests that guard this in
// operatormcp_test.go each cover a slice of it: one stats only StoreDir
// and names two tools, the other returns at `remote_api`. So a tool that
// wrote 5.9 MB of graph cache into WorkDir passed both.
//
// This walks the WHOLE tool set and the WHOLE working directory, so the
// next ReadOnly tool that writes is caught by the class rather than by
// whoever happens to review it.
func TestNoReadOnlyToolWritesAnything(t *testing.T) {
	dir := t.TempDir()
	mapFixture(t, dir)
	s := &Server{StoreDir: filepath.Join(dir, "absent-store"), WorkDir: dir, ReadOnly: true, Only: FamilyLocal}

	// One plausible payload per argument shape; a tool that refuses the
	// arguments still must not have written anything by then.
	payloads := []string{
		`{}`,
		`{"query":"Store"}`,
		`{"id":"pkg:pkg/seam"}`,
		`{"from":"pkg:pkg/user","to":"pkg:pkg/seam"}`,
	}
	before := snapshot(t, dir)
	for _, tool := range s.Tools() {
		if !tool.ReadOnly {
			continue
		}
		for _, args := range payloads {
			if _, err := s.Call(context.Background(), tool.Name, json.RawMessage(args)); err != nil {
				t.Fatalf("Call(%s, %s): %v", tool.Name, args, err)
			}
		}
	}
	for path := range snapshot(t, dir) {
		if !before[path] {
			t.Errorf("a ReadOnly tool created %s in read-only mode", path)
		}
	}
}

// snapshot lists every path under root, so a test can assert that a set
// of calls added none.
func snapshot(t *testing.T, root string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seen
}

// An argument nobody declared must be refused rather than ignored: the
// shared decoder is moving to DisallowUnknownFields (#1335), and a tool
// whose schema omits a property it accepts would start failing then.
func TestLocalMapSchemasDeclareEveryArgumentTheyRead(t *testing.T) {
	for _, tool := range localMapTools() {
		var schema struct {
			Properties           map[string]any `json:"properties"`
			AdditionalProperties *bool          `json:"additionalProperties"`
			Required             []string       `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", tool.Name, err)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: schema does not close additionalProperties", tool.Name)
		}
		if len(schema.Properties) == 0 {
			t.Errorf("%s: schema declares no property", tool.Name)
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				t.Errorf("%s: %q is required but not declared", tool.Name, req)
			}
		}
		if !tool.ReadOnly {
			t.Errorf("%s: a graph query is read-only", tool.Name)
		}
	}
}
