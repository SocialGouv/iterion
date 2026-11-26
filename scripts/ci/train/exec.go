package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/git"
)

// gitRunner runs git with no repository hook and no inherited redirection
// variable (GIT_DIR and its family would point every gesture at another
// repository).
type gitRunner struct{}

func (g *gitRunner) runCode(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", git.NoRunHooks(args...)...)
	cmd.Dir = dir
	cmd.Env = git.SanitizeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), nil
	}
	if err != nil {
		return string(out), -1, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), 0, nil
}

func (g *gitRunner) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, code, err := g.runCode(ctx, dir, args...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(out))
	}
	return out, nil
}

func (g *gitRunner) lines(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := g.run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res, nil
}

func (g *gitRunner) revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// secretSuffixes and secretNames are the environment variables kept out of
// every command that runs pull request code (the gate, its probes, the
// generators, the setup). This is a scrub of the obvious, not containment:
// the code still runs as the operator and can read what the operator's files
// hold — see checkLot's trust boundary.
var (
	secretSuffixes = []string{"_TOKEN", "_API_KEY", "_APIKEY", "_SECRET", "_SECRET_KEY", "_SECRET_ACCESS_KEY", "_PASSWORD", "_PAT", "_CREDENTIALS"}
	secretNames    = []string{"SSH_AUTH_SOCK", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "NPM_TOKEN", "NODE_AUTH_TOKEN"}
)

// prCodeEnv is os.Environ() without the secrets above, with gh pointed at an
// empty config directory so a `gh` the pull request's code runs finds no
// login.
func prCodeEnv(emptyDir string, extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		if slices.Contains(secretNames, up) || up == "GH_CONFIG_DIR" {
			continue
		}
		drop := false
		for _, suf := range secretSuffixes {
			if strings.HasSuffix(up, suf) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(append(out, "GH_CONFIG_DIR="+emptyDir), extra...)
}

// GateResult is a full gate's verdict: its exit code and the members its
// matrix recorded red or refused.
type GateResult struct {
	Code    int
	Red     []string
	Refused []string
	// Incomplete is gate.sh's "a required or staged context has no member":
	// its exit 3, which a red member's exit 1 takes precedence over.
	Incomplete bool
}

// Gate runs the local gate the direct-landing lane defines.
type Gate interface {
	// Full runs every member on dir's checkout, evidence under ev.
	Full(ctx context.Context, dir, ev string) (GateResult, error)
	// Probe runs only members on dir's checkout and returns the red ones. A
	// member that refuses (its declared precondition is unmet) is an error:
	// it says nothing about the tree.
	Probe(ctx context.Context, dir, ev string, members []string) ([]string, error)
}

// scriptGate is scripts/ci/gate.sh for the full gate and `task -x <member>`
// for a probe — the very command gate.sh runs per member, so a probe and the
// gate cannot disagree on what a member is.
type scriptGate struct {
	Log io.Writer
	// EmptyDir is handed to the gate as GH_CONFIG_DIR (see prCodeEnv).
	EmptyDir string
}

func (g scriptGate) Full(ctx context.Context, dir, ev string) (GateResult, error) {
	if err := os.MkdirAll(ev, 0o755); err != nil {
		return GateResult{}, err
	}
	logPath := filepath.Join(ev, "gate.log")
	logf, err := os.Create(logPath)
	if err != nil {
		return GateResult{}, err
	}
	defer logf.Close()
	cmd := exec.CommandContext(ctx, "bash", "scripts/ci/gate.sh")
	cmd.Dir = dir
	cmd.Env = prCodeEnv(g.EmptyDir, "ITERION_CI_GATE_EVIDENCE="+ev)
	cmd.Stdout, cmd.Stderr = logf, logf
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return GateResult{}, fmt.Errorf("gate.sh: %w", err)
		}
		code = ee.ExitCode()
	}
	res := GateResult{Code: code}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		return res, err
	}
	res.Incomplete = strings.Contains(string(logBytes), "ci:gate: INCOMPLETE")
	refusing, err := refusingMembers(dir)
	if err != nil {
		return res, err
	}
	matrix, err := readMatrix(filepath.Join(ev, "matrix.tsv"))
	if err != nil {
		if code == 2 {
			// gate.sh refuses before writing a matrix when its own
			// precondition fails; the log says which.
			return res, nil
		}
		return res, err
	}
	for _, row := range matrix {
		switch {
		case row.rc == 0:
		case row.rc == 2 && slices.Contains(refusing, row.member):
			res.Refused = append(res.Refused, row.member)
		default:
			res.Red = append(res.Red, row.member)
		}
	}
	return res, nil
}

