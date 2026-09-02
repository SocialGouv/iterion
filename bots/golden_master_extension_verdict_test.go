package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenMasterExtensionVerdictBinaryReference pins the certifier's
// behaviour on a reference blob that is not valid UTF-8.
//
// `extension_verdict` reads every judged blob through `git show` with
// text=True. On a reference carrying a byte like 0xff that raised an uncaught
// UnicodeDecodeError and took the whole mode down with a traceback: the run
// lost its verdict entirely, so every legitimate extension beside it was
// refused with a message naming nothing. It failed closed, which is why this
// is a robustness fix and not a hole — but "refused, not crashed" is the
// standard this harness states for itself.
//
// The decode is LOSSLESS (surrogateescape), and the second case is what pins
// that: "replace" would also stop the crash while mapping every bad byte onto
// one character, so two blobs differing only there would compare equal. The
// rewrite of an existing binary reference — the masking vector this whole
// mechanism exists to refuse — must still be caught.
func TestGoldenMasterExtensionVerdictBinaryReference(t *testing.T) {
	for _, tool := range []string{"python3", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}

	harness, err := os.ReadFile("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Fatalf("git %v: %v (%s)", args, cerr, out)
		}
	}
	write := func(rel string, content []byte) {
		t.Helper()
		if werr := os.WriteFile(filepath.Join(ws, rel), content, 0o644); werr != nil {
			t.Fatal(werr)
		}
	}
	appendLedger := func(kind, body string) {
		t.Helper()
		f, oerr := os.OpenFile(filepath.Join(ws, ".golden-master/EXTENSIONS.md"),
			os.O_APPEND|os.O_WRONLY, 0o644)
		if oerr != nil {
			t.Fatal(oerr)
		}
		defer f.Close()
		if _, werr := f.WriteString("\n<!-- iterion:extension-" + kind + "\n" + body + "\n-->\n"); werr != nil {
			t.Fatal(werr)
		}
	}

	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if merr := os.MkdirAll(filepath.Join(ws, ".golden-master/refs"), 0o755); merr != nil {
		t.Fatal(merr)
	}
	write(".golden-master/harness.py", harness)
	write(".golden-master/corpus.json", []byte(`{"entries":[{"id":"a","method":"GET","path":"/a"}]}`+"\n"))
	write(".golden-master/EXTENSIONS.md", []byte("# Extensions\n"))
	write(".golden-master/refs/a.txt", []byte("ref A\n"))
	git("add", "-A")
	git("commit", "-qm", "base")

	head := func() string {
		t.Helper()
		out, rerr := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
		if rerr != nil {
			t.Fatal(rerr)
		}
		return string(out[:len(out)-1])
	}

	type row struct {
		ID       string   `json:"id"`
		OK       bool     `json:"ok"`
		Problems []string `json:"problems"`
	}
	type verdictOut struct {
		Acted    []row    `json:"acted"`
		OkPaths  []string `json:"ok_paths"`
		Problems []string `json:"problems"`
		Error    string   `json:"error"`
	}
	verify := func(base string) verdictOut {
		t.Helper()
		cmd := exec.Command("python3", filepath.Join(ws, ".golden-master/harness.py"))
		cmd.Dir = ws
		cmd.Env = append(os.Environ(),
			"GM_MODE=extend-verify",
			"GM_WORKSPACE="+ws,
			"GM_DIR=.golden-master",
			"GM_BASE="+base,
		)
		out, rerr := cmd.Output()
		if rerr != nil {
			t.Fatalf("extend-verify crashed instead of returning a verdict: %v\n"+
				"a mode that dies takes the whole run's verdict with it, and every "+
				"legitimate extension beside this one is refused naming nothing.\n"+
				"stdout: %s", rerr, out)
		}
		var v verdictOut
		if uerr := json.Unmarshal(out, &v); uerr != nil {
			t.Fatalf("extend-verify output is not JSON: %v (out %q)", uerr, out)
		}
		return v
	}

	// The lot files a request; the net's subbot acts it in a LATER commit —
	// filing and acting are different powers, and sharing a commit is refused.
	binary := []byte("ref \xff\xfe binary\n")

	t.Run("a non-UTF-8 reference is judged, not crashed on", func(t *testing.T) {
		base := head()
		appendLedger("request", `{"id": "X1", "lot": "L1", "paths": [".golden-master/refs/b.txt"]}`)
		git("add", "-A")
		git("commit", "-qm", "lot: file the request")
		write(".golden-master/refs/b.txt", binary)
		appendLedger("act", `{"id": "X1", "recorded_paths": [".golden-master/refs/b.txt"]}`)
		git("add", "-A")
		git("commit", "-qm", "subbot: act it")

		v := verify(base)
		if v.Error != "" {
			t.Fatalf("verdict carries an error: %s", v.Error)
		}
		if len(v.Acted) != 1 || !v.Acted[0].OK {
			t.Fatalf("a pure addition was refused: %+v", v.Acted)
		}
		// It IS an addition: absent at base, present at HEAD, a regular file,
		// claimed by its request. Being unreadable as text is not a masking
		// vector, and refusing it would be a different (unstated) policy.
		if len(v.OkPaths) != 1 || v.OkPaths[0] != ".golden-master/refs/b.txt" {
			t.Fatalf("ok_paths = %v, want the added reference", v.OkPaths)
		}
	})

	t.Run("rewriting an existing non-UTF-8 reference is still refused", func(t *testing.T) {
		base := head()
		appendLedger("request", `{"id": "X2", "lot": "L2", "paths": [".golden-master/refs/b.txt"]}`)
		git("add", "-A")
		git("commit", "-qm", "lot: file a second request")
		write(".golden-master/refs/b.txt", []byte("ref \xff\xfe TAMPERED\n"))
		appendLedger("act", `{"id": "X2", "recorded_paths": [".golden-master/refs/b.txt"]}`)
		git("add", "-A")
		git("commit", "-qm", "act: rewrite the existing binary reference")

		v := verify(base)
		if len(v.OkPaths) != 0 {
			t.Fatalf("a rewrite of an existing reference was exempted: ok_paths = %v", v.OkPaths)
		}
		var found bool
		for _, r := range v.Acted {
			if r.ID == "X2" {
				if r.OK {
					t.Fatalf("the rewriting act was certified ok: %+v", r)
				}
				for _, p := range r.Problems {
					if strings.Contains(p, "existed at base") {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("the rewrite was not named as one — a lossy decode would "+
				"make two blobs differing only in their bad bytes compare "+
				"equal: %+v", v.Acted)
		}
	})
}
