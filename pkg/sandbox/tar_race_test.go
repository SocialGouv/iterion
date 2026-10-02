package sandbox

import (
	"strings"
	"testing"
)

// TestOnlyTarRaceWarnings: tar's warnings for a tree that changed while it
// was archived are recognised alone, with or without the trailer kubectl
// exec adds; anything else on stderr is a failure.
func TestOnlyTarRaceWarnings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   bool
	}{
		{"a rewritten file", "tar: ./a.log: file changed as we read it\n", true},
		{"a file listed then removed", "tar: ./tmp/part.json: File removed before we read it\n", true},
		{"several racing writers", "tar: ./a.log: file changed as we read it\ntar: ./tmp/part.json: File removed before we read it\n", true},
		{"through kubectl exec", "tar: ./server.log: file changed as we read it\n" + KubectlRemoteExit1 + "\n", true},
		{"through kubectl exec, removed", "tar: ./.git/index.lock: File removed before we read it\n" + KubectlRemoteExit1, true},
		{"a racing writer and another tar error", "tar: ./a: file changed as we read it\ntar: ./b: Cannot open: Permission denied", false},
		{"the same through kubectl exec", "tar: ./a: file changed as we read it\ntar: ./b: Cannot open: Permission denied\n" + KubectlRemoteExit1, false},
		{"the kubectl trailer alone", KubectlRemoteExit1, false},
		{"kubectl's own failure", "error: unable to upgrade connection: container not found", false},
		{"another exit code's trailer", "tar: ./a: file changed as we read it\ncommand terminated with exit code 2", false},
		{"a warning without tar's prefix", "./a.log: file changed as we read it", false},
		{"a socket beside a racing writer", "tar: ./server.log: file changed as we read it\ntar: ./app.sock: socket ignored\n" + KubectlRemoteExit1, true},
		{"a socket alone", "tar: ./app.sock: socket ignored", false},
		{"nothing", "", false},
	} {
		if got := OnlyTarRaceWarnings(tc.stderr); got != tc.want {
			t.Errorf("%s: OnlyTarRaceWarnings(%q) = %v, want %v", tc.name, tc.stderr, got, tc.want)
		}
	}
}

// TestTarRace_namesTheMembers: the members a race names, a socket's notice
// named as none.
func TestTarRace_namesTheMembers(t *testing.T) {
	members, only := TarRace("tar: ./state.db: file changed as we read it\ntar: ./floor.json.tmp: File removed before we read it\ntar: ./app.sock: socket ignored\ntar: .: file changed as we read it\n" + KubectlRemoteExit1)
	if !only || strings.Join(members, " ") != "./state.db ./floor.json.tmp ." {
		t.Fatalf("TarRace = %q, %v; want the three raced members, a race only", members, only)
	}
}
