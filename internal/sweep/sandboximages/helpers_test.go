package sandboximages

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// parseBotUnit parses a catalog bot as its UNIT — the file and the fragments
// its imports reach — and returns what parser.Parse returns for a single file.
func parseBotUnit(path string) *parser.ParseResult {
	u := unittest.Whole(unit.LoadDir(path))
	return &parser.ParseResult{File: u.Merged, Diagnostics: u.Diagnostics}
}

// compilePlanPhaseBot compiles root/bots/<bot>/main.bot and fails the test on
// any compile error.
func compilePlanPhaseBot(t *testing.T, root, bot string) *ir.Workflow {
	t.Helper()
	path := filepath.Join(root, "bots", bot, "main.bot")
	cr := ir.Compile(parseBotUnit(path).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	return cr.Workflow
}

type ultra11yPin struct {
	Version   string
	SHA256    string
	Size      int64
	Integrity string
}

// rgaaReadPin parses bots/rgaa-audit/ultra11y.pin. Values are exact — a line
// with surrounding whitespace or a CRLF is refused here, before CI, because
// scripts/ultra11y-engine.sh compares them byte for byte.
func rgaaReadPin(t *testing.T, root string) ultra11yPin {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "bots", "rgaa-audit", "ultra11y.pin"))
	if err != nil {
		t.Fatalf("read pin: %v", err)
	}
	var pin ultra11yPin
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(line) != line {
			t.Fatalf("pin line with surrounding whitespace or CRLF: %q", line)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" {
			t.Fatalf("pin line is not key=value: %q", line)
		}
		if seen[key] {
			t.Fatalf("pin key %q repeated", key)
		}
		seen[key] = true
		switch key {
		case "version":
			pin.Version = value
		case "sha256":
			pin.SHA256 = value
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("pin size: %v", err)
			}
			pin.Size = n
		case "integrity":
			pin.Integrity = value
		default:
			t.Fatalf("unknown pin key %q", key)
		}
	}
	for _, k := range []string{"version", "sha256", "size", "integrity"} {
		if !seen[k] {
			t.Fatalf("pin file lacks %q", k)
		}
	}
	return pin
}
