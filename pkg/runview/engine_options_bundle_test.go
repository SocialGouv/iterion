package runview

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A bundle that fails to open drops its skills, prompts, recipes and
// attachments from the run — and the engine then answers from the model's
// priors, which reads as "the bot got dumber" rather than as a typo. The
// branch made assembleBundle hard-error on a missing export path, so ONE
// stale exports.workflows[].path is enough to reach this. The error used
// to be discarded by an `err == nil &&` guard; it must be named.
func TestEngineOptionsNamesABundleItCouldNotOpen(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "mybot")
	if err := os.MkdirAll(filepath.Join(bundleDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(main, []byte("workflow w:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stale export path — the file it names does not exist.
	manifest := "name: mybot\nversion: 0.1.0\nexports:\n  workflows:\n    - id: extra\n      path: workflows/gone.bot\n"
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	s := &Service{logger: iterlog.New(iterlog.LevelWarn, &logs)}
	opts := s.engineOptions(s.logger, "hash", main, "run-name", finalizationOpts{}, launchExtras{})
	if len(opts) == 0 {
		t.Fatal("engineOptions returned nothing")
	}
	if !strings.Contains(logs.String(), "could not be opened") {
		t.Fatalf("a bundle that failed to open was dropped in silence; logs = %q", logs.String())
	}
	if !strings.Contains(logs.String(), bundleDir) {
		t.Fatalf("the warning does not name the bundle; logs = %q", logs.String())
	}
}

// The mirror: a healthy bundle still loads, and a standalone .bot outside
// any bundle stays silent — the warning must not fire on the shape it was
// always allowed to have.
func TestEngineOptionsStaysSilentForAHealthyOrAbsentBundle(t *testing.T) {
	dir := t.TempDir()
	standalone := filepath.Join(dir, "solo.bot")
	if err := os.WriteFile(standalone, []byte("workflow w:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(dir, "goodbot")
	if err := os.MkdirAll(filepath.Join(bundleDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	healthy := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(healthy, []byte("workflow w:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("name: goodbot\nversion: 0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{standalone, healthy} {
		var logs bytes.Buffer
		s := &Service{logger: iterlog.New(iterlog.LevelWarn, &logs)}
		s.engineOptions(s.logger, "hash", path, "run-name", finalizationOpts{}, launchExtras{})
		if strings.Contains(logs.String(), "could not be opened") {
			t.Fatalf("%s warned about a bundle it could open; logs = %q", path, logs.String())
		}
	}
}
