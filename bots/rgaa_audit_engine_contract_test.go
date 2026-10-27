package bots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The rgaa-audit bot (Acci) delegates every RGAA judgement to the ultra11y
// engine: the static rules, the worklist of residual criteria, the verdict
// contract its fold enforces, the French report and its integrity checks, the
// issue set. The bot only orchestrates. That makes the engine's CLI contract
// the bot's load-bearing wall — and ultra11y is deterministic and offline for
// everything but `scan`, so the wall can be exercised in CI against the REAL
// pinned engine instead of a stub that is kinder than the attacker.
//
// These tests run the pinned engine on testdata/rgaa-audit-site (two HTML
// pages and one TSX component with planted defects) and assert the shapes the
// bot reads: exit codes, JSON keys, counters, file layout. Every assertion
// below was measured on ultra11y 5.45.0; a pin bump that changes one of them
// is a migration, and this file is where it shows first.
//
// Facts worth their weight (all measured, all relied on by the bot):
//   - `verify --apply` folds PARTIALLY by default: exit 0 as soon as one
//     verdict lands, refusals listed in `issues`/`rejectedCriteria`; exit 1
//     only when nothing lands or under `--strict`. A criterion absent from the
//     file is refused as a coverage gap, and stays "à évaluer".
//   - `audit --standard rgaa` writes a `pack-audit` document whose ROOT
//     `conformancePct` is the AUTOMATIC pass rate (engine-decided C over
//     engine-decided C + NC, agent verdicts excluded); `report --json` and
//     the fold JSON echo the core (WCAG) rate instead. The RGAA rate — validated over
//     validated + NC + à évaluer, NA excluded — appears in no JSON output;
//     the report prints it, and the bot computes it from criteria[].
//   - `check --require-decided=pages --json` nests the undecided list under
//     `decided.undecided`.
//   - `tickets` resolves a provider even with `--out --dry-run` and its
//     `list()` attempts one network call; the file is written regardless.
//   - The C gate grounds citations on the SOURCE (file exists, snippet
//     matches when given), not on the evidence the item was shown: a stray
//     citation is accepted. The bot checks citations ⊆ evidence itself.
//   - Reports split the business summary (RGAA rate) and the technical annex
//     (Décidés / automatic rate). check and verify follow the annex link.
//   - Partial/manual folds no longer double count open criteria; re-folding
//     a decided criterion returns a structured refusal. The bot still checks
//     report counts, folds the real audit once, and never treats a crash as
//     a successful refusal.
//
// The engine comes from ULTRA11Y_BIN (scripts/ultra11y-engine.sh prints it,
// installing the pinned version when needed). Without it the tests skip
// locally and FAIL in CI: a green CI that never ran the engine would be the
// façade this file exists to prevent.

const (
	rgaaAuditPinFile  = "rgaa-audit/ultra11y.pin"
	rgaaEngineLockDir = "../scripts/ultra11y-engine"
	rgaaAuditFixture  = "testdata/rgaa-audit-site"
	rgaaEngineTimeout = 120 * time.Second
)

type ultra11yPin struct {
	Version   string
	SHA256    string
	Size      int64
	Integrity string
}

