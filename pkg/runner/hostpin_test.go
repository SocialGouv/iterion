package runner

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinHostInHostsFile_AddsAndRestores(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	const orig = "127.0.0.1 localhost\n::1 localhost\n"
	if err := os.WriteFile(hosts, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	restore, err := pinHostInHostsFile(hosts, "github.com", net.ParseIP("140.82.112.3"))
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	got, _ := os.ReadFile(hosts)
	// Original content preserved + the pin line appended with its marker.
	if !strings.HasPrefix(string(got), orig) {
		t.Errorf("original hosts content not preserved:\n%s", got)
	}
	want := "140.82.112.3 github.com  " + ssrfPinMarker
	if !strings.Contains(string(got), want) {
		t.Errorf("pin line missing; got:\n%s", got)
	}

	// Restore removes ONLY the pin line, leaving the original intact.
	restore()
	after, _ := os.ReadFile(hosts)
	if strings.Contains(string(after), ssrfPinMarker) {
		t.Errorf("pin line survived restore:\n%s", after)
	}
	if strings.TrimRight(string(after), "\n") != strings.TrimRight(orig, "\n") {
		t.Errorf("restore did not return to original:\n%q\nwant:\n%q", after, orig)
	}
}

func TestPinHostInHostsFile_Guards(t *testing.T) {
	if _, err := pinHostInHostsFile("", "h", net.ParseIP("1.1.1.1")); err == nil {
		t.Error("want error for empty hosts path")
	}
	if _, err := pinHostInHostsFile("/x", "", net.ParseIP("1.1.1.1")); err == nil {
		t.Error("want error for empty host")
	}
	if _, err := pinHostInHostsFile("/x", "h", nil); err == nil {
		t.Error("want error for nil ip")
	}
	// Unwritable/missing path → error (caller proceeds best-effort).
	if _, err := pinHostInHostsFile(filepath.Join(t.TempDir(), "nope", "hosts"), "h", net.ParseIP("1.1.1.1")); err == nil {
		t.Error("want error for unreadable hosts path")
	}
}
