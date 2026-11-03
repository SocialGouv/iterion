package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTestPatternDoesNotHideAFailingLongName(t *testing.T) {
	dir := t.TempDir()
	longName := "Test" + strings.Repeat("Long", 3000)
	for name, body := range map[string]string{
		"go.mod":       "module example.test/longname\n\ngo 1.26\n",
		"long_test.go": "package longname\nimport \"testing\"\nfunc TestShort(t *testing.T) {}\nfunc " + longName + "(t *testing.T) { t.Fatal(\"long name remains selected\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	var pattern bytes.Buffer
	if err := runTests(1, 0, false, ".", time.Minute, &pattern); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("go", "test", "-mod=vendor", "-count=1", "-run", strings.TrimSpace(pattern.String()), ".").CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("long name remains selected")) {
		t.Fatalf("partition hid the failing long test: err %v, pattern bytes %d, output %s", err, pattern.Len(), output)
	}
}