// rgaaReadPin parses bots/rgaa-audit/ultra11y.pin. Values are exact — a line
// with surrounding whitespace or a CRLF is refused here, before CI, because
// scripts/ultra11y-engine.sh compares them byte for byte.
func rgaaReadPin(t *testing.T) ultra11yPin {
	t.Helper()
	raw, err := os.ReadFile(rgaaAuditPinFile)
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

func TestRgaaAuditEnginePinFileIsWellFormed(t *testing.T) {
	pin := rgaaReadPin(t)
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(pin.Version) {
		t.Fatalf("pin version %q is not a release version", pin.Version)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin.SHA256) {
		t.Fatalf("pin sha256 %q is not 64 hex chars", pin.SHA256)
	}
	if pin.Size <= 0 {
		t.Fatalf("pin size must be positive, got %d", pin.Size)
	}
	if !regexp.MustCompile(`^sha512-[A-Za-z0-9+/]+=*$`).MatchString(pin.Integrity) {
		t.Fatalf("pin integrity %q is not an npm sha512 integrity string", pin.Integrity)
	}
	// scripts/ultra11y-pin.sh is the grep point CI and the Taskfile use; it
	// must print exactly what this test reads (one line, no stray whitespace).
	script := filepath.Join("..", "scripts", "ultra11y-pin.sh")
	for field, want := range map[string]string{"version": pin.Version, "sha256": pin.SHA256, "size": strconv.FormatInt(pin.Size, 10), "integrity": pin.Integrity} {
		out, err := exec.Command("bash", script, field).Output()
		if err != nil {
			t.Fatalf("ultra11y-pin.sh %s: %v", field, err)
		}
		if string(out) != want+"\n" {
			t.Fatalf("ultra11y-pin.sh %s printed %q, pin file says %q", field, out, want)
		}
	}
	// The install-time pin (scripts/ultra11y-engine/package{,-lock}.json, the
	// `npm ci` input) must name the same version and tarball integrity.
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	rgaaReadJSON(t, filepath.Join(rgaaEngineLockDir, "package.json"), &pkg)
	if pkg.Dependencies["ultra11y"] != pin.Version {
		t.Fatalf("scripts/ultra11y-engine/package.json pins ultra11y %q, the pin file says %q", pkg.Dependencies["ultra11y"], pin.Version)
	}
	var lock struct {
		Packages map[string]struct {
			Version   string `json:"version"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	rgaaReadJSON(t, filepath.Join(rgaaEngineLockDir, "package-lock.json"), &lock)
	locked, ok := lock.Packages["node_modules/ultra11y"]
	if !ok || locked.Version != pin.Version || locked.Integrity != pin.Integrity {
		t.Fatalf("scripts/ultra11y-engine/package-lock.json locks ultra11y %+v, the pin file says %s / %s", locked, pin.Version, pin.Integrity)
	}
}

// rgaaCheckEngine is the Go twin of the `ok()` guard in
// scripts/ultra11y-engine.sh and the rule the bot's `prepare` node follows: a
// candidate entry file is fingerprinted (size, then sha256) BEFORE anything
// of it runs, and only a file carrying the pinned fingerprint is executed to
// confirm the pinned version. Either check alone would accept a moved tag or
// a different file with the right banner. The hash covers the entry file —
// its dependency closure is pinned at install time by the lockfile.
func rgaaCheckEngine(bin string, pin ultra11yPin) error {
	st, err := os.Stat(bin)
	if err != nil {
		return err
	}
	if st.Size() != pin.Size {
		return fmt.Errorf("engine size %d, pin wants %d", st.Size(), pin.Size)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != pin.SHA256 {
		return fmt.Errorf("engine sha256 %s, pin wants %s", got, pin.SHA256)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rgaaEngineTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("--version: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if got := lines[len(lines)-1]; got != pin.Version {
		return fmt.Errorf("engine version %q, pin wants %q", got, pin.Version)
	}
	return nil
}

// rgaaResolveEngine returns the pinned engine's entry file, or skips locally /
// fails in CI when ULTRA11Y_BIN is not set. An explicit ULTRA11Y_BIN that is
// not the pinned engine fails: it is never silently replaced.
func rgaaResolveEngine(t *testing.T) (string, ultra11yPin) {
	t.Helper()
	pin := rgaaReadPin(t)
	bin := os.Getenv("ULTRA11Y_BIN")
	if bin == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("ULTRA11Y_BIN is unset in CI — the workflow must install the pinned engine (scripts/ultra11y-engine.sh) before go test")
		}
		t.Skip("ULTRA11Y_BIN unset — export ULTRA11Y_BIN=$(scripts/ultra11y-engine.sh) to run the engine contract")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required to run the pinned engine: %v", err)
	}
	if err := rgaaCheckEngine(bin, pin); err != nil {
		t.Fatalf("ULTRA11Y_BIN does not match the pin: %v", err)
	}
	return bin, pin
}

func TestRgaaAuditEngineMatchesPin(t *testing.T) {
	bin, pin := rgaaResolveEngine(t)

	t.Run("a_file_with_the_wrong_fingerprint_is_refused_without_being_run", func(t *testing.T) {
		// The stub would write a marker if executed; the fingerprint must
		// refuse it first.
		dir := t.TempDir()
		marker := filepath.Join(dir, "executed")
		stub := filepath.Join(dir, "ultra11y.mjs")
		src := "require('fs').writeFileSync(" + strconv.Quote(marker) + ", 'ran');console.log('" + pin.Version + "')\n"
		if err := os.WriteFile(stub, []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
		err := rgaaCheckEngine(stub, pin)
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("stub engine accepted or refused for the wrong reason: %v", err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the unverified candidate was executed before its fingerprint was checked")
		}
	})

	t.Run("the_right_size_with_a_different_hash_is_refused", func(t *testing.T) {
		raw, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		tampered := filepath.Join(t.TempDir(), "ultra11y.mjs")
		// Same size, one byte flipped.
		flipped := append([]byte(nil), raw...)
		flipped[len(flipped)/2] ^= 0x01
		if err := os.WriteFile(tampered, flipped, 0o755); err != nil {
			t.Fatal(err)
		}
		err = rgaaCheckEngine(tampered, pin)
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("tampered engine accepted or refused for the wrong reason: %v", err)
		}
	})

	t.Run("the_resolver_refuses_an_explicit_ULTRA11Y_BIN_that_is_not_the_pin", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "executed")
		stub := filepath.Join(dir, "ultra11y.mjs")
		src := "require('fs').writeFileSync(" + strconv.Quote(marker) + ", 'ran');console.log('" + pin.Version + "')\n"
		if err := os.WriteFile(stub, []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", filepath.Join("..", "scripts", "ultra11y-engine.sh"))
		cmd.Env = append(os.Environ(), "ULTRA11Y_BIN="+stub, "ULTRA11Y_HOME="+filepath.Join(dir, "never-used"))
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("the resolver substituted another engine for an explicit ULTRA11Y_BIN: %s", out)
		}
		if !strings.Contains(string(out), "refusing to substitute") {
			t.Fatalf("the resolver failed for another reason than the explicit refusal: %s", out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the resolver executed the unverified candidate before fingerprinting it")
		}
	})

	t.Run("the_resolver_prints_the_resolved_absolute_path_of_the_pinned_engine", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "engine-link.mjs")
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(abs, link); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", filepath.Join("..", "scripts", "ultra11y-engine.sh"))
		cmd.Env = append(os.Environ(), "ULTRA11Y_BIN="+link)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("resolver: %v", err)
		}
		want, err := filepath.EvalSymlinks(abs)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want+"\n" {
			t.Fatalf("resolver printed %q, want the symlink-resolved absolute path %q", out, want)
		}
	})
}

func TestRgaaAuditEnginePrivateInstallRetriesNpm(t *testing.T) {
	bin, pin := rgaaResolveEngine(t)
	for _, tc := range []struct {
		name          string
		successOn     int
		plantUnpinned bool
		wantErr       bool
	}{
		{name: "two_failures_then_success", successOn: 3},
		{name: "persistent_failure_is_bounded_and_prints_no_unpinned_candidate", successOn: 0, plantUnpinned: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeBin := filepath.Join(dir, "bin")
			if err := os.Mkdir(fakeBin, 0o755); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "npm-calls")
			if err := os.WriteFile(calls, []byte("0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fakeNpm := `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = root ]; then exit 0; fi
if [ "${1:-}" != ci ]; then
  echo "unexpected npm command: $*" >&2
  exit 2
fi
read -r calls < "$RGAA_TEST_NPM_CALLS"
calls=$((calls + 1))
echo "$calls" > "$RGAA_TEST_NPM_CALLS"
if [ "$calls" -ne "$RGAA_TEST_NPM_SUCCESS_ON" ]; then
  echo "npm error code ECONNRESET" >&2
  exit 1
fi
mkdir -p node_modules/ultra11y/scripts
cp "$RGAA_TEST_NPM_ENGINE" node_modules/ultra11y/scripts/ultra11y.mjs
`
			if err := os.WriteFile(filepath.Join(fakeBin, "npm"), []byte(fakeNpm), 0o755); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(dir, "home")
			installed := filepath.Join(home, pin.Version, "node_modules", "ultra11y", "scripts", "ultra11y.mjs")
			if tc.plantUnpinned {
				if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(installed, []byte("not the pinned engine\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join("..", "scripts", "ultra11y-engine.sh"))
			cmd.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"ULTRA11Y_BIN=",
				"ULTRA11Y_HOME="+home,
				"RGAA_TEST_NPM_CALLS="+calls,
				"RGAA_TEST_NPM_ENGINE="+bin,
				"RGAA_TEST_NPM_SUCCESS_ON="+strconv.Itoa(tc.successOn),
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if tc.wantErr && err == nil {
				t.Fatalf("persistent npm failure succeeded: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("private install did not recover from two npm failures: %v\n%s", err, stderr.String())
			}
			if tc.wantErr && stdout.Len() != 0 {
				t.Fatalf("failed private install printed a candidate: %q", stdout.String())
			}
			raw, readErr := os.ReadFile(calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			got, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if convErr != nil {
				t.Fatal(convErr)
			}
			if got != 3 {
				t.Fatalf("npm ci ran %d times, want exactly 3 attempts", got)
			}
			if !tc.wantErr {
				want, evalErr := filepath.EvalSymlinks(installed)
				if evalErr != nil {
					t.Fatal(evalErr)
				}
				if stdout.String() != want+"\n" {
					t.Fatalf("resolver printed %q, want the installed pinned engine %q", stdout.String(), want)
				}
			}
		})
	}
}

// engineRun runs one engine command with a bounded timeout and returns the
// exit code, stdout and stderr. The exit code comes from the process, never
// from a pipe.
func rgaaEngineRun(t *testing.T, bin, dir string, env []string, args ...string) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rgaaEngineTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", append([]string{bin}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "npm_config_update_notifier=false")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("ultra11y %s: %v (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("ultra11y %s: timed out after %s", strings.Join(args, " "), rgaaEngineTimeout)
	}
	return code, stdout.Bytes(), stderr.Bytes()
}

func rgaaReadJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func rgaaWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rgaaCopyFixtureSite(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "site")
	if err := os.CopyFS(dst, os.DirFS(rgaaAuditFixture)); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

// The shapes the bot reads. Fields the bot ignores are left out on purpose.
type packAuditDoc struct {
	Kind           string            `json:"kind"`
	Standard       string            `json:"standard"`
	SchemaVersion  int               `json:"schemaVersion"`
	ConformancePct float64           `json:"conformancePct"`
	Findings       []packFinding     `json:"findings"`
	ResidualRisks  []json.RawMessage `json:"residualRisks"`
	Criteria       []packCriterion   `json:"criteria"`
	Core           struct {
		ConformancePct float64 `json:"conformancePct"`
		Scope          struct {
			Files int `json:"files"`
		} `json:"scope"`
	} `json:"core"`
}

type packFinding struct {
	CriteriaID string `json:"criteriaId"`
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	RuleID     string `json:"ruleId"`
}

type packCriterion struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	DecidedBy    *string `json:"decidedBy"`
	Inapplicable *bool   `json:"inapplicable"`
}

type worklistOut struct {
	Count        int    `json:"count"`
	TodoPath     string `json:"todoPath"`
	ItemsDir     string `json:"itemsDir"`
	VerdictsPath string `json:"verdictsPath"`
	MdPath       string `json:"mdPath"`
	Items        []struct {
		CriteriaID     string `json:"criteriaId"`
		Automatability string `json:"automatability"`
	} `json:"items"`
}

type foldOut struct {
	OK               bool     `json:"ok"`
	Applied          int      `json:"applied"`
	StillManual      int      `json:"stillManual"`
	Rejected         int      `json:"rejected"`
	RejectedCriteria []string `json:"rejectedCriteria"`
	Issues           []any    `json:"issues"`
	ConformancePct   float64  `json:"conformancePct"`
}

type checkOut struct {
	OK      bool  `json:"ok"`
	Issues  []any `json:"issues"`
	Decided *struct {
		OK         bool     `json:"ok"`
		Total      int      `json:"total"`
		Undecided  []string `json:"undecided"`
		Provenance struct {
			Total     int `json:"total"`
			Engine    int `json:"engine"`
			Scan      int `json:"scan"`
			Agent     int `json:"agent"`
			Declared  int `json:"declared"`
			Undecided int `json:"undecided"`
		} `json:"provenance"`
	} `json:"decided"`
}

func rgaaStatusCounts(doc packAuditDoc) map[string]int {
	counts := map[string]int{}
	for _, c := range doc.Criteria {
		counts[c.Status]++
	}
	return counts
}

// rgaaFlagged lists the criteria the engine wrote back as `inapplicable`
// while still `manual` — the shape behind the report double count.
func rgaaFlagged(doc packAuditDoc) []string {
	var ids []string
	for _, c := range doc.Criteria {
		if c.Inapplicable != nil && *c.Inapplicable && c.Status == "manual" {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// rgaaDocCounts is what the report's « Décidés » line must say about a
// document: validated = C without `inapplicable`; faute de sujet = every
// criterion carrying `inapplicable`; à évaluer = manual.
type rgaaDocCounts struct {
	Decided, Total, Conformes, NC, FauteDeSujet, AEvaluer int
}

func rgaaCountDoc(doc packAuditDoc) rgaaDocCounts {
	var c rgaaDocCounts
	c.Total = len(doc.Criteria)
	for _, cr := range doc.Criteria {
		inap := cr.Inapplicable != nil && *cr.Inapplicable
		if inap {
			c.FauteDeSujet++
		}
		switch cr.Status {
		case "C":
			if !inap {
				c.Conformes++
			}
			c.Decided++
		case "NC":
			c.NC++
			c.Decided++
		case "manual":
			c.AEvaluer++
		default:
			c.Decided++
		}
	}
	return c
}

// rgaaRate is the RGAA conformance rate the French report prints: validated
// ÷ (validated + NC + à évaluer), NA excluded from both sides, rounded — in
// the engine's own order of operations, (v / a) * 100: `100 * v / a` rounds
// the other way on splits such as 23/40 (57.49999… against 57.5).
func rgaaRate(c rgaaDocCounts) int {
	den := c.Conformes + c.NC + c.AEvaluer
	if den == 0 {
		return -1
	}
	return int(math.Round((float64(c.Conformes) / float64(den)) * 100))
}

var (
	rgaaDecidedLine   = regexp.MustCompile(`(?m)^- \*\*Décidés\*\* : (\d+)/(\d+) — (-?\d+) conforme\(s\), (-?\d+) non conforme\(s\), (-?\d+) conforme\(s\) faute de sujet, (-?\d+) à évaluer`)
	rgaaRateLine      = regexp.MustCompile(`(?m)^- \*\*Taux de conformité RGAA[^*]*\*\* : (-?\d+)%`)
	rgaaAutoRateLine  = regexp.MustCompile(`(?m)^- \*\*Taux de réussite automatique[^*]*\*\* : (-?\d+)%`)
	rgaaReportHeading = "Taux de conformité"
)

type rgaaReportLines struct {
	Decided rgaaDocCounts
	Rate    int
	Auto    int
}

func rgaaReadReportLines(t *testing.T, path string) rgaaReportLines {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	link := regexp.MustCompile(`\]\((annexe-technique-[A-Za-z0-9._-]+\.md)\)`).FindStringSubmatch(body)
	if len(link) != 2 {
		t.Fatalf("business report %s has no technical-annex link", path)
	}
	annex, err := os.ReadFile(filepath.Join(filepath.Dir(path), link[1]))
	if err != nil {
		t.Fatal(err)
	}
	body += "\n" + string(annex)
	m := rgaaDecidedLine.FindStringSubmatch(body)
	r := rgaaRateLine.FindStringSubmatch(body)
	a := rgaaAutoRateLine.FindStringSubmatch(body)
	if m == nil || r == nil || a == nil {
		t.Fatalf("report %s lacks the Décidés / taux lines this test reads (decided=%v rate=%v auto=%v)", path, m != nil, r != nil, a != nil)
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	return rgaaReportLines{
		Decided: rgaaDocCounts{Decided: atoi(m[1]), Total: atoi(m[2]), Conformes: atoi(m[3]), NC: atoi(m[4]), FauteDeSujet: atoi(m[5]), AEvaluer: atoi(m[6])},
		Rate:    atoi(r[1]),
		Auto:    atoi(a[1]),
	}
}

// rgaaEngineJSON runs a `--json` command and requires parseable JSON on
// stdout: a crash (exit 1, nothing printed) must never read as a refusal.
func rgaaEngineJSON(t *testing.T, bin, dir string, env []string, into any, args ...string) int {
	t.Helper()
	code, stdout, stderr := rgaaEngineRun(t, bin, dir, env, args...)
	if !json.Valid(stdout) {
		t.Fatalf("ultra11y %s printed no JSON (exit %d)\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
	}
	if into != nil {
		if err := json.Unmarshal(stdout, into); err != nil {
			t.Fatalf("ultra11y %s --json: %v\n%s", strings.Join(args, " "), err, stdout)
		}
	}
	return code
}

func rgaaReportPath(t *testing.T, bin, dir, audit, out string, std []string) string {
	t.Helper()
	var rep struct {
		Path string `json:"path"`
	}
	if code := rgaaEngineJSON(t, bin, dir, nil, &rep, append([]string{"report", "--in", audit, "--out", out, "--json"}, std...)...); code != 0 || rep.Path == "" {
		t.Fatalf("report exit %d, path %q", code, rep.Path)
	}
	return rep.Path
}

// The subtests below are sequential and share one run directory: run the
// whole test, not one subtest.
func TestRgaaAuditEngineContract(t *testing.T) {
	bin, _ := rgaaResolveEngine(t)
	site := rgaaCopyFixtureSite(t)
	run := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(run, "audit-latest.json")
	rawAudit := filepath.Join(run, "audit-raw.json")
	std := []string{"--standard", "rgaa", "--lang", "fr"}
	fold := func(t *testing.T, adj, in, out string, extra ...string) foldOut {
		t.Helper()
		var f foldOut
		rgaaEngineJSON(t, bin, site, nil, &f, append(append([]string{"verify", "--apply", adj, "--in", in, "--out", out}, extra...), append([]string{"--json"}, std...)...)...)
		return f
	}
	probe := func(t *testing.T, name string) (dir, copyPath string) {
		t.Helper()
		dir = filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		copyPath = filepath.Join(dir, "audit-latest.json")
		rgaaCopyFile(t, rawAudit, copyPath)
		return dir, copyPath
	}

	var doc packAuditDoc
	t.Run("audit_writes_a_pack_document_with_the_planted_defects", func(t *testing.T) {
		code, _, stderr := rgaaEngineRun(t, bin, site, nil, append([]string{"audit", ".", "--graph", "--jsx", "--out", run, "--json"}, std...)...)
		if code != 0 {
			t.Fatalf("audit exit %d: %s", code, stderr)
		}
		rgaaReadJSON(t, audit, &doc)
		if doc.Kind != "pack-audit" || doc.Standard != "rgaa" || doc.SchemaVersion != 2 {
			t.Fatalf("unexpected document header: kind=%q standard=%q schemaVersion=%d", doc.Kind, doc.Standard, doc.SchemaVersion)
		}
		if doc.Core.Scope.Files != 3 {
			t.Fatalf("core.scope.files = %d, fixture has 3 source files", doc.Core.Scope.Files)
		}
		found := map[string]bool{}
		for _, f := range doc.Findings {
			found[f.CriteriaID] = true
			if f.File == "" || f.Line == 0 || f.RuleID == "" || f.Severity == "" {
				t.Fatalf("finding without anchor/rule/severity: %+v", f)
			}
		}
		// The planted defects: iframe without title (2.1), empty link (6.2),
		// unlabelled field (11.1), empty button (11.9).
		for _, id := range []string{"2.1", "6.2", "11.1", "11.9"} {
			if !found[id] {
				t.Fatalf("planted defect on criterion %s not reported; findings on %v", id, rgaaSortedKeys(found))
			}
		}
		counts := rgaaStatusCounts(doc)
		if counts["NC"] < 4 || counts["manual"] < 90 || len(doc.Criteria) != 106 {
			t.Fatalf("criteria statuses %v over %d criteria — expected ≥4 NC and ≥90 manual on 106", counts, len(doc.Criteria))
		}
		if len(doc.ResidualRisks) != counts["manual"] {
			t.Fatalf("residualRisks (%d) ≠ manual criteria (%d)", len(doc.ResidualRisks), counts["manual"])
		}
		// The ROOT conformancePct is the AUTOMATIC pass rate — engine-decided C
		// over engine-decided C + NC — not the RGAA conformance rate, which no
		// JSON output carries (the report prints it; see the report subtests).
		auto := int(math.Round(100 * float64(counts["C"]) / float64(counts["C"]+counts["NC"])))
		if int(doc.ConformancePct) != auto {
			t.Fatalf("root conformancePct %.0f ≠ automatic pass rate %d (C %d ÷ C+NC %d)", doc.ConformancePct, auto, counts["C"], counts["C"]+counts["NC"])
		}
		rgaaCopyFile(t, audit, rawAudit)
	})
	if t.Failed() {
		return
	}

	var worklist worklistOut
	t.Run("verify_manual_writes_the_worklist_and_one_brief_per_criterion", func(t *testing.T) {
		code := rgaaEngineJSON(t, bin, site, nil, &worklist, append([]string{"verify", "--manual", "--in", audit, "--out", run, "--no-web", "--json"}, std...)...)
		if code != 0 {
			t.Fatalf("verify --manual exit %d", code)
		}
		manual := rgaaStatusCounts(doc)["manual"]
		if worklist.Count != manual || len(worklist.Items) != manual {
			t.Fatalf("worklist count %d / items %d, audit has %d manual criteria", worklist.Count, len(worklist.Items), manual)
		}
		for _, p := range []string{worklist.TodoPath, worklist.VerdictsPath, worklist.MdPath} {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("worklist file missing: %v", err)
			}
		}
		for _, it := range worklist.Items {
			if _, err := os.Stat(filepath.Join(worklist.ItemsDir, it.CriteriaID+".md")); err != nil {
				t.Fatalf("no brief for criterion %s in %s", it.CriteriaID, worklist.ItemsDir)
			}
		}
		var skeleton struct {
			Kind     string         `json:"kind"`
			Contract map[string]any `json:"contract"`
			Items    []map[string]any
		}
		rgaaReadJSON(t, worklist.VerdictsPath, &skeleton)
		if skeleton.Kind != "adjudication" || len(skeleton.Contract) == 0 || len(skeleton.Items) != worklist.Count {
			t.Fatalf("verdicts skeleton: kind=%q contract keys=%d items=%d", skeleton.Kind, len(skeleton.Contract), len(skeleton.Items))
		}
	})
	if t.Failed() {
		return
	}

	// Adjudication files start from the engine's skeleton (verdicts-only; the
	// fold re-hydrates evidence from the audit). The evidence an item was
	// shown lives in the full worklist (todoPath).
	var skeleton, todo map[string]any
	rgaaReadJSON(t, worklist.VerdictsPath, &skeleton)
	rgaaReadJSON(t, worklist.TodoPath, &todo)
	items := todo["items"].([]any)
	evidenceOf := map[string][]any{}
	for _, raw := range items {
		it := raw.(map[string]any)
		ev, _ := it["evidence"].([]any)
		evidenceOf[it["criteriaId"].(string)] = ev
	}
	var targetID string
	for _, raw := range items {
		it := raw.(map[string]any)
		if len(evidenceOf[it["criteriaId"].(string)]) > 0 {
			targetID = it["criteriaId"].(string)
			break
		}
	}
	if targetID == "" {
		t.Fatal("no worklist item carries evidence — the fixture must show the engine something")
	}
	citationFor := func(id string) map[string]any {
		first := evidenceOf[id][0].(map[string]any)
		return map[string]any{"file": first["file"], "line": first["line"], "snippet": first["snippet"]}
	}
	adjudication := func(verdicts ...map[string]any) map[string]any {
		adj := map[string]any{}
		for k, v := range skeleton {
			adj[k] = v
		}
		list := make([]any, 0, len(verdicts))
		for _, v := range verdicts {
			list = append(list, v)
		}
		adj["items"] = list
		return adj
	}
	naVerdict := func(id string) map[string]any {
		return map[string]any{"criteriaId": id, "verdict": "NA", "justification": "Aucun sujet de ce type dans le périmètre audité (fixture)."}
	}
	goodC := map[string]any{"criteriaId": targetID, "verdict": "C", "justification": "Conforme sur l'évidence citée (fixture).", "citations": []any{citationFor(targetID)}}
	badC := map[string]any{"criteriaId": targetID, "verdict": "C", "justification": "Conforme (fixture, sans citation)."}
	writeAdj := func(t *testing.T, dir string, adj map[string]any) string {
		t.Helper()
		p := filepath.Join(dir, "adjudication.json")
		rgaaWriteJSON(t, p, adj)
		return p
	}

	t.Run("fold_refuses_a_C_without_citations_and_lands_nothing", func(t *testing.T) {
		dir, copyPath := probe(t, "badc")
		f := fold(t, writeAdj(t, dir, adjudication(badC)), copyPath, dir)
		// Nothing landed, so the partial fold reports ok:false — the "exit 1
		// only when nothing lands" rule (the exit code is checked below).
		if f.OK || f.Applied != 0 || !rgaaContains(f.RejectedCriteria, targetID) || !strings.Contains(fmt.Sprint(f.Issues), "cit") {
			t.Fatalf("a C without citations must be refused by name, naming the citations: %+v", f)
		}
		code, _, _ := rgaaEngineRun(t, bin, site, nil, append([]string{"verify", "--apply", filepath.Join(dir, "adjudication.json"), "--in", copyPath, "--out", dir, "--json"}, std...)...)
		if code != 1 {
			t.Fatalf("exit %d when nothing lands, expected 1", code)
		}
	})

	t.Run("partial_fold_lands_a_cited_C_and_refuses_the_coverage_gaps", func(t *testing.T) {
		ledger := filepath.Join(run, "ledger.json")
		f := fold(t, writeAdj(t, run, adjudication(goodC)), audit, run, "--ledger", ledger)
		if !f.OK || f.Applied != 1 || f.StillManual != 0 {
			t.Fatalf("applied=%d stillManual=%d ok=%v — exactly the cited C must land", f.Applied, f.StillManual, f.OK)
		}
		// Every other open criterion is absent from the file: the engine
		// refuses each as a coverage gap and leaves it "à évaluer".
		if f.Rejected != worklist.Count-1 || len(f.RejectedCriteria) != worklist.Count-1 {
			t.Fatalf("rejected=%d (%d ids), expected %d coverage gaps", f.Rejected, len(f.RejectedCriteria), worklist.Count-1)
		}
		rgaaReadJSON(t, audit, &doc)
		if got := rgaaStatusCounts(doc)["manual"]; got != worklist.Count-1 {
			t.Fatalf("after the fold %d criteria are still manual, expected %d", got, worklist.Count-1)
		}
		var ledgerDoc struct {
			Kind    string `json:"kind"`
			Entries []struct {
				CriteriaID string `json:"criteriaId"`
				DecidedBy  string `json:"decidedBy"`
			} `json:"entries"`
		}
		rgaaReadJSON(t, ledger, &ledgerDoc)
		if ledgerDoc.Kind != "verdict-ledger" || len(ledgerDoc.Entries) != 1 || ledgerDoc.Entries[0].CriteriaID != targetID || ledgerDoc.Entries[0].DecidedBy != "agent" {
			t.Fatalf("ledger must record exactly the accepted verdict: %+v", ledgerDoc)
		}
		// The fold JSON echoes the core (WCAG) rate — neither the automatic
		// pass rate at the document root nor the RGAA rate.
		if int(f.ConformancePct) != int(doc.Core.ConformancePct) {
			t.Fatalf("fold conformancePct %.0f ≠ core %.0f", f.ConformancePct, doc.Core.ConformancePct)
		}
		if flagged := rgaaFlagged(doc); len(flagged) != 0 {
			t.Fatalf("coverage gaps incorrectly marked inapplicable: %v", flagged)
		}
	})
	if t.Failed() {
		return
	}

	t.Run("strict_fold_refuses_any_coverage_gap_and_leaves_the_audit_untouched", func(t *testing.T) {
		dir, copyPath := probe(t, "strict")
		before, _ := os.ReadFile(copyPath)
		var f foldOut
		code := rgaaEngineJSON(t, bin, site, nil, &f, append([]string{"verify", "--apply", writeAdj(t, dir, adjudication(goodC)), "--in", copyPath, "--out", dir, "--strict", "--json"}, std...)...)
		if code != 1 || f.OK || f.Applied != 0 {
			t.Fatalf("--strict with %d criteria absent: exit %d ok=%v applied=%d", worklist.Count-1, code, f.OK, f.Applied)
		}
		for _, is := range f.Issues {
			if !strings.Contains(fmt.Sprint(is), "coverage gap") {
				t.Fatalf("a --strict issue is not a coverage gap: %v", is)
			}
		}
		after, _ := os.ReadFile(copyPath)
		if !bytes.Equal(before, after) {
			t.Fatal("--strict rewrote the audit although it refused the adjudication")
		}
		// Positive control: a complete adjudication folds under --strict.
		var complete []map[string]any
		for _, raw := range items {
			id := raw.(map[string]any)["criteriaId"].(string)
			if len(evidenceOf[id]) > 0 {
				complete = append(complete, map[string]any{"criteriaId": id, "verdict": "C", "justification": "Conforme sur l'évidence citée (fixture).", "citations": []any{citationFor(id)}})
			} else {
				complete = append(complete, naVerdict(id))
			}
		}
		dir2, copy2 := probe(t, "strict-complete")
		code = rgaaEngineJSON(t, bin, site, nil, &f, append([]string{"verify", "--apply", writeAdj(t, dir2, adjudication(complete...)), "--in", copy2, "--out", dir2, "--strict", "--json"}, std...)...)
		if code != 0 || !f.OK || f.Applied != worklist.Count || f.Rejected != 0 {
			t.Fatalf("a complete adjudication under --strict: exit %d ok=%v applied=%d rejected=%d (want 0/true/%d/0)", code, f.OK, f.Applied, f.Rejected, worklist.Count)
		}
	})

	t.Run("refolding_a_decided_criterion_returns_a_structured_refusal", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "refold")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		folded := filepath.Join(dir, "audit-latest.json")
		rgaaCopyFile(t, audit, folded)
		before, _ := os.ReadFile(folded)
		var f foldOut
		code := rgaaEngineJSON(t, bin, site, nil, &f, append([]string{"verify", "--apply", writeAdj(t, dir, adjudication(goodC)), "--in", folded, "--out", dir, "--strict", "--json"}, std...)...)
		if code == 0 || f.OK || f.Applied != 0 || !strings.Contains(fmt.Sprint(f.Issues), "criterion "+goodC["criteriaId"].(string)+": not open for adjudication") {
			t.Fatalf("re-fold must refuse the already decided criterion with JSON: exit %d, %+v", code, f)
		}
		after, _ := os.ReadFile(folded)
		if !bytes.Equal(before, after) {
			t.Fatal("refused strict re-fold changed the audit")
		}
	})

	t.Run("report_and_check_accept_the_raw_audit", func(t *testing.T) {
		dir, copyPath := probe(t, "raw")
		var rep struct {
			Path           string  `json:"path"`
			ConformancePct float64 `json:"conformancePct"`
			Date           string  `json:"date"`
		}
		if code := rgaaEngineJSON(t, bin, site, nil, &rep, append([]string{"report", "--in", copyPath, "--out", dir, "--json"}, std...)...); code != 0 {
			t.Fatalf("report exit %d", code)
		}
		if !strings.HasSuffix(rep.Path, "rgaa-"+rep.Date+".md") {
			t.Fatalf("report path %q does not carry the standard and the audit date %q", rep.Path, rep.Date)
		}
		body, err := os.ReadFile(rep.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "RGAA") || !strings.Contains(string(body), rgaaReportHeading) {
			t.Fatalf("report %s does not read as a French RGAA report", rep.Path)
		}
		var raw packAuditDoc
		rgaaReadJSON(t, copyPath, &raw)
		lines := rgaaReadReportLines(t, rep.Path)
		// Three different numbers: `report --json` echoes the core (WCAG)
		// rate; the document root is the automatic pass rate, printed as
		// « Taux de réussite automatique »; the RGAA rate is the « Taux de
		// conformité » line, computed from criteria[] and in no JSON at all.
		if int(rep.ConformancePct) != int(raw.Core.ConformancePct) || lines.Auto != int(raw.ConformancePct) {
			t.Fatalf("report --json %.0f / core %.0f / automatic line %d / root %.0f disagree", rep.ConformancePct, raw.Core.ConformancePct, lines.Auto, raw.ConformancePct)
		}
		counts := rgaaCountDoc(raw)
		if lines.Rate != rgaaRate(counts) {
			t.Fatalf("report RGAA rate %d%% ≠ %d%% computed from criteria[] (%+v)", lines.Rate, rgaaRate(counts), counts)
		}
		if lines.Decided != counts {
			t.Fatalf("report Décidés line %+v ≠ document %+v", lines.Decided, counts)
		}
		var chk checkOut
		code := rgaaEngineJSON(t, bin, site, nil, &chk, append([]string{"check", "--report", rep.Path, "--in", copyPath, "--json"}, std[:2]...)...)
		if code != 0 || !chk.OK {
			t.Fatalf("check --report refused the engine's own report (exit %d): %v", code, chk.Issues)
		}
	})

	t.Run("partial_and_manual_folds_keep_report_counts_consistent", func(t *testing.T) {
		for _, manual := range []bool{false, true} {
			dir, copyPath := probe(t, fmt.Sprintf("consistent-%v", manual))
			verdicts := []map[string]any{goodC}
			if manual {
				verdicts = []map[string]any{{"criteriaId": targetID, "verdict": "manual", "reason": "undecidable", "justification": "fixture"}}
			}
			f := fold(t, writeAdj(t, dir, adjudication(verdicts...)), copyPath, dir)
			if manual && f.StillManual != 1 {
				t.Fatalf("manual fold: %+v", f)
			}
			var after packAuditDoc
			rgaaReadJSON(t, copyPath, &after)
			rep := rgaaReportPath(t, bin, site, copyPath, dir, std)
			lines, counts := rgaaReadReportLines(t, rep), rgaaCountDoc(after)
			if lines.Decided != counts || lines.Rate != rgaaRate(counts) || len(rgaaFlagged(after)) != 0 {
				t.Fatalf("partial/manual fold report disagrees: %+v / %+v", lines, counts)
			}
			var chk checkOut
			code := rgaaEngineJSON(t, bin, site, nil, &chk, append([]string{"check", "--report", rep, "--in", copyPath, "--json"}, std[:2]...)...)
			if code != 0 || !chk.OK {
				t.Fatalf("check --report refused a consistent partial/manual fold: %d %+v", code, chk)
			}
		}
	})

	t.Run("the_C_gate_grounds_citations_on_the_source_not_on_the_shown_evidence", func(t *testing.T) {
		// The engine resolves a citation against the SOURCE (file exists,
		// snippet matches when given) — not against the evidence list the
		// item was shown. A citation of an unrelated line is accepted, so the
		// bot carries its own check: citations ⊆ the item's evidence anchors.
		dir, copyPath := probe(t, "grounding")
		stray := map[string]any{"criteriaId": targetID, "verdict": "C", "justification": "Conforme (citation hors évidence, fixture).", "citations": []any{map[string]any{"file": "contact.html", "line": 2}}}
		f := fold(t, writeAdj(t, dir, adjudication(stray)), copyPath, dir)
		if f.Applied != 1 || rgaaContains(f.RejectedCriteria, targetID) {
			t.Fatalf("the engine now refuses a citation outside the shown evidence (%+v) — the bot-side relevance check can be dropped; update this test", f)
		}
	})

	t.Run("check_require_decided_nests_the_undecided_list", func(t *testing.T) {
		var chk checkOut
		code := rgaaEngineJSON(t, bin, site, nil, &chk, append([]string{"check", "--in", audit, "--require-decided=pages", "--json"}, std[:2]...)...)
		if code != 1 || chk.OK || chk.Decided == nil {
			t.Fatalf("with %d undecided criteria check must exit 1 with a decided block (exit %d, ok %v)", worklist.Count-1, code, chk.OK)
		}
		p := chk.Decided.Provenance
		if len(chk.Decided.Undecided) != worklist.Count-1 || p.Undecided != worklist.Count-1 || p.Agent != 1 || p.Total != 106 {
			t.Fatalf("decided block: undecided=%d provenance=%+v, expected %d undecided and 1 agent verdict", len(chk.Decided.Undecided), p, worklist.Count-1)
		}
	})

	t.Run("tickets_writes_one_issue_per_criterion_and_makes_one_provider_call", func(t *testing.T) {
		// `tickets` resolves a provider even for `--out --dry-run` and its
		// list() makes exactly one request, which degrades on failure; the
		// file is written anyway. A local listener pins both facts.
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer srv.Close()
		env := []string{"GITHUB_API_URL=" + srv.URL, "GH_TOKEN=", "GITHUB_TOKEN="}
		code, _, stderr := rgaaEngineRun(t, bin, site, env, append([]string{"tickets", "--in", audit, "--out", run, "--grain", "criterion", "--provider", "github", "--transport", "rest", "--dry-run", "--max-tickets", "50"}, std...)...)
		if code != 0 {
			t.Fatalf("tickets exit %d: %s", code, stderr)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Fatalf("tickets made %d provider requests, expected exactly one list() attempt", n)
		}
		matches, _ := filepath.Glob(filepath.Join(run, "issues-*.json"))
		if len(matches) != 1 {
			t.Fatalf("expected one issues-<date>.json, got %v", matches)
		}
		var issues struct {
			SchemaVersion int    `json:"schemaVersion"`
			Kind          string `json:"kind"`
			Grain         string `json:"grain"`
			Count         int    `json:"count"`
			Issues        []struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
				Scope  struct {
					Grain      string `json:"grain"`
					CriteriaID string `json:"criteriaId"`
				} `json:"scope"`
			} `json:"issues"`
		}
		rgaaReadJSON(t, matches[0], &issues)
		if issues.SchemaVersion != 1 || issues.Kind != "issues" || issues.Grain != "criterion" || issues.Count != len(issues.Issues) {
			t.Fatalf("issues file header: schemaVersion=%d kind=%q grain=%q count=%d", issues.SchemaVersion, issues.Kind, issues.Grain, issues.Count)
		}
		titles := map[string]bool{}
		byCriterion := map[string]bool{}
		for _, is := range issues.Issues {
			if !strings.HasPrefix(is.Title, "[a11y] RGAA ") || is.Scope.CriteriaID == "" || is.Body == "" || len(is.Labels) == 0 {
				t.Fatalf("issue shape: %+v", is)
			}
			if titles[is.Title] {
				t.Fatalf("duplicate title %q — the title is the de-dup key", is.Title)
			}
			titles[is.Title] = true
			byCriterion[is.Scope.CriteriaID] = true
		}
		for _, id := range []string{"2.1", "6.2", "11.1", "11.9"} {
			if !byCriterion[id] {
				t.Fatalf("no ticket for the planted defect on %s; tickets on %v", id, rgaaSortedKeys(byCriterion))
			}
		}
	})

	t.Run("status_sample_and_pages_without_a_browser", func(t *testing.T) {
		// `status` reads harness configs under $HOME and resolves Playwright
		// from --cwd first: a private HOME keeps the operator's dotfiles out,
		// and --cwd points at the engine's own package, never the audited tree.
		hermetic := []string{"HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir()}
		enginePkg := filepath.Dir(filepath.Dir(bin))
		var st struct {
			Version string `json:"version"`
			Browser *struct {
				OK bool `json:"ok"`
			} `json:"browser"`
		}
		if code := rgaaEngineJSON(t, bin, site, hermetic, &st, "status", "--browser", "--cwd", enginePkg, "--json"); code != 0 || st.Browser == nil || st.Version == "" {
			t.Fatalf("status --browser --json: exit %d, %+v", code, st)
		}
		var sample struct {
			OK      bool `json:"ok"`
			Missing []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"missing"`
		}
		if code := rgaaEngineJSON(t, bin, site, nil, &sample, "sample", "check", "--standard", "rgaa", "--json"); code != 0 || len(sample.Missing) == 0 || sample.Missing[0].ID == "" {
			t.Fatalf("sample check --json: exit %d, %+v", code, sample)
		}
		// No snapshot on disk: `pages` refuses rather than inventing a grid.
		code, _, stderr := rgaaEngineRun(t, bin, site, nil, append([]string{"pages", "--in", audit, "--json", "--out", run}, std...)...)
		if code != 1 || !strings.Contains(string(stderr), "aucune page") {
			t.Fatalf("pages without snapshots: exit %d, stderr %s", code, stderr)
		}
	})
}

func rgaaCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func rgaaContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func rgaaSortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
