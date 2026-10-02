package unit

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A unit whose root is the filesystem root — the pathological LoadDir of a
// main at "/x.bot" — gets NO root cut: Join("/", rel) is "/rel", so the
// prefix the cut would build is "//", a name nothing carries, and the cut
// would do nothing in silence exactly when the whole filesystem is under
// the root. The refusal is loud instead (#2047): the names stay as they
// are, and one warning says why.
func TestAFilesystemRootUnitRefusesTheRootCutLoudly(t *testing.T) {
	u := &Unit{
		Root: "/",
		Main: "main.bot",
		Diagnostics: []parser.Diagnostic{{
			Code: parser.DiagExpectedToken, Severity: parser.SeverityError,
			File: "/main.bot", Line: 2, Column: 3,
			Message: "cannot read /main.bot beside /etc/passwd",
		}},
	}
	u.RelDiagnostics()
	if len(u.Diagnostics) != 2 {
		t.Fatalf("want the diagnostic kept and one warning appended, got %v", u.Diagnostics)
	}
	d := u.Diagnostics[0]
	if d.File != "/main.bot" || !strings.Contains(d.Message, "/etc/passwd") {
		t.Errorf("the cut ran on a filesystem-root unit: %+v", d)
	}
	w := u.Diagnostics[1]
	if w.Code != parser.DiagFilesystemRoot || w.Severity != parser.SeverityWarning || w.Hint == "" {
		t.Errorf("the refusal is not a catalogued warning: %+v", w)
	}
	// A second pass warns no second time.
	u.RelDiagnostics()
	if len(u.Diagnostics) != 2 {
		t.Errorf("the refusal was said twice: %v", u.Diagnostics)
	}
	// The texts that ride no diagnostic no-op the same way.
	if got := u.RelText("open /main.bot: no"); got != "open /main.bot: no" {
		t.Errorf("RelText = %q, want the text unchanged", got)
	}
	if got := RelTextRoot("/", "open /main.bot: no"); got != "open /main.bot: no" {
		t.Errorf("RelTextRoot = %q, want the text unchanged", got)
	}
	if got := u.RelName("/main.bot"); got != "/main.bot" {
		t.Errorf("RelName = %q, want the name unchanged", got)
	}
	if err := u.RelError(errors.New("open /main.bot: no")); err.Error() != "open /main.bot: no" {
		t.Errorf("RelError = %v, want the error unchanged", err)
	}
}

// A relative root — ".", the fixpoint filepath.Dir shares with "/" — is no
// filesystem root: no E048, no refused cut.
func TestARelativeRootIsNotAFilesystemRoot(t *testing.T) {
	u := &Unit{
		Root: ".",
		Main: "main.bot",
		Diagnostics: []parser.Diagnostic{{
			Code: parser.DiagExpectedToken, Severity: parser.SeverityError,
			File: "main.bot", Line: 1, Column: 1, Message: "m",
		}},
	}
	u.RelDiagnostics()
	if u.RootCutRefused() {
		t.Error("a relative root refuses no cut")
	}
	for _, d := range u.Diagnostics {
		if d.Code == parser.DiagFilesystemRoot {
			t.Errorf("E048 on a relative root: %v", d)
		}
	}
}

// The ordinary cut the refusal must never disable: a root that is a
// directory comes off every name under it.
func TestAnOrdinaryRootStillCuts(t *testing.T) {
	u := &Unit{
		Root: "/srv/bot",
		Main: "main.bot",
		Diagnostics: []parser.Diagnostic{{
			Code: parser.DiagExpectedToken, Severity: parser.SeverityError,
			File: "/srv/bot/main.bot", Line: 2, Column: 3,
			Message: "cannot read /srv/bot/lib/a.bot",
		}},
	}
	u.RelDiagnostics()
	if len(u.Diagnostics) != 1 {
		t.Fatalf("no warning belongs on an ordinary unit: %v", u.Diagnostics)
	}
	d := u.Diagnostics[0]
	if d.File != "main.bot" || d.Message != "cannot read lib/a.bot" {
		t.Errorf("the cut did not rewrite the names: %+v", d)
	}
	if got := u.RelText("open /srv/bot/main.bot: no"); got != "open main.bot: no" {
		t.Errorf("RelText = %q", got)
	}
	if got := u.RelName("/srv/bot/main.bot"); got != "main.bot" {
		t.Errorf("RelName = %q", got)
	}
	if err := u.RelError(errors.New("open /srv/bot/main.bot: no")); err.Error() != "open main.bot: no" {
		t.Errorf("RelError = %v", err)
	}
}
