package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v2"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// iterion schedule — cron for recurring bot runs (no always-on daemon)
//
// A declarative manifest (default ~/.iterion/schedules.yaml) is the single
// source of truth. `schedule install` materialises it into a managed block of
// the host crontab; each cron line calls `iterion schedule run <name>`, which
// re-reads the manifest and invokes the run in-process. Because the host's
// own cron is the trigger, no iterion process needs to stay resident — this is
// the "without an always-on dispatcher loop" path the dispatcher can't offer.
// ---------------------------------------------------------------------------

// ScheduleManifest is the declarative set of recurring runs. It is host-wide
// (a host has a single crontab); each entry carries its own Workdir so one
// manifest can schedule bots across several repositories.
type ScheduleManifest struct {
	Version   int             `yaml:"version"`
	Schedules []ScheduleEntry `yaml:"schedules"`
}

// ScheduleEntry is one recurring run. Cron is a standard 5-field expression
// passed opaquely to the host cron — iterion does not interpret it, the host
// scheduler does.
type ScheduleEntry struct {
	Name        string            `yaml:"name"`
	Cron        string            `yaml:"cron"`
	Bot         string            `yaml:"bot"`
	Workdir     string            `yaml:"workdir,omitempty"`
	StoreDir    string            `yaml:"store_dir,omitempty"`
	Sandbox     string            `yaml:"sandbox,omitempty"`
	Timeout     string            `yaml:"timeout,omitempty"`
	Vars        map[string]string `yaml:"vars,omitempty"`
	Description string            `yaml:"description,omitempty"`
	Disabled    bool              `yaml:"disabled,omitempty"`

	// Overlap policy + pre-launch guard (pkg/schedgate). Overlap
	// defaults to "skip": a tick whose previous run is still live is
	// skipped (and audited) instead of piling up concurrent runs.
	// Guard is an optional `sh -lc` snippet run before any launch —
	// exit 0 fires the run with its stdout in vars[guard_var],
	// non-zero skips the tick. See docs/scheduling.md#overlap-policy.
	// Overlap "keepalive" runs the bot always-on at the cron cadence (host
	// crontab floors at 1 minute — sub-minute keepalive needs the resident
	// scheduler): a stale run silent past StaleAfter is relaunched.
	Overlap       string `yaml:"overlap,omitempty"`
	MaxConcurrent int    `yaml:"max_concurrent,omitempty"`
	Guard         string `yaml:"guard,omitempty"`
	GuardTimeout  string `yaml:"guard_timeout,omitempty"`
	GuardVar      string `yaml:"guard_var,omitempty"`
	StaleAfter    string `yaml:"stale_after,omitempty"`

	// Retry policy (pkg/retrypolicy) for a run this schedule launches that
	// dies on an exhausted provider usage window. Declared flat like the
	// schedgate fields above; project with retryPolicy(). Only the fields
	// set here override the machine default.
	//
	// Note the surface difference: locally the wait is held in-process by
	// the bounded auto-resume loop, so it needs `--auto-resume N` (or
	// ITERION_AUTO_RESUME) to be on at all — these fields shape that wait
	// rather than enabling it. In cloud mode the wait is durable and needs
	// no such opt-in.
	RetryUsageWindow string `yaml:"retry_usage_window,omitempty"`
	RetryMaxAttempts int    `yaml:"retry_max_attempts,omitempty"`
	RetryMaxWait     string `yaml:"retry_max_wait,omitempty"`
	RetryJitter      string `yaml:"retry_jitter,omitempty"`
}

// policy projects the entry's schedgate fields into a normalized Policy.
func (e ScheduleEntry) policy() schedgate.Policy {
	return schedgate.Normalize(schedgate.Policy{
		Overlap:       e.Overlap,
		MaxConcurrent: e.MaxConcurrent,
		Guard:         e.Guard,
		GuardTimeout:  e.GuardTimeout,
		GuardVar:      e.GuardVar,
		StaleAfter:    e.StaleAfter,
	})
}

