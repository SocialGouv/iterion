package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A native action (board.*, ADR-151) is executed by the transport the
// executor's build registers — no catalog, no connection, no sealer — so a
// CLI run whose only actions are native must not open the local sealer: on a
// host whose iterion home holds a sealed store and no reachable key that
// aborts the run, and on a fresh host it mints key material (a keyfile, or a
// keychain entry and its unlock prompt) for a catalog it never consults.
//
// The probe is a keyfile that does not decode: opening the sealer reads it
// FIRST (before any keychain call) and fails on it, so "the sealer was
// opened" is observable as that error and nothing touches the OS keychain
// or writes a byte. The HTTP row is the falsifier: the same home makes the
// gate's other side fail, so a silent pass on the native row is the gate.
func TestANativeOnlyRunOpensNoSealer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv("ITERION_SECRETS_KEY", "")
	keyfile := filepath.Join(home, secrets.MasterKeyFileName)
	if err := os.WriteFile(keyfile, []byte("not-a-32-byte-key"), 0o600); err != nil {
		t.Fatalf("write probe keyfile: %v", err)
	}

	nativeOnly := &ir.Workflow{Nodes: map[string]ir.Node{
		"plan": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "plan"}, Action: "board.issue.list", Capabilities: []string{"board.read"}},
		"file": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "file"}, Action: "board.issue.create", Capabilities: []string{"board.create"}},
	}}
	resolver, client, err := localConnectorsForRun(nativeOnly, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("a native-only workflow opened the local sealer: %v", err)
	}
	if resolver != nil || client != nil {
		t.Fatalf("a native-only workflow built a catalog resolver (%T) it has no use for", resolver)
	}

	withHTTP := &ir.Workflow{Nodes: map[string]ir.Node{
		"plan":    nativeOnly.Nodes["plan"],
		"comment": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "comment"}, Action: "forgejo.issue.comment", Connection: "main"},
	}}
	if _, _, err := localConnectorsForRun(withHTTP, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), keyfile) {
		t.Fatalf("an HTTP action must open the sealer (and meet the probe keyfile): err = %v", err)
	}
}
