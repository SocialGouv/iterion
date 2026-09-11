package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCloneSource(t *testing.T) {
	accepted := []struct {
		name string
		src  string
	}{
		{"https", "https://github.com/org/repo.git"},
		{"https no .git", "https://example.com/org/repo"},
		{"ssh scheme", "ssh://git@host.example.com/org/repo.git"},
		{"ssh scheme no user", "ssh://host.example.com/org/repo.git"},
		{"scp-like", "git@github.com:org/repo.git"},
		{"scp-like no user", "github.com:org/repo.git"},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if err := ValidateCloneSource(tc.src); err != nil {
				t.Errorf("ValidateCloneSource(%q) = %v, want nil", tc.src, err)
			}
		})
	}

	rejected := []struct {
		name string
		src  string
	}{
		{"ext remote helper", "ext::sh -c 'touch /tmp/pwned'"},
		{"file scheme", "file:///etc/passwd"},
		{"git scheme", "git://github.com/org/repo.git"},
		{"http cleartext", "http://github.com/org/repo.git"},
		{"ftp", "ftp://host/repo"},
		{"arbitrary remote helper", "transport::address"},
		{"empty", ""},
		{"whitespace", "   "},
		{"null byte", "https://h/r\x00"},
		{"absolute path", "/tmp/some/repo"},
		{"relative path", "./repo"},
		{"plain word", "repo"},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			err := ValidateCloneSource(tc.src)
			if err == nil {
				t.Fatalf("ValidateCloneSource(%q) = nil, want error", tc.src)
			}
			// Acceptance #3: user-facing errors explain the source transport
			// is unsupported. Empty/null-byte are the structural exceptions.
			if tc.src != "" && strings.TrimSpace(tc.src) != "" && !strings.Contains(tc.src, "\x00") {
				msg := err.Error()
				if !strings.Contains(msg, "transport") && !strings.Contains(msg, "supported") {
					t.Errorf("error for %q = %q, want it to mention transport/supported", tc.src, msg)
				}
			}
		})
	}
}

func TestShallowCloneCommandPlansKeepNamedRefsSeparateFromCommitPins(t *testing.T) {
	url, dest := "git@github.com:org/repo.git", "/tmp/dest"
	if got := namedRefCloneArgs(url, "release/v1", dest); !sameCloneArgs(got, []string{"clone", "--depth", "1", "--single-branch", "--branch", "release/v1", "--", url, dest}) {
		t.Fatalf("named args = %#v", got)
	}
	if got := commitCloneArgs(url, dest); !sameCloneArgs(got, []string{"clone", "--depth", "1", "--single-branch", "--no-checkout", "--", url, dest}) {
		t.Fatalf("commit args = %#v", got)
	}
	for _, ref := range []string{
		strings.Repeat("a", 40),
		strings.Repeat("A", 40),
		strings.Repeat("a", 39),
		" " + strings.Repeat("a", 40),
		"main",
	} {
		want := len(ref) == 40 && ref == strings.Repeat("a", 40)
		if fullCommitSHA.MatchString(ref) != want {
			t.Fatalf("fullCommitSHA.MatchString(%q) = %t, want %t", ref, fullCommitSHA.MatchString(ref), want)
		}
	}
}

func TestShallowCloneCommitFetchesAndChecksOutExactSHA(t *testing.T) {
	source := t.TempDir()
	gitCloneTest(t, source, "init", "-q")
	gitCloneTest(t, source, "config", "user.name", "Clone Test")
	gitCloneTest(t, source, "config", "user.email", "clone@example.invalid")
	writeCloneTestFile(t, filepath.Join(source, "bundle.txt"), "first\n")
	gitCloneTest(t, source, "add", "--", "bundle.txt")
	gitCloneTest(t, source, "commit", "-qm", "first")
	writeCloneTestFile(t, filepath.Join(source, "bundle.txt"), "pinned\n")
	gitCloneTest(t, source, "commit", "-am", "pinned")
	sha := strings.TrimSpace(gitCloneTest(t, source, "rev-parse", "HEAD"))
	bare := filepath.Join(t.TempDir(), "origin.git")
	gitCloneTest(t, source, "clone", "--bare", source, bare)

	dest := filepath.Join(t.TempDir(), "checkout")
	if err := shallowCloneCommit(context.Background(), "file://"+bare, sha, dest); err != nil {
		t.Fatalf("shallowCloneCommit: %v", err)
	}
	if got := strings.TrimSpace(gitCloneTest(t, dest, "rev-parse", "HEAD")); got != sha {
		t.Fatalf("HEAD = %s, want %s", got, sha)
	}
	if body := string(readCloneTestFile(t, filepath.Join(dest, "bundle.txt"))); body != "pinned\n" {
		t.Fatalf("checked out content = %q", body)
	}
	if err := shallowCloneCommit(context.Background(), "file://"+bare, strings.Repeat("d", 40), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "fetch exact commit") {
		t.Fatalf("unreachable commit error = %v", err)
	}
}

func sameCloneArgs(got, want []string) bool {
	return strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func gitCloneTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", NoAutoMaintenance(append([]string{"-C", dir}, args...)...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeCloneTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCloneTestFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