func (g scriptGate) Probe(ctx context.Context, dir, ev string, members []string) ([]string, error) {
	if err := os.MkdirAll(ev, 0o755); err != nil {
		return nil, err
	}
	refusing, err := refusingMembers(dir)
	if err != nil {
		return nil, err
	}
	var red []string
	for _, m := range members {
		logPath := filepath.Join(ev, strings.NewReplacer(":", "_", "/", "_").Replace(m)+".log")
		logf, err := os.Create(logPath)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "task", "-x", m)
		cmd.Dir = dir
		cmd.Env = prCodeEnv(g.EmptyDir)
		cmd.Stdout, cmd.Stderr = logf, logf
		runErr := cmd.Run()
		logf.Close()
		rc := 0
		if runErr != nil {
			var ee *exec.ExitError
			if !errors.As(runErr, &ee) {
				return nil, fmt.Errorf("task -x %s: %w", m, runErr)
			}
			rc = ee.ExitCode()
		}
		fmt.Fprintf(g.Log, "ci:train:   task -x %s → %d (%s)\n", m, rc, logPath)
		switch {
		case rc == 0:
		case rc == 2 && slices.Contains(refusing, m):
			return nil, fmt.Errorf("member %s refused to run (exit 2, its declared precondition): no verdict — %s", m, logPath)
		default:
			red = append(red, m)
		}
	}
	return red, nil
}

type matrixRow struct {
	member string
	rc     int
}

func readMatrix(path string) ([]matrixRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []matrixRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s: malformed row %q", path, sc.Text())
		}
		rc, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%s: malformed exit code in %q", path, sc.Text())
		}
		rows = append(rows, matrixRow{member: parts[0], rc: rc})
	}
	return rows, sc.Err()
}

// refusingMembers reads the gate map's refusing_members: the members whose
// exit 2 means "a precondition is unmet", not "red".
func refusingMembers(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".github", "ci-gate-map.yaml"))
	if err != nil {
		return nil, err
	}
	var m struct {
		Refusing []string `yaml:"refusing_members"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf(".github/ci-gate-map.yaml: %w", err)
	}
	return m.Refusing, nil
}

// derivedExact and derivedPrefixSuffix name the files a generator writes
// from the tree: a conflict on one is resolved by regenerating it, never by
// picking a side.
var (
	derivedExact        = []string{"internal/mdcode/testdata/committed-reading.txt"}
	derivedPrefixSuffix = [][2]string{{"docs/references/map-", ".md"}}
)

func isDerived(path string) bool {
	if slices.Contains(derivedExact, path) {
		return true
	}
	for _, ps := range derivedPrefixSuffix {
		if strings.HasPrefix(path, ps[0]) && strings.HasSuffix(path, ps[1]) && !strings.Contains(path[len(ps[0]):], "/") {
			return true
		}
	}
	return false
}

// Regenerator rewrites the derived files of a checkout from its sources.
type Regenerator interface {
	Regenerate(ctx context.Context, dir string) error
}

// commandRegen runs the generators of the derived files above, in order.
type commandRegen struct {
	Commands [][]string
	Log      io.Writer
	EmptyDir string
}

// defaultGenerators write every file isDerived accepts. Order matters: the
// repository maps are tracked .md files the committed-reading golden pins,
// so the maps are rewritten first and the golden last.
var defaultGenerators = [][]string{
	{"go", "run", "-mod=vendor", "./cmd/iterion", "map", "gen"},
	{"go", "run", "./cmd/corpus-reading", "--write"},
}

func (r commandRegen) Regenerate(ctx context.Context, dir string) error {
	for _, c := range r.Commands {
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		cmd.Dir = dir
		cmd.Env = prCodeEnv(r.EmptyDir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(c, " "), err, tail(string(out), 20))
		}
	}
	return nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
