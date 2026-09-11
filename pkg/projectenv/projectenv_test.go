package projectenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotEvaluatesShellWithoutMutatingProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BASE_SUFFIX=${BASE}-tail\nPROJECT_ONLY='two words'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := os.Getenv("PROJECT_ONLY")
	env, err := Snapshot([]string{"HOME=/host/home", "BASE=head", "PATH=" + os.Getenv("PATH")}, path)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := toMap(env)
	if got["BASE_SUFFIX"] != "head-tail" || got["PROJECT_ONLY"] != "two words" {
		t.Fatalf("effective env = %#v", got)
	}
	if os.Getenv("PROJECT_ONLY") != before {
		t.Fatal("Snapshot mutated the parent process environment")
	}
}

func TestSnapshotRejectsReservedOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("ITERION_HOME=/tmp/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Snapshot([]string{"HOME=/host/home", "ITERION_HOME=/host/iterion", "PATH=" + os.Getenv("PATH")}, path)
	if err == nil || !strings.Contains(err.Error(), "ITERION_HOME") {
		t.Fatalf("reserved override error = %v", err)
	}
}