// retryPolicy projects the entry's retry fields. Not normalized — this is
// one layer of a precedence chain, and defaults filled here would
// masquerade as an explicit per-schedule choice.
func (e ScheduleEntry) retryPolicy() retrypolicy.Policy {
	return retrypolicy.Policy{
		UsageWindow: e.RetryUsageWindow,
		MaxAttempts: e.RetryMaxAttempts,
		MaxWait:     e.RetryMaxWait,
		Jitter:      e.RetryJitter,
	}
}

// ---------------------------------------------------------------------------
// Manifest path / load / save
// ---------------------------------------------------------------------------

// resolveScheduleManifestPath picks the manifest path: explicit override wins,
// then ITERION_SCHEDULES_FILE, then ~/.iterion/schedules.yaml.
func resolveScheduleManifestPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("ITERION_SCHEDULES_FILE"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for default schedules.yaml: %w", err)
	}
	return filepath.Join(home, ".iterion", "schedules.yaml"), nil
}

// loadScheduleManifest reads the manifest, treating a missing file as an empty
// (version 1) manifest so `add` works on first use.
func loadScheduleManifest(path string) (*ScheduleManifest, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ScheduleManifest{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m ScheduleManifest
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Version == 0 {
		m.Version = 1
	}
	return &m, nil
}

func saveScheduleManifest(path string, m *ScheduleManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (m *ScheduleManifest) find(name string) (*ScheduleEntry, int) {
	for i := range m.Schedules {
		if m.Schedules[i].Name == name {
			return &m.Schedules[i], i
		}
	}
	return nil, -1
}

// upsert replaces an entry with the same name or appends a new one. Returns
// true when a new entry was created.
func (m *ScheduleManifest) upsert(e ScheduleEntry) bool {
	if _, idx := m.find(e.Name); idx >= 0 {
		m.Schedules[idx] = e
		return false
	}
	m.Schedules = append(m.Schedules, e)
	return true
}

func (m *ScheduleManifest) remove(name string) bool {
	if _, idx := m.find(name); idx >= 0 {
		m.Schedules = append(m.Schedules[:idx], m.Schedules[idx+1:]...)
		return true
	}
	return false
}

// validateScheduleEntry guards the fields that end up in a crontab line and a
// log filename, where stray whitespace/slashes would break things.
func validateScheduleEntry(e ScheduleEntry) error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("schedule name is required (--name)")
	}
	if strings.ContainsAny(e.Name, " \t\n/\\") {
		return fmt.Errorf("schedule name %q must not contain whitespace or path separators", e.Name)
	}
	if strings.TrimSpace(e.Bot) == "" {
		return fmt.Errorf("--bot is required (path to a .bot workflow or .botz bundle)")
	}
	if err := validateCronExpr(e.Cron); err != nil {
		return err
	}
	if e.Timeout != "" {
		if _, err := time.ParseDuration(e.Timeout); err != nil {
			return fmt.Errorf("invalid timeout %q: %w", e.Timeout, err)
		}
	}
	// canReap=false: the host-cron tick drops GateOutcome.ReapRunIDs.
	if err := schedgate.ValidateReapable(schedgate.Policy{
		Overlap:       e.Overlap,
		MaxConcurrent: e.MaxConcurrent,
		Guard:         e.Guard,
		GuardTimeout:  e.GuardTimeout,
		GuardVar:      e.GuardVar,
		StaleAfter:    e.StaleAfter,
	}, false); err != nil {
		return err
	}
	return nil
}

