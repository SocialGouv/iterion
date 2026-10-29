package botinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A --force install REMOVES the target whole before copying the source over
// it. `--dest bots` is the committable location `bots install --help` names,
// and that is where AUTHORED bundles live: pointed there, --force destroyed
// hand-written work with no lock, no version and no diff (#2689 item 2). The
// flag's own help says it overwrites "an existing install", and the record of
// what this installer installed is its own origin file.
func TestForceRefusesToReplaceABundleTheInstallerDidNotInstall(t *testing.T) {
	repo := t.TempDir()
	writeBundle(t, repo, "mybot", 0)
	authored := t.TempDir()
	wd := t.TempDir()

	// The authored bundle: a bot someone wrote, with a file no install of
	// `mybot` would ever produce.
	target := filepath.Join(authored, "mybot")
	writeBundle(t, target, "mybot", 0)
	keepsake := filepath.Join(target, "skills", "hand-written.md")
	if err := os.WriteFile(keepsake, []byte("# authored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Install(context.Background(), Options{Source: repo, Dest: authored, Workdir: wd, Force: true})
	if err == nil {
		t.Fatal("--force over authored work succeeded; the bundle is gone")
	}
	for _, want := range []string{"no record of installing it", "--replace-unrecorded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(keepsake); statErr != nil {
		t.Fatalf("the refused install deleted the authored file anyway: %v", statErr)
	}

	// The escape hatch is reachable and does what it says: the operator's
	// explicit override is never silently replaced by the refusal.
	if _, err := Install(context.Background(), Options{
		Source: repo, Dest: authored, Workdir: wd, Force: true, ReplaceUnrecorded: true,
	}); err != nil {
		t.Fatalf("--replace-unrecorded should go through: %v", err)
	}
	if _, statErr := os.Stat(keepsake); statErr == nil {
		t.Fatal("the override did not replace the bundle")
	}
}

// The installer's OWN installs still take --force: the refusal must not cost
// the capability the flag exists for.
func TestForceStillReplacesTheInstallersOwnInstall(t *testing.T) {
	repo := t.TempDir()
	writeBundle(t, repo, "mybot", 0)
	dest := t.TempDir()
	wd := t.TempDir()
	if _, err := Install(context.Background(), Options{Source: repo, Dest: dest, Workdir: wd}); err != nil {
		t.Fatal(err)
	}
	// The origin record the install wrote is what makes this target its own.
	if _, err := ReadOrigin(filepath.Join(dest, "mybot")); err != nil {
		t.Fatalf("an install with no origin record cannot be recognized later: %v", err)
	}
	if _, err := Install(context.Background(), Options{Source: repo, Dest: dest, Workdir: wd, Force: true}); err != nil {
		t.Fatalf("--force over the installer's own install: %v", err)
	}
}

// An origin record that cannot be READ is not a licence to delete: whether
// the target is authored work is then unknown, and the installer says so
// instead of guessing the permissive answer.
func TestForceRefusesWhenTheInstallLedgerCannotBeRead(t *testing.T) {
	repo := t.TempDir()
	writeBundle(t, repo, "mybot", 0)
	dest := t.TempDir()
	wd := t.TempDir()
	if _, err := Install(context.Background(), Options{Source: repo, Dest: dest, Workdir: wd}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dest, "mybot")
	if err := os.WriteFile(originPath(target), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), Options{Source: repo, Dest: dest, Workdir: wd, Force: true})
	if err == nil {
		t.Fatal("--force over an unreadable ledger succeeded")
	}
	for _, want := range []string{"install record cannot be read", "--replace-unrecorded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(target, "main.bot")); statErr != nil {
		t.Fatalf("the refused install deleted the bundle anyway: %v", statErr)
	}
}

// A target that is a FILE where a bundle is expected carries no origin
// record either, so --force refuses it too rather than unlinking whatever is
// in the way.
func TestForceRefusesANonBundleInTheWay(t *testing.T) {
	repo := t.TempDir()
	writeBundle(t, repo, "mybot", 0)
	dest := t.TempDir()
	wd := t.TempDir()
	stranger := filepath.Join(dest, "mybot")
	if err := os.WriteFile(stranger, []byte("not a bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), Options{Source: repo, Dest: dest, Workdir: wd, Force: true}); err == nil {
		t.Fatal("--force unlinked a stranger it has no record of")
	}
	if _, statErr := os.Stat(stranger); statErr != nil {
		t.Fatalf("the refused install removed the file anyway: %v", statErr)
	}
}
