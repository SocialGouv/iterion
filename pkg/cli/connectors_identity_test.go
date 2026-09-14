package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestConnectorRegenerationPreservesExistingIdentities(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "spec.json"), filepath.Join(root, "probe")
	options := cli.ConnectorsGenOptions{Spec: source, Out: dest, ID: "probe", KeepOverlay: true}
	var doc map[string]any
	if err := json.Unmarshal([]byte(genFixture), &doc); err != nil {
		t.Fatal(err)
	}
	generate := func() *spec.Package {
		t.Helper()
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cli.ConnectorsGen(options, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		pkg, err := spec.LoadGenerated(dest)
		if err != nil {
			t.Fatal(err)
		}
		return pkg
	}
	before := generate()
	paths := doc["paths"].(map[string]any)
	paths["/aaa"] = map[string]any{"get": map[string]any{
		"tags": []string{"issue"}, "operationId": "issueListIssues",
		"responses": map[string]any{"200": map[string]any{"description": "ok"}},
	}}
	doc["securityDefinitions"].(map[string]any)["AAAFirstToken"] = map[string]any{"type": "apiKey", "in": "header", "name": "X-New-Token"}
	after := generate()
	for _, old := range before.Operations() {
		got, ok := after.Operation(old.ID)
		if !ok || got.HTTP.Method != old.HTTP.Method || got.HTTP.Path != old.HTTP.Path {
			t.Errorf("public identity %s moved from %s %s to %s %s", old.ID, old.HTTP.Method, old.HTTP.Path, got.HTTP.Method, got.HTTP.Path)
		}
	}
	old := before.Connector.Auth[0]
	got, ok := after.Connector.AuthScheme(old.ID)
	if !ok || got.Name != old.Name {
		t.Errorf("auth identity %s moved from %s to %s", old.ID, old.Name, got.Name)
	}
}

func connectorIdentityFixture(t *testing.T) cli.ConnectorsGenOptions {
	t.Helper()
	root := t.TempDir()
	source, dest := filepath.Join(root, "spec.json"), filepath.Join(root, "probe")
	if err := os.WriteFile(source, []byte(genFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := cli.ConnectorsGenOptions{Spec: source, Out: dest, ID: "probe", KeepOverlay: true}
	if err := cli.ConnectorsGen(opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestConnectorIdentityRefusalsPreservePackage(t *testing.T) {
	for _, reason := range []string{"malformed lock", "unknown lock field", "overlay mismatch", "public id transfer", "auth placement transfer", "unchecked overlay", "interrupted replacement", "concurrent writer", "unsupported authored file"} {
		t.Run(reason, func(t *testing.T) {
			opts := connectorIdentityFixture(t)
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(opts.Out, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch reason {
			case "malformed lock":
				write("identity.lock.yaml", "schema_version: [\n")
			case "unknown lock field":
				body, err := os.ReadFile(filepath.Join(opts.Out, "identity.lock.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				write("identity.lock.yaml", string(body)+"unknown: true\n")
			case "overlay mismatch":
				write("overlay.yaml", "schema_version: 1\nconnector: probe\noperations:\n  probe.absent.op:\n    mcp: true\n")
			case "public id transfer":
				pkg, err := spec.LoadGenerated(opts.Out)
				if err != nil {
					t.Fatal(err)
				}
				ids := map[string]string{}
				for _, op := range pkg.Operations() {
					ids[op.HTTP.Method] = op.ID
				}
				write("overlay.yaml", fmt.Sprintf("schema_version: 1\nconnector: probe\ndrop: [%s]\noperations:\n  %s:\n    id: %s\n", ids["GET"], ids["POST"], ids["GET"]))
			case "auth placement transfer":
				write("overlay.yaml", "schema_version: 1\nconnector: probe\nauth:\n- id: token\n  kind: api_key\n  in: header\n  name: X-New-Auth\n")
			case "unchecked overlay":
				write("overlay.yaml", genOverlay)
				opts.KeepOverlay = false
			case "interrupted replacement":
				if err := os.Mkdir(filepath.Join(filepath.Dir(opts.Out), ".probe.connector-backup-crash"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "concurrent writer":
				lock, err := store.AcquireFileLock(filepath.Join(filepath.Dir(opts.Out), ".probe.connector-generation.lock"), "test")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Unlock()
			case "unsupported authored file":
				if runtime.GOOS == "windows" {
					t.Skip("symlink fixture requires Unix")
				}
				if err := os.Symlink(opts.Spec, filepath.Join(opts.Out, "authored.link")); err != nil {
					t.Fatal(err)
				}
			}
			before := connectorFiles(t, opts.Out)
			if err := cli.ConnectorsGen(opts, &bytes.Buffer{}); err == nil {
				t.Fatal("unsafe regeneration accepted")
			}
			if !reflect.DeepEqual(before, connectorFiles(t, opts.Out)) {
				t.Fatal("failed regeneration changed existing bytes")
			}
		})
	}
}

func TestConnectorRegenerationPreservesAuthoredFilesAndModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	opts := connectorIdentityFixture(t)
	path := filepath.Join(opts.Out, "operator-notes.md")
	if err := os.WriteFile(path, []byte("authored contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cli.ConnectorsGen(opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "authored contents\n" || info.Mode().Perm() != 0o600 {
		t.Fatalf("authored file changed: %q %o", body, info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(opts.Out))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "connector-stage-") || strings.Contains(entry.Name(), "connector-backup-") {
			t.Errorf("temporary replacement left behind: %s", entry.Name())
		}
	}
}

func TestConnectorFutureIdentityLockRefusesWithoutWriting(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "spec.json"), filepath.Join(root, "probe")
	if err := os.WriteFile(source, []byte(genFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := cli.ConnectorsGenOptions{Spec: source, Out: dest, ID: "probe", KeepOverlay: true}
	if err := cli.ConnectorsGen(opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "identity.lock.yaml"), []byte("schema_version: 99\nconnector: probe\nfuture_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := connectorFiles(t, dest)
	if err := cli.ConnectorsGen(opts, &bytes.Buffer{}); err == nil {
		t.Error("future identity lock was ignored")
	}
	if !reflect.DeepEqual(before, connectorFiles(t, dest)) {
		t.Error("refused regeneration changed the existing package")
	}
}

func connectorFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		files[path] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