// validateCronExpr does the cheap structural check (5 fields). Field-range
// validity is left to the host cron, which is the authority that runs it.
func validateCronExpr(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("--cron is required (5-field expression, e.g. \"0 2 * * 1\")")
	}
	if n := len(strings.Fields(expr)); n != 5 {
		return fmt.Errorf("cron expression %q must have 5 fields (min hour dom month dow), got %d", expr, n)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

type ScheduleCommonOptions struct {
	ManifestPath string // --manifest; "" → resolveScheduleManifestPath
}

type ScheduleAddOptions struct {
	ScheduleCommonOptions
	Name          string
	Cron          string
	Bot           string
	Workdir       string
	StoreDir      string
	Sandbox       string
	Timeout       string
	VarFlags      []string
	Description   string
	Disabled      bool
	Overlap       string
	MaxConcurrent int
	Guard         string
	GuardTimeout  string
	GuardVar      string
	StaleAfter    string
}

type ScheduleRefOptions struct {
	ScheduleCommonOptions
	Name string
}

type ScheduleRunOptions struct {
	ScheduleCommonOptions
	Name   string
	DryRun bool
}

type ScheduleInstallOptions struct {
	ScheduleCommonOptions
	Print bool   // render the managed block to stdout; don't touch the crontab
	TZ    string // crontab CRON_TZ; "" → UTC
}

// ---------------------------------------------------------------------------
// add / list / remove
// ---------------------------------------------------------------------------

func RunScheduleAdd(p *Printer, opts ScheduleAddOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}

	workdir := opts.Workdir
	if workdir == "" {
		if wd, err := os.Getwd(); err == nil {
			workdir = wd
		}
	}
	vars, err := parseScheduleVars(opts.VarFlags)
	if err != nil {
		return err
	}
	entry := ScheduleEntry{
		Name:          opts.Name,
		Cron:          opts.Cron,
		Bot:           opts.Bot,
		Workdir:       workdir,
		StoreDir:      opts.StoreDir,
		Sandbox:       opts.Sandbox,
		Timeout:       opts.Timeout,
		Vars:          vars,
		Description:   opts.Description,
		Disabled:      opts.Disabled,
		Overlap:       opts.Overlap,
		MaxConcurrent: opts.MaxConcurrent,
		Guard:         opts.Guard,
		GuardTimeout:  opts.GuardTimeout,
		GuardVar:      opts.GuardVar,
		StaleAfter:    opts.StaleAfter,
	}
	if err := validateScheduleEntry(entry); err != nil {
		return err
	}

	m, err := loadScheduleManifest(path)
	if err != nil {
		return err
	}
	created := m.upsert(entry)
	if err := saveScheduleManifest(path, m); err != nil {
		return err
	}

	verb := "updated"
	if created {
		verb = "added"
	}
	p.Line("✓ schedule %q %s (%s) → %s", entry.Name, verb, entry.Cron, path)
	p.Line("  run `iterion schedule install` to (re)sync the host crontab")
	return nil
}

func parseScheduleVars(flags []string) (map[string]string, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	// Historical behavior: store the raw key (no trim), but reject when
	// the trimmed key would be empty. trimKey stays false so " key=v" is
	// stored under " key" exactly as before.
	return parseKVPairs[string](flags, kvOpts[string]{
		errFmt:            "--var %q must be key=value",
		requireTrimmedKey: true,
	})
}

func RunScheduleList(p *Printer, opts ScheduleCommonOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}
	m, err := loadScheduleManifest(path)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(m)
		return nil
	}
	p.Header(fmt.Sprintf("Schedules — %s", path))
	if len(m.Schedules) == 0 {
		p.Line("  (none) — add one with `iterion schedule add`")
		return nil
	}
	rows := make([][]string, 0, len(m.Schedules))
	for _, e := range m.Schedules {
		state := "enabled"
		if e.Disabled {
			state = "disabled"
		}
		rows = append(rows, []string{e.Name, e.Cron, e.Bot, e.Workdir, state})
	}
	p.Table([]string{"NAME", "CRON", "BOT", "WORKDIR", "STATE"}, rows)
	return nil
}

func RunScheduleRemove(p *Printer, opts ScheduleRefOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}
	m, err := loadScheduleManifest(path)
	if err != nil {
		return err
	}
	if !m.remove(opts.Name) {
		return fmt.Errorf("schedule %q not found in %s", opts.Name, path)
	}
	if err := saveScheduleManifest(path, m); err != nil {
		return err
	}
	p.Line("✓ schedule %q removed → %s", opts.Name, path)
	p.Line("  run `iterion schedule install` to update the host crontab")
	return nil
}

