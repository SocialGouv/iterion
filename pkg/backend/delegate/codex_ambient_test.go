package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestCodexConfigCarriesTheWorkspaceSwitchOnBothPasses(t *testing.T) {
	for _, c := range []struct {
		policy  ambient.Policy
		docsOff bool
	}{
		{ambient.All, false},
		{ambient.Workspace, false},
		{ambient.Operator, true},
		{ambient.None, true},
	} {
		task := Task{AmbientContext: c.policy}
		work := &codexsdk.CodexAgentOptions{}
		codexWebSearchOption(task)(work) // the work pass's option
		for pass, cfg := range map[string]map[string]string{
			"work":   work.Config,
			"format": codexConfig(task, codexWebSearchModeDisabled),
		} {
			if cfg["web_search"] == "" {
				t.Errorf("%v/%s: web_search dropped from %v (WithConfig replaces the map)", c.policy, pass, cfg)
			}
			got, set := cfg["project_doc_max_bytes"]
			if set != c.docsOff || (set && got != "0") {
				t.Errorf("%v/%s: project_doc_max_bytes=%q set=%v, want set=%v", c.policy, pass, got, set, c.docsOff)
			}
		}
	}
}

func codexHomeFixture(t *testing.T, withInstructions bool) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".codex")
	mustWriteFile(t, filepath.Join(home, "auth.json"), `{"tokens":"old"}`)
	mustWriteFile(t, filepath.Join(home, "config.toml"), "model = \"x\"\n")
	if withInstructions {
		mustWriteFile(t, filepath.Join(home, "AGENTS.md"), "operator instructions\n")
		mustWriteFile(t, filepath.Join(home, "AGENTS.override.md"), "operator override\n")
	}
	return home
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHomeIsLeftAloneWithoutInstructionFiles(t *testing.T) {
	home := codexHomeFixture(t, false)
	dir, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		t.Fatal(err)
	}
	if dir != home {
		t.Fatalf("dir = %s, want the home itself when it holds no instruction file", dir)
	}
	if _, err := os.Stat(filepath.Join(home, codexOverlayDirName)); !os.IsNotExist(err) {
		t.Fatalf("an overlay was built for nothing: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if dir, _, err := codexHomeWithoutInstructions(filepath.Join(t.TempDir(), "absent")); err != nil || dir == "" {
		t.Fatalf("a missing home: dir=%q err=%v, want itself and no error", dir, err)
	}
}

func TestCodexHomeOverlayHidesInstructionsAndCarriesWritesBack(t *testing.T) {
	home := codexHomeFixture(t, true)
	dir, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		t.Fatal(err)
	}
	if dir == home || filepath.Dir(filepath.Dir(dir)) != home {
		t.Fatalf("overlay %s is not a fresh directory inside %s", dir, home)
	}
	for _, hidden := range codexInstructionFiles {
		if _, err := os.Lstat(filepath.Join(dir, hidden)); !os.IsNotExist(err) {
			t.Errorf("%s is visible in the overlay: %v", hidden, err)
		}
	}
	for _, linked := range []string{"auth.json", "config.toml", "sessions"} {
		info, err := os.Lstat(filepath.Join(dir, linked))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink into the home: %v", linked, err)
		}
	}
	if info, err := os.Stat(filepath.Join(home, "sessions")); err != nil || !info.IsDir() {
		t.Fatalf("sessions/ was not created in the real home: %v", err)
	}

	// codex refreshes its token by replacing auth.json, and starts a history.
	tmp := filepath.Join(dir, "auth.json.tmp")
	mustWriteFile(t, tmp, `{"tokens":"rotated"}`)
	if err := os.Rename(tmp, filepath.Join(dir, "auth.json")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, "history.jsonl"), "{}\n")

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(b) != `{"tokens":"rotated"}` {
		t.Errorf("home auth.json = %s: the rotated token was lost with the overlay", b)
	}
	if _, err := os.Stat(filepath.Join(home, "history.jsonl")); err != nil {
		t.Errorf("a file codex created was not carried back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, codexOverlayDirName)); !os.IsNotExist(err) {
		t.Errorf("the overlay root survived its last overlay: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "AGENTS.md")); string(b) != "operator instructions\n" {
		t.Errorf("the operator's AGENTS.md was touched: %q", b)
	}
}

func TestCodexOverlayReportsADirectoryItCannotCarryBack(t *testing.T) {
	home := codexHomeFixture(t, true)
	if err := os.MkdirAll(filepath.Join(home, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		t.Fatal(err)
	}
	// codex replaced the linked log/ with a real directory.
	if err := os.Remove(filepath.Join(dir, "log")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := release(); err == nil {
		t.Fatal("release merged or dropped a directory in silence; it must report it")
	}
}

func TestCodexSpawnEnvMovesTheHomeOnlyWithoutTheOperator(t *testing.T) {
	home := codexHomeFixture(t, true)
	t.Setenv("CODEX_HOME", home)
	for _, c := range []struct {
		policy ambient.Policy
		moved  bool
	}{
		{ambient.All, false},
		{ambient.Operator, false},
		{ambient.Workspace, true},
		{ambient.None, true},
	} {
		env, release, err := codexSpawnEnv(context.Background(), Task{AmbientContext: c.policy})
		if err != nil {
			t.Fatalf("%v: %v", c.policy, err)
		}
		moved := env["CODEX_HOME"] != "" && env["CODEX_HOME"] != home
		if moved != c.moved {
			t.Errorf("%v: CODEX_HOME=%q, moved=%v want %v", c.policy, env["CODEX_HOME"], moved, c.moved)
		}
		if err := release(); err != nil {
			t.Errorf("%v: release: %v", c.policy, err)
		}
	}
}

func TestCodexOverlaySweepReleasesStaleSiblings(t *testing.T) {
	home := codexHomeFixture(t, true)
	// A leftover from a killed process: one symlink, one file codex replaced,
	// directory mtime backdated past the sweep age.
	stale := filepath.Join(home, codexOverlayDirName, "home-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "config.toml"), filepath.Join(stale, "config.toml")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(stale, "auth.json"), `{"tokens":"rotated-by-the-crashed-node"}`)
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A young sibling belongs to a live node and must survive.
	fresh := filepath.Join(home, codexOverlayDirName, "home-fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}

	_, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale overlay survived the sweep: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(b) != `{"tokens":"rotated-by-the-crashed-node"}` {
		t.Errorf("home auth.json = %s: the crashed node's carry-back was lost", b)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the live node's overlay was swept: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
