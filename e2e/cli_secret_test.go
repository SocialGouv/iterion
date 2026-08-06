package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion secret set|list|rm` is the local (desktop/CLI) credential
// lifecycle: values are AES-GCM sealed before they touch disk, never
// echoed, and resolved into a run by the same path the executor uses.
// The stores and the sealer are unit-covered; the LIFECYCLE — write with
// the CLI, read back through the run path, never leak the plaintext —
// was not.
//
// Mutation check: stop sealing and the "sealed on disk" assertion fails;
// stop resolving and the run-path read comes back empty; leak the value
// into `list` and the no-plaintext assertions fail; break the
// project-over-global layering and the override assertion fails.

const (
	secretPlaintext = "iterion-e2e-plaintext-1234"
	secretRotated   = "iterion-e2e-rotated-5678"
)

// isolateSecretStore points the global iterion data dir at a temp dir and
// pins the master key via ITERION_SECRETS_KEY, so the test never reads or
// writes the operator's real store — and never touches the OS keychain.
// Returns the project store dir to pass as --store-dir.
func isolateSecretStore(t *testing.T) string {
	t.Helper()
	t.Setenv("ITERION_HOME", t.TempDir())
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(key))
	return t.TempDir()
}

func humanPrinter(buf *bytes.Buffer) *cli.Printer {
	return &cli.Printer{W: buf, Format: cli.OutputHuman}
}

// resolveThroughRunPath reads the named secrets exactly the way a run
// does (secrets.ResolveLocalCredentials over the layered local store),
// so the assertion is about what a bot would actually receive.
func resolveThroughRunPath(t *testing.T, projectDir string, names ...string) map[string]string {
	t.Helper()
	st, err := cli.LocalSecretStores(projectDir)
	if err != nil {
		t.Fatalf("open local secret stores: %v", err)
	}
	sealer, err := secrets.NewLocalSealer(store.GlobalIterionDataDir(), nil)
	if err != nil {
		t.Fatalf("build sealer: %v", err)
	}
	creds, err := secrets.ResolveLocalCredentials(context.Background(), st, sealer, names, iterlog.Nop())
	if err != nil {
		t.Fatalf("resolve local credentials: %v", err)
	}
	return creds.Generic
}

// grepTree returns the files under root whose bytes contain needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // an unreadable entry cannot hold the plaintext
		}
		data, rerr := os.ReadFile(path)
		if rerr == nil && bytes.Contains(data, []byte(needle)) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return hits
}

// TestSecretSetListRemoveRoundTrip drives the full lifecycle and asserts
// the observable contract at every step.
func TestSecretSetListRemoveRoundTrip(t *testing.T) {
	projectDir := isolateSecretStore(t)
	t.Setenv("E2E_SECRET_SOURCE", secretPlaintext)

	// --- set -----------------------------------------------------------
	var setOut bytes.Buffer
	if err := cli.RunSecretSet(humanPrinter(&setOut), cli.SecretOptions{
		Name:     "E2E_TOKEN",
		FromEnv:  "E2E_SECRET_SOURCE",
		StoreDir: projectDir,
	}); err != nil {
		t.Fatalf("RunSecretSet: %v", err)
	}
	if !strings.Contains(setOut.String(), "Stored") {
		t.Errorf("set output = %q, want it to report the secret was stored", setOut.String())
	}
	if !strings.Contains(setOut.String(), "1234") {
		t.Errorf("set output = %q, want the last4 of the value", setOut.String())
	}
	if strings.Contains(setOut.String(), secretPlaintext) {
		t.Fatal("set echoed the plaintext value back to the operator")
	}

	// --- sealed on disk --------------------------------------------------
	// The whole point of the local store: the value must not be readable
	// from the filesystem.
	if hits := grepTree(t, store.GlobalIterionDataDir(), secretPlaintext); len(hits) > 0 {
		t.Fatalf("plaintext found unsealed in the global store: %v", hits)
	}
	if hits := grepTree(t, projectDir, secretPlaintext); len(hits) > 0 {
		t.Fatalf("plaintext found unsealed in the project store: %v", hits)
	}

	// --- list ------------------------------------------------------------
	var listOut bytes.Buffer
	if err := cli.RunSecretList(jsonPrinter(&listOut), cli.SecretOptions{StoreDir: projectDir}); err != nil {
		t.Fatalf("RunSecretList: %v", err)
	}
	if !strings.Contains(listOut.String(), "E2E_TOKEN") {
		t.Fatalf("list output does not name the stored secret: %s", listOut.String())
	}
	if !strings.Contains(listOut.String(), `"scope": "global"`) {
		t.Errorf("list output does not report the global scope: %s", listOut.String())
	}
	if strings.Contains(listOut.String(), secretPlaintext) {
		t.Fatal("list leaked the plaintext value")
	}

	// --- the run path reads it back --------------------------------------
	if got := resolveThroughRunPath(t, projectDir, "E2E_TOKEN")["E2E_TOKEN"]; got != secretPlaintext {
		t.Fatalf("run-path resolution = %q, want the value the CLI stored", got)
	}

	// --- rotate (upsert by name) -----------------------------------------
	t.Setenv("E2E_SECRET_SOURCE", secretRotated)
	var rotateOut bytes.Buffer
	if err := cli.RunSecretSet(humanPrinter(&rotateOut), cli.SecretOptions{
		Name:     "E2E_TOKEN",
		FromEnv:  "E2E_SECRET_SOURCE",
		StoreDir: projectDir,
	}); err != nil {
		t.Fatalf("RunSecretSet (rotate): %v", err)
	}
	if !strings.Contains(rotateOut.String(), "Rotated") {
		t.Errorf("rotate output = %q, want it to report a rotation rather than a second record", rotateOut.String())
	}
	if got := resolveThroughRunPath(t, projectDir, "E2E_TOKEN")["E2E_TOKEN"]; got != secretRotated {
		t.Fatalf("after rotate the run path still resolves %q, want the new value", got)
	}

	// --- remove -----------------------------------------------------------
	var rmOut bytes.Buffer
	if err := cli.RunSecretRemove(humanPrinter(&rmOut), cli.SecretOptions{Name: "E2E_TOKEN", StoreDir: projectDir}); err != nil {
		t.Fatalf("RunSecretRemove: %v", err)
	}
	if got, ok := resolveThroughRunPath(t, projectDir, "E2E_TOKEN")["E2E_TOKEN"]; ok {
		t.Fatalf("removed secret still resolves as %q", got)
	}
	if err := cli.RunSecretRemove(humanPrinter(&bytes.Buffer{}), cli.SecretOptions{Name: "E2E_TOKEN", StoreDir: projectDir}); err == nil {
		t.Error("removing an absent secret succeeded, want a clear error")
	}
}

// TestSecretProjectScopeOverridesGlobal: the per-project layer is what
// lets one repo pin a different credential under the same name. If the
// layering broke, a run in that project would silently get the machine
// value — the failure mode this asserts against.
func TestSecretProjectScopeOverridesGlobal(t *testing.T) {
	projectDir := isolateSecretStore(t)

	t.Setenv("E2E_SECRET_SOURCE", "global-value-0001")
	if err := cli.RunSecretSet(humanPrinter(&bytes.Buffer{}), cli.SecretOptions{
		Name: "E2E_SHARED", FromEnv: "E2E_SECRET_SOURCE", StoreDir: projectDir,
	}); err != nil {
		t.Fatalf("set global: %v", err)
	}

	t.Setenv("E2E_SECRET_SOURCE", "project-value-0002")
	var projOut bytes.Buffer
	if err := cli.RunSecretSet(humanPrinter(&projOut), cli.SecretOptions{
		Name: "E2E_SHARED", FromEnv: "E2E_SECRET_SOURCE", Project: true, StoreDir: projectDir,
	}); err != nil {
		t.Fatalf("set project: %v", err)
	}
	if !strings.Contains(projOut.String(), "project scope") {
		t.Errorf("set --project output = %q, want it to report the project scope", projOut.String())
	}

	if got := resolveThroughRunPath(t, projectDir, "E2E_SHARED")["E2E_SHARED"]; got != "project-value-0002" {
		t.Fatalf("run-path resolution = %q, want the project layer to win over the global one", got)
	}

	// Removing the project record must uncover the global one again,
	// not delete both.
	if err := cli.RunSecretRemove(humanPrinter(&bytes.Buffer{}), cli.SecretOptions{Name: "E2E_SHARED", StoreDir: projectDir}); err != nil {
		t.Fatalf("remove project record: %v", err)
	}
	if got := resolveThroughRunPath(t, projectDir, "E2E_SHARED")["E2E_SHARED"]; got != "global-value-0001" {
		t.Fatalf("after removing the project record the run path resolves %q, want the global value", got)
	}
}

// TestSecretSetRejectsBadInput: the guards an operator hits on a typo.
func TestSecretSetRejectsBadInput(t *testing.T) {
	projectDir := isolateSecretStore(t)

	if err := cli.RunSecretSet(&cli.Printer{W: io.Discard}, cli.SecretOptions{
		Name: "bad name!", FromEnv: "E2E_SECRET_SOURCE", StoreDir: projectDir,
	}); err == nil {
		t.Error("an invalid secret name was accepted")
	}

	if err := cli.RunSecretSet(&cli.Printer{W: io.Discard}, cli.SecretOptions{
		Name: "E2E_MISSING", FromEnv: "E2E_DEFINITELY_UNSET_VAR", StoreDir: projectDir,
	}); err == nil {
		t.Error("--from-env on an unset variable was accepted, want an explicit error rather than an empty secret")
	}

	// Nothing may have landed in the store.
	var listOut bytes.Buffer
	if err := cli.RunSecretList(jsonPrinter(&listOut), cli.SecretOptions{StoreDir: projectDir}); err != nil {
		t.Fatalf("RunSecretList: %v", err)
	}
	if strings.Contains(listOut.String(), "E2E_MISSING") {
		t.Errorf("a rejected `secret set` still created a record: %s", listOut.String())
	}
}
