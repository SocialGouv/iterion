package cli_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The connection commands had no test at all, which is how `add` came to
// report a grant that differed from the record it had just written.

// connectionsWorkspace puts a real connector package where `localCatalog`
// looks, and redirects the sealer's key and home so the test never touches the
// operator's keychain or store.
func connectionsWorkspace(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "connectors", "forgejo"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("the shipped Forgejo package is not present: %v", err)
	}
	ws := t.TempDir()
	// Symlinked rather than copied: the package is most of a megabyte of
	// generated YAML and the catalog only ever reads it.
	if err := os.MkdirAll(filepath.Join(ws, "connectors"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(src, filepath.Join(ws, "connectors", "forgejo")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("ITERION_HOME", filepath.Join(ws, "home"))
	t.Setenv("FORGE_TEST_TOKEN", "probe-token")
	t.Chdir(ws)
	return ws
}

// TestConnectionsAddReportsTheGrantItStored.
//
// With no --capability the default is `action`, and `add` echoed the empty
// FLAG instead of the resolved grant — so it printed "capabilities: " while
// `connections list` printed "action" for the very same record. An operator
// reading the first would conclude the connection grants nothing.
func TestConnectionsAddReportsTheGrantItStored(t *testing.T) {
	ws := connectionsWorkspace(t)
	storeDir := filepath.Join(ws, ".iterion")

	var add bytes.Buffer
	err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
		Connector: "forgejo",
		Scheme:    "token",
		BaseURL:   "https://codeberg.org",
		TokenEnv:  "FORGE_TEST_TOKEN",
		StoreDir:  storeDir,
	}, &add)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(add.String(), "capabilities: action") {
		t.Errorf("add must report the grant it stored, got:\n%s", add.String())
	}

	var list bytes.Buffer
	if err := cli.ConnectionsList(storeDir, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list.String(), "action") {
		t.Errorf("list must show the same grant, got:\n%s", list.String())
	}
}

// An explicit grant is reported as given — the case that worked by accident,
// pinned so the shared rendering cannot regress it while fixing the default.
func TestConnectionsAddReportsAnExplicitGrant(t *testing.T) {
	ws := connectionsWorkspace(t)
	var add bytes.Buffer
	err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
		Connector:    "forgejo",
		Scheme:       "token",
		Alias:        "both",
		TokenEnv:     "FORGE_TEST_TOKEN",
		Capabilities: []string{"action", "agent"},
		StoreDir:     filepath.Join(ws, ".iterion"),
	}, &add)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(add.String(), "action+agent") {
		t.Errorf("add must report both grants, got:\n%s", add.String())
	}
}

// A base URL this process will refuse must be said at the moment it is
// ADDED. The refusal otherwise arrives a layer away, in a run, from a workflow
// that names an operation rather than a URL.
func TestConnectionsAddWarnsOnAHostTheGuardRefuses(t *testing.T) {
	ws := connectionsWorkspace(t)
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "")

	var add bytes.Buffer
	err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
		Connector: "forgejo",
		Scheme:    "token",
		BaseURL:   "http://127.0.0.1:3000",
		TokenEnv:  "FORGE_TEST_TOKEN",
		StoreDir:  filepath.Join(ws, ".iterion"),
	}, &add)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	out := add.String()
	if !strings.Contains(out, "ITERION_CONNECTOR_ALLOW_PRIVATE") {
		t.Errorf("a loopback base URL must be flagged with its remedy, got:\n%s", out)
	}
	// The record is still created: the environment can change, and this
	// command is not the place to decide a deployment's network policy.
	var list bytes.Buffer
	if err := cli.ConnectionsList(filepath.Join(ws, ".iterion"), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list.String(), "127.0.0.1:3000") {
		t.Errorf("the connection must still be stored, got:\n%s", list.String())
	}
}

// With the hatch open there is nothing to warn about.
func TestConnectionsAddIsSilentWhenThePrivateHatchIsOpen(t *testing.T) {
	ws := connectionsWorkspace(t)
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "1")

	var add bytes.Buffer
	err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
		Connector: "forgejo",
		Scheme:    "token",
		BaseURL:   "http://127.0.0.1:3000",
		TokenEnv:  "FORGE_TEST_TOKEN",
		StoreDir:  filepath.Join(ws, ".iterion"),
	}, &add)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if strings.Contains(add.String(), "warning") {
		t.Errorf("the hatch is open, so there is nothing to warn about, got:\n%s", add.String())
	}
}

// A token passed as a flag would land in the shell history and in `ps`, so
// there is no --token and the env var is required.
func TestConnectionsAddRequiresATokenEnv(t *testing.T) {
	ws := connectionsWorkspace(t)
	var out bytes.Buffer
	err := cli.ConnectionsAdd(cli.ConnectionAddOptions{
		Connector: "forgejo",
		Scheme:    "token",
		StoreDir:  filepath.Join(ws, ".iterion"),
	}, &out)
	if err == nil {
		t.Fatal("a connection with no credential must be refused")
	}
	if !strings.Contains(err.Error(), "--token-env") {
		t.Errorf("the refusal must name the flag, got: %v", err)
	}
}