// ---------------------------------------------------------------------------
// run — invoked by cron (or by hand) to execute one schedule
// ---------------------------------------------------------------------------

// runRunFn is a seam so schedule tests can capture the RunOptions the
// gate hands to the engine without executing a real run (same pattern
// as the crontab reader/writer seams below).
var runRunFn = RunRun

func RunScheduleRun(ctx context.Context, p *Printer, opts ScheduleRunOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}
	m, err := loadScheduleManifest(path)
	if err != nil {
		return err
	}
	e, idx := m.find(opts.Name)
	if idx < 0 {
		return fmt.Errorf("schedule %q not found in %s", opts.Name, path)
	}
	if e.Disabled && !opts.DryRun {
		p.Line("schedule %q is disabled — skipping", e.Name)
		return nil
	}

	runOpts := RunOptions{
		File:          e.Bot,
		StoreDir:      e.StoreDir,
		Sandbox:       e.Sandbox,
		NoInteractive: true, // cron has no TTY — never block on a human pause
		Retry:         e.retryPolicy(),
	}
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil {
			return fmt.Errorf("schedule %q: invalid timeout %q: %w", e.Name, e.Timeout, err)
		}
		runOpts.Timeout = d
	}
	if len(e.Vars) > 0 {
		flags := make([]string, 0, len(e.Vars))
		for _, k := range sortedKeys(e.Vars) {
			flags = append(flags, k+"="+e.Vars[k])
		}
		vars, err := ParseVarFlags(flags)
		if err != nil {
			return err
		}
		runOpts.Vars = vars
	}

	if opts.DryRun {
		p.Line("schedule %q would run:", e.Name)
		p.Line("  cd %s && iterion run %s%s", e.Workdir, e.Bot, dryRunArgs(*e))
		return nil
	}

	if e.Workdir != "" {
		prev, _ := os.Getwd()
		if err := os.Chdir(e.Workdir); err != nil {
			return fmt.Errorf("schedule %q: chdir %s: %w", e.Name, e.Workdir, err)
		}
		if prev != "" {
			// chdir is process-global, not goroutine-local: restore it so
			// RunScheduleRun is safe to call from a long-lived process, not
			// only a one-shot cron child.
			defer func() { _ = os.Chdir(prev) }()
		}
	}

	// Overlap + guard gate (pkg/schedgate). Skips exit 0: a skipped tick
	// is a normal outcome recorded in the tick audit, not a cron error
	// (non-zero would mailspam the operator every blocked tick).
	policy := e.policy()
	auditPath := tickAuditPath(path)

	// Must resolve exactly like the launch below (RunRun -> storeAnchorDir), or
	// the gate lists runs from one store while the tick writes to another:
	// LiveRunsForSchedule comes back empty, EvaluateOverlap always fires, and
	// every entry without an explicit --store-dir silently loses its
	// at-most-one-live guarantee.
	storeDir := runStoreDir(e.Bot, e.StoreDir)
	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("schedule %q: open store %s for overlap check: %w", e.Name, storeDir, err)
	}
	out := schedgate.Apply(ctx, schedgate.GateInput{
		Policy:     policy,
		Lister:     s,
		ScheduleID: e.Name,
		Record:     newHostCronTickRecord(*e, ""),
		GuardDir:   e.Workdir,
		GuardEnv: []string{
			"ITERION_SCHEDULE=" + e.Name,
			"ITERION_SCHEDULE_BOT=" + e.Bot,
		},
	})
	if !out.Proceed {
		writeTickAudit(p, auditPath, out.Record)
		switch out.Record.Decision {
		case schedgate.TickSkippedOverlap:
			p.Line("⏭ schedule %q skipped — %s (cancel it or set overlap: allow; see `iterion schedule audit --name %s`)", e.Name, out.Record.Reason, e.Name)
		case schedgate.TickGuardBlocked:
			p.Line("⏭ schedule %q skipped — %s", e.Name, out.Record.Reason)
		default:
			p.Line("⏭ schedule %q skipped — guard error: %s", e.Name, out.Record.Error)
		}
		return nil
	}
	if out.GuardRan {
		if runOpts.Vars == nil {
			runOpts.Vars = map[string]string{}
		}
		runOpts.Vars[policy.GuardVar] = out.GuardStdout
	}

	// Pre-mint the run ID so the fired audit row correlates with the run
	// even when RunRun fails downstream, and stamp the schedule
	// provenance the overlap check queries on the next tick.
	runID, err := store.GenerateRunID()
	if err != nil {
		return fmt.Errorf("schedule %q: generate run id: %w", e.Name, err)
	}
	runOpts.RunID = runID
	runOpts.Source = &store.RunSource{
		Kind:         store.RunSourceKindSchedule,
		ScheduleID:   e.Name,
		ScheduleName: e.Name,
	}

	p.Line("▶ schedule %q: iterion run %s", e.Name, e.Bot)
	runErr := runRunFn(ctx, runOpts, p)

	rec := newHostCronTickRecord(*e, schedgate.TickFired)
	rec.RunID = runID
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	writeTickAudit(p, auditPath, rec)
	return runErr
}

