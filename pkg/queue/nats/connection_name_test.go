package nats

import (
	"os"
	"strings"
	"testing"
)

func TestNATSConnectionNamesDistinguishProcessRoles(t *testing.T) {
	if got := applyDefaults(Config{}).ConnectionName; got != "iterion" {
		t.Fatalf("existing caller lost its default diagnostic name: %q", got)
	}
	for _, invalid := range []string{"line\nbreak", "name.with.dot", strings.Repeat("x", 65)} {
		if conn, err := Connect(t.Context(), Config{URL: "nats://127.0.0.1:1", ConnectionName: invalid}); err == nil {
			conn.Close()
			t.Fatalf("unsafe diagnostic name %q was accepted", invalid)
		}
	}
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("connection-name integration requires ITERION_TEST_NATS_URI")
		}
		t.Skip("ITERION_TEST_NATS_URI not set")
	}
	for _, name := range []string{"iterion-server", "iterion-runner"} {
		conn, err := Connect(t.Context(), Config{URL: uri, ConnectionName: name})
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.NATS().Opts.Name; got != name {
			conn.Close()
			t.Fatalf("NATS CONNECT lost role name: want %q, got %q", name, got)
		}
		conn.Close()
	}
}