// dryRunArgs renders the non-default run flags for the dry-run preview.
func dryRunArgs(e ScheduleEntry) string {
	var b strings.Builder
	for _, k := range sortedKeys(e.Vars) {
		fmt.Fprintf(&b, " --var %s=%s", k, e.Vars[k])
	}
	if e.StoreDir != "" {
		fmt.Fprintf(&b, " --store-dir %s", e.StoreDir)
	}
	if e.Sandbox != "" {
		fmt.Fprintf(&b, " --sandbox %s", e.Sandbox)
	}
	if e.Timeout != "" {
		fmt.Fprintf(&b, " --timeout %s", e.Timeout)
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// install / uninstall — host crontab integration
// ---------------------------------------------------------------------------

const (
	cronBlockBegin = "# >>> iterion schedules (managed by `iterion schedule install`) >>>"
	cronBlockEnd   = "# <<< iterion schedules <<<"
)

// crontabReader / crontabWriter are seams so install/uninstall can be tested
// without mutating the real host crontab.
var (
	crontabReader = defaultCrontabRead
	crontabWriter = defaultCrontabWrite
)

func defaultCrontabRead() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "crontab", "-l")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// A user with no crontab yet makes `crontab -l` exit non-zero with
		// "no crontab for <user>" — that's an empty crontab, not a failure.
		if strings.Contains(strings.ToLower(errb.String()), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func defaultCrontabWrite(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "crontab", "-")
	cmd.Stdin = strings.NewReader(text)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crontab -: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

func RunScheduleInstall(p *Printer, opts ScheduleInstallOptions) error {
	path, err := resolveScheduleManifestPath(opts.ManifestPath)
	if err != nil {
		return err
	}
	m, err := loadScheduleManifest(path)
	if err != nil {
		return err
	}
	if len(m.Schedules) == 0 {
		return fmt.Errorf("no schedules in %s — add one with `iterion schedule add` first", path)
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve iterion binary path: %w", err)
	}
	tz := opts.TZ
	if tz == "" {
		tz = "UTC"
	}
	logDir := filepath.Join(filepath.Dir(path), "logs")
	block := renderCronBlock(m, cronBlockParams{
		binPath:      bin,
		manifestPath: path,
		pathEnv:      os.Getenv("PATH"),
		logDir:       logDir,
		tz:           tz,
	})

	if opts.Print {
		p.Line("%s", block)
		return nil
	}

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir %s: %w", logDir, err)
	}
	existing, err := crontabReader()
	if err != nil {
		return err
	}
	if err := crontabWriter(spliceManagedBlock(existing, block)); err != nil {
		return err
	}

	enabled := 0
	for _, e := range m.Schedules {
		if !e.Disabled {
			enabled++
		}
	}
	p.Line("✓ installed %d schedule(s) into the host crontab (CRON_TZ=%s)", enabled, tz)
	p.Line("  manifest: %s", path)
	p.Line("  logs:     %s/schedule-<name>.log", logDir)
	p.Line("  verify:   crontab -l")
	return nil
}

func RunScheduleUninstall(p *Printer, opts ScheduleCommonOptions) error {
	existing, err := crontabReader()
	if err != nil {
		return err
	}
	if !strings.Contains(existing, cronBlockBegin) {
		p.Line("no iterion-managed schedules found in the host crontab")
		return nil
	}
	stripped := stripManagedBlock(existing)
	if stripped != "" {
		stripped += "\n"
	}
	if err := crontabWriter(stripped); err != nil {
		return err
	}
	p.Line("✓ removed iterion-managed schedules from the host crontab")
	p.Line("  (the manifest is left intact — `iterion schedule install` re-applies it)")
	return nil
}

// ---------------------------------------------------------------------------
// crontab block rendering + splice (pure, unit-tested)
// ---------------------------------------------------------------------------

type cronBlockParams struct {
	binPath      string
	manifestPath string
	pathEnv      string
	logDir       string
	tz           string
}

// stripCRLF removes carriage returns and newlines so an env value written
// into the crontab block (PATH, CRON_TZ) can't inject additional cron lines.
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// renderCronBlock turns the manifest into the managed crontab block. Each
// enabled entry routes through `iterion schedule run <name>` so the manifest
// stays authoritative at trigger time; disabled entries are emitted as
// comments so `crontab -l` still documents them.
func renderCronBlock(m *ScheduleManifest, prm cronBlockParams) string {
	var b strings.Builder
	b.WriteString(cronBlockBegin + "\n")
	b.WriteString("# Managed by iterion — edit the manifest then `iterion schedule install`.\n")
	b.WriteString("# Remove with `iterion schedule uninstall`.\n")
	if prm.tz != "" {
		// CRON_TZ is honoured by cronie/Vixie cron; elsewhere it is a harmless
		// env var. It makes the schedules fire in the intended zone (UTC by
		// default) regardless of the host's local time.
		b.WriteString("CRON_TZ=" + stripCRLF(prm.tz) + "\n")
	}
	if prm.pathEnv != "" {
		// cron runs with a minimal PATH; capture the install-time PATH so
		// docker/git/the scanners are reachable. Strip CR/LF so a hostile
		// PATH (or TZ) value can't inject extra crontab lines.
		b.WriteString("PATH=" + stripCRLF(prm.pathEnv) + "\n")
	}
	for _, e := range m.Schedules {
		if e.Disabled {
			fmt.Fprintf(&b, "# (disabled) %s — %s %s\n", e.Name, e.Cron, e.Bot)
			continue
		}
		if e.Description != "" {
			b.WriteString("# " + e.Description + "\n")
		}
		logFile := filepath.Join(prm.logDir, "schedule-"+e.Name+".log")
		fmt.Fprintf(&b, "%s cd %s && %s schedule run %s --manifest %s >> %s 2>&1\n",
			e.Cron,
			shellQuote(e.Workdir),
			shellQuote(prm.binPath),
			shellQuote(e.Name),
			shellQuote(prm.manifestPath),
			shellQuote(logFile),
		)
	}
	b.WriteString(cronBlockEnd)
	return b.String()
}

// stripManagedBlock removes the managed block (markers inclusive) and trims
// trailing blank lines, leaving any user-authored crontab lines untouched.
func stripManagedBlock(existing string) string {
	lines := strings.Split(existing, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, ln := range lines {
		switch strings.TrimSpace(ln) {
		case cronBlockBegin:
			inBlock = true
			continue
		case cronBlockEnd:
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// spliceManagedBlock returns existing with the managed block replaced (if one
// is present) or appended. Idempotent: splicing the same block twice yields
// the same result.
func spliceManagedBlock(existing, block string) string {
	base := stripManagedBlock(existing)
	if base == "" {
		return block + "\n"
	}
	return base + "\n\n" + block + "\n"
}

// shellQuote single-quotes s for a POSIX sh command line, leaving obviously
// safe tokens bare. Used for the crontab `cd`/binary/arg fields.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '/' || r == '.' || r == '_' || r == '-' ||
			r == ':' || r == '@' || r == '+' || r == ','
		if !safe {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
