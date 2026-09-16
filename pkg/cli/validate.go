package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dryrun"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/subbotcontracts"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/fix"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// ValidateResult holds the outcome of a validate command.
type ValidateResult struct {
	File               string   `json:"file"`
	Valid              bool     `json:"valid"`
	WorkflowName       string   `json:"workflow_name,omitempty"`
	NodeCount          int      `json:"node_count,omitempty"`
	EdgeCount          int      `json:"edge_count,omitempty"`
	BundleName         string   `json:"bundle_name,omitempty"`
	BundleVersion      string   `json:"bundle_version,omitempty"`
	ParseDiagnostics   []string `json:"parse_diagnostics,omitempty"`
	CompileDiagnostics []string `json:"compile_diagnostics,omitempty"`
	// BundleDiagnostics holds manifest↔workflow consistency findings
	// (bundlelint, C2xx). Kept separate from CompileDiagnostics so the
	// studio can distinguish DSL-level from manifest-level issues.
	BundleDiagnostics []string `json:"bundle_diagnostics,omitempty"`
	// Diagnostics is the structured form of the three lists above: one
	// object per finding with its stage, code, severity, source position,
	// message and fix line — what a validate loop (an agent, an editor, the
	// MCP local_validate tool) acts on. The string lists stay for readers
	// that only print.
	Diagnostics []ValidateDiagnostic `json:"diagnostics,omitempty"`
	// PublicContract is the contract the workflow keeps, as the compiler
	// bound it (ADR-099) — present only when the program compiles without
	// an error, so a view is never shown of a program that is not one.
	PublicContract *ir.PublicContract `json:"public_contract,omitempty"`
	// Exec is the dry run's report (`--exec`): what two passes of the
	// compiled program met without a model, a shell or the workspace —
	// present only when the program compiles without an error, and only
	// when asked. Its findings are not diagnostics: they do not decide
	// `valid`, they tell the author what the first run would have met.
	Exec *dryrun.Report `json:"exec,omitempty"`
	// ExecError says why the dry run asked for did not run — the fixtures
	// could not be read, or the run failed; the compile verdict stands.
	ExecError string `json:"exec_error,omitempty"`
}

// ValidateOptions widen `iterion validate`. Exec runs the compiled program
// under a dry run once it compiles (pkg/dryrun); Fixtures names a JSON file
// of node outputs the dry run answers with — an object `{node: output}`, or
// the replay shape, a list of `{"node": …, "output": {…}}` — and implies
// Exec.
type ValidateOptions struct {
	Exec bool
	// ExecTimeout bounds one pass of the dry run, the children it simulates
	// included; zero is a minute. A pass that runs out of time is said so in
	// the report (`timed_out`), apart from a death of the program.
	ExecTimeout time.Duration
	// Strict fails the command when the dry run's report is not clean —
	// a pass died, or a reference, shell or fixture finding stands — the
	// switch a CI gate flips; without it the exit code is the compiler's,
	// and `exec.clean` in the JSON is the report's word.
	Strict   bool
	Fixtures string
	// Vars and Preset give the dry run its launch values — `--var k=v` and
	// `--preset` as `run` reads them; Inputs are the same values already
	// typed (the MCP tool's object), merged after them. A var without a
	// default and without a value stays shaped. Any of them implies Exec.
	Vars   []string
	Preset string
	Inputs map[string]any
}

// ValidateDiagnostic is one finding of `iterion validate` in the shape a
// tool can act on: where (file:line:column when the stage could attribute
// one), what (code + message) and the one-line fix.
type ValidateDiagnostic struct {
	Source   string `json:"source"` // parse | compile | bundle
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"` // error | warning
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
	// Edit is the mechanical remedy the diagnostic carries, when its fix
	// is the same every time (pkg/dsl/fix; `iterion fix` applies it): the
	// literal before and after, at its line and column.
	Edit *fix.Edit `json:"edit,omitempty"`
}

// formatDiagnostic renders one finding for the human output: the
// position when known, then severity, code and message on one line, and
// the fix line indented beneath it.
func formatDiagnostic(d ValidateDiagnostic) []string {
	var b strings.Builder
	if d.Line > 0 {
		fmt.Fprintf(&b, "%s:%d:%d: ", d.File, d.Line, d.Column)
	}
	b.WriteString(d.Severity)
	if d.Code != "" {
		fmt.Fprintf(&b, " [%s]", d.Code)
	}
	b.WriteString(": ")
	b.WriteString(d.Message)
	lines := []string{b.String()}
	if d.Hint != "" {
		lines = append(lines, "    fix: "+d.Hint)
	}
	return lines
}

// sortValidateDiagnostics orders the findings the way a reader edits: by
// source position, then code, node, edge and message. A finding with no
// position (a global compile check, a bundle check) goes LAST: it is
// usually a consequence of a positioned one — `entry node "a" not found`
// because a tab on line 2 broke the declaration — and must not be the first
// line the operator reads. The compiler already orders its own list this
// way; RunValidate concatenates the parse, compile and bundle stages, so the
// same key is applied once more across them, otherwise a parse finding on
// line 12 precedes a compile finding on line 7.
func sortValidateDiagnostics(ds []ValidateDiagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if (a.Line == 0) != (b.Line == 0) {
			return a.Line != 0
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.EdgeID != b.EdgeID {
			return a.EdgeID < b.EdgeID
		}
		return a.Message < b.Message
	})
}

// printDiagnostics writes the structured findings under a heading.
func printDiagnostics(p *Printer, diags []ValidateDiagnostic) {
	if len(diags) == 0 {
		return
	}
	p.Blank()
	p.Line("  Diagnostics:")
	for _, d := range diags {
		for _, l := range formatDiagnostic(d) {
			p.Line("    %s", l)
		}
	}
}

// RunValidate parses, compiles, and validates a .bot file or `.botz`
// archive. For bundles, the workflow source is extracted to a cache
// directory and validated; bundle metadata (name, version) is reported
// alongside the workflow result.
func RunValidate(path string, p *Printer) error {
	return RunValidateWith(path, p, ValidateOptions{})
}

// RunValidateWith is RunValidate with its options.
func RunValidateWith(path string, p *Printer, opts ValidateOptions) error {
	return RunValidateWithContext(context.Background(), path, p, opts)
}

// RunValidateWithContext is RunValidateWith under a context that bounds the
// dry run — its passes and the children they simulate.
func RunValidateWithContext(ctx context.Context, path string, p *Printer, opts ValidateOptions) error {
	path = ResolveRecipePath(path)
	if err := requireWorkflowPathExists(path); err != nil {
		return err
	}

	// Bundle dispatch: detect .botz or directory bundles and unpack before
	// validating, via the shared helper (same path as run/resume/doctor).
	// Plain .bot paths fall through with a nil bundle.
	bundleHandle, iterPath, kind, cleanup, err := openBundleOrFile(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer func() { _ = cleanup() }()

	var bundleName, bundleVersion, bundleDir string
	if bundleHandle != nil && bundleHandle.Manifest != nil {
		bundleName = bundleHandle.Manifest.Name
		bundleVersion = bundleHandle.Manifest.Version
	}
	switch kind {
	case bundle.KindBundle:
		// A .botz extracts to a cache dir, so the bundle's name-on-disk (used
		// by the per-bot-memory stability check) is the archive's stem.
		bundleDir = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	case bundle.KindBundleDir:
		// The bundle's OWN root, never the path the operator typed: a bare
		// main.bot promoted to its bundle by openBundleOrFile would else
		// name the FILE, and the per-bot-memory name-stability check (C230)
		// would refuse `validate bots/x/main.bot` on a bundle whose three
		// names agree — while `validate bots/x` passes.
		bundleDir = filepath.Base(bundleHandle.Dir)
	}
	path = iterPath

	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read file: %w", err)
	}

	result := &ValidateResult{
		File:          path,
		Valid:         true,
		BundleName:    bundleName,
		BundleVersion: bundleVersion,
	}

	// Parse — under the path in full: an include resolves beside the file
	// so named, and the compiler refuses a relative name rather than read
	// beside whatever the process sits in.
	parsePath := path
	if abs, err := filepath.Abs(path); err == nil {
		parsePath = abs
	}
	// The unit: this file as its main, the fragments its imports reach
	// read beside it — a bot in several files is validated as the program
	// it is, each file's diagnostics at its own path.
	u := unit.LoadDirWithMain(parsePath, parsePath, src)
	for _, d := range u.Diagnostics {
		result.ParseDiagnostics = append(result.ParseDiagnostics, d.Error())
		result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
			Source:   "parse",
			Code:     string(d.Code),
			Severity: d.Severity.String(),
			File:     d.File,
			Line:     d.Line,
			Column:   d.Column,
			Message:  d.Message,
			Hint:     d.Hint,
		})
		if d.Severity == parser.SeverityError {
			result.Valid = false
		}
	}

	// The profile the file is read in must be a choice (C144): a headerless
	// file that profile 2 would read otherwise is told so, with the counts.
	// An explicit `dsl: 1` IS the choice, and is told nothing.
	// Each file of the unit reads under its own profile, so each is told.
	for _, f := range u.Files {
		if f.AST == nil || f.AST.Profile != 0 || len(f.ProfileReads) == 0 {
			continue
		}
		escapes, paragraphs := 0, 0
		for _, r := range f.ProfileReads {
			if r.Kind == "escape" {
				escapes++
			} else {
				paragraphs++
			}
		}
		result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
			Source:   "parse",
			Code:     string(ir.DiagProfileOneMatters),
			Severity: "warning",
			File:     f.Name,
			Line:     f.ProfileReads[0].Line,
			Message: fmt.Sprintf("no `dsl:` header: read as profile 1, and profile 2 would read this file otherwise — %d quoted literal(s) hold a backslash, %d blank line(s) sit inside prompt bodies (first at line %d)",
				escapes, paragraphs, f.ProfileReads[0].Line),
			Hint: ir.HintFor(ir.DiagProfileOneMatters),
		})
	}

	// Bundle prompts must merge into the AST before ir.Compile validates
	// node-level prompt references.
	if bundleHandle != nil && u.Merged != nil {
		if err := runview.MergeBundlePrompts(u.Merged, bundleHandle); err != nil {
			return fmt.Errorf("bundle: merge prompts: %w", err)
		}
	}

	if u.Merged == nil || len(u.Merged.Workflows) == 0 {
		result.Valid = false
		why := "no workflow found"
		// A file under lib/ is a fragment: a piece of the bot whose main
		// imports it, and it holds no workflow by design. Validated alone
		// it can only fail; the remedy is the main.
		if filepath.Base(filepath.Dir(parsePath)) == unit.FragmentDir {
			why = "no workflow found: " + filepath.Base(parsePath) + " is a fragment under " + unit.FragmentDir + "/, validated through the main that imports it"
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "parse",
				Severity: "error",
				File:     parsePath,
				Message:  why,
				Hint:     "run `iterion validate` on the bot's main file (the one with `import \"" + unit.FragmentDir + "/" + filepath.Base(parsePath) + "\"`)",
			})
		}
		sortValidateDiagnostics(result.Diagnostics)
		if p.Format == OutputJSON {
			p.JSON(result)
		} else {
			p.Header("Validate: " + path)
			printDiagnostics(p, result.Diagnostics)
			p.Line("  result: INVALID (" + why + ")")
		}
		return validationFailed(p)
	}

	// Compile (includes static validation).
	cr := ir.Compile(u.Merged)
	for _, d := range cr.Diagnostics {
		result.CompileDiagnostics = append(result.CompileDiagnostics, d.Error())
		result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
			Source:   "compile",
			Code:     string(d.Code),
			Severity: d.Severity.String(),
			File:     d.File,
			Line:     d.Line,
			Column:   d.Column,
			Message:  d.Message,
			Hint:     d.Hint,
			NodeID:   d.NodeID,
			EdgeID:   d.EdgeID,
		})
		if d.Severity == ir.SeverityError {
			result.Valid = false
		}
	}
	annotateEdits(result, u, cr.Diagnostics)

	if cr.Workflow != nil {
		if err := mcp.PrepareWorkflow(cr.Workflow, filepath.Dir(path)); err != nil {
			result.CompileDiagnostics = append(result.CompileDiagnostics, err.Error())
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "compile",
				Severity: "error",
				Message:  err.Error(),
			})
			result.Valid = false
		}
		result.WorkflowName = cr.Workflow.Name
		result.NodeCount = len(cr.Workflow.Nodes)
		result.EdgeCount = len(cr.Workflow.Edges)
		if result.Valid {
			result.PublicContract = cr.Workflow.Contract
		}
	}

	// Bundle consistency: cross-check the manifest against the compiled
	// workflow (var maps, forge secret, capabilities, per-bot-memory name
	// stability). Only runs for bundles; plain .bot files have no manifest.
	if bundleHandle != nil && cr.Workflow != nil {
		syntax := bundle.MaxSyntaxRequirementsDir(bundleHandle.Dir)
		diags := bundlelint.CheckConsistency(bundlelint.Input{
			// nil for a bundle known by its skills/ alone: the profile checks
			// still run, the manifest-side ones are skipped.
			Manifest:    bundleHandle.Manifest,
			Workflow:    cr.Workflow,
			Frontmatter: bundle.ParseFrontmatter(src), // reuse the bytes already read
			DirName:     bundleDir,
			Skills:      scanBundleSkills(bundleHandle.SkillsDir),
			// The engine contract (C250/C251), held against THIS binary — the
			// author's local half of the guard the push admission and the
			// runner apply on a deployment.
			EngineBuild: appinfo.FullVersion(),
			// What the executable sources use (C252): a profile above 1,
			// `import` or a `contract` asks for a declared floor.
			Syntax: syntax,
			// The children's contracts, for the subbot projection (C255):
			// each `subbot source:` read within the bundle's collection and
			// compiled as its own unit.
			SubbotContracts: subbotcontracts.Read(bundleHandle.Dir, parsePath, cr.Workflow),
		})
		for _, d := range diags {
			result.BundleDiagnostics = append(result.BundleDiagnostics, d.Error())
			msg := d.Message
			if d.Field != "" {
				msg = d.Field + ": " + msg
			}
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "bundle",
				Code:     string(d.Code),
				Severity: d.Severity.String(),
				Message:  msg,
				Hint:     d.Hint,
			})
			if d.Severity == bundlelint.SeverityError {
				result.Valid = false
			}
		}
	}
	// A file named like a manifest beside a loose main.bot that did NOT
	// mark it — a typo in its only distinctive key, a file the parser
	// cannot read — leaves the file validated alone, and the verdict says
	// why (C223): the one outcome that would otherwise be silent.
	if bundleHandle == nil {
		if m, why := bundle.ForeignManifestBeside(path); m != "" {
			msg := filepath.Base(m) + " beside main.bot was not read as this bundle's manifest: it " + why + " — the file was validated alone, without the prompts, presets and skills beside it"
			result.BundleDiagnostics = append(result.BundleDiagnostics, "warning ["+string(bundlelint.DiagManifestNotRead)+"]: "+msg)
			result.Diagnostics = append(result.Diagnostics, ValidateDiagnostic{
				Source:   "bundle",
				Code:     string(bundlelint.DiagManifestNotRead),
				Severity: "warning",
				Message:  msg,
				Hint:     "if it is this bot's manifest, fix it (the reason names the keys); a manifest of another tool beside a loose main.bot needs nothing",
			})
		}
	}

	// The dry run, on a program that compiles: never on one that does not —
	// the diagnostics above are its remedy, a run of it would meet them
	// again as noise.
	// A dry run that could not run — fixtures unreadable, the run itself
	// failing — is said in the result beside the compile verdict, which
	// stands and is printed; the command then exits non-zero for the dry
	// run, not for the program.
	if opts.wantsDryRun() && result.Valid && cr.Workflow != nil {
		fixtures, err := loadDryRunFixtures(opts.Fixtures)
		inputs, ierr := dryRunInputs(cr.Workflow, opts)
		if err == nil {
			err = ierr
		}
		if err != nil {
			result.ExecError = err.Error()
		} else {
			collection := filepath.Dir(parsePath)
			if bundleHandle != nil {
				collection = bundleHandle.Dir
			}
			report, err := dryrun.Run(ctx, cr.Workflow, dryrun.Options{
				Fixtures: fixtures,
				Inputs:   inputs,
				Path:     parsePath,
				Children: dryRunChildren(collection),
				Timeout:  opts.ExecTimeout,
			})
			if err != nil {
				result.ExecError = "dry run: " + err.Error()
			} else {
				result.Exec = report
			}
		}
	}

	sortValidateDiagnostics(result.Diagnostics)
	if p.Format == OutputJSON {
		p.JSON(result)
	} else {
		p.Header("Validate: " + path)
		if bundleName != "" || bundleVersion != "" {
			p.KV("Bundle", bundleName+" "+bundleVersion)
		}
		if cr.Workflow != nil {
			p.KV("Workflow", result.WorkflowName)
			p.KV("Nodes", fmt.Sprintf("%d", result.NodeCount))
			p.KV("Edges", fmt.Sprintf("%d", result.EdgeCount))
			if result.PublicContract != nil {
				printPublicContract(p, result.PublicContract)
			}
		}
		printDiagnostics(p, result.Diagnostics)
		if result.Exec != nil {
			p.Blank()
			for _, line := range strings.Split(strings.TrimRight(result.Exec.Render(), "\n"), "\n") {
				p.Line("  " + line)
			}
		}
		if result.ExecError != "" {
			p.Blank()
			p.Line("  dry run: not run — " + result.ExecError)
		}
		p.Blank()
		if result.Valid {
			p.Line("  result: OK")
		} else {
			p.Line("  result: INVALID")
		}
	}

	if !result.Valid {
		return validationFailed(p)
	}
	if result.ExecError != "" {
		return dryRunFailed(p, result.ExecError)
	}
	if opts.Strict && result.Exec != nil && !result.Exec.Clean() {
		return dryRunNotClean(p, result.Exec)
	}
	return nil
}

// dryRunNotClean is the error of a dry run whose report is not clean under
// --strict, returned AFTER the result was printed: in JSON mode it is
// marked ErrReported and the CLI prints nothing more. A pass that ran out
// of time is named as the bound's doing, with the flag that raises it.
func dryRunNotClean(p *Printer, report *dryrun.Report) error {
	if p.Format == OutputJSON {
		return fmt.Errorf("dry run not clean: %w", ErrReported)
	}
	for _, pass := range report.Passes {
		if pass.TimedOut {
			return fmt.Errorf("dry run not clean: a pass ran out of time before the run ended — the dry run's bound, not the program: raise it with --exec-timeout (see the report above)")
		}
	}
	return fmt.Errorf("dry run not clean: a pass died, or a reference, shell or fixture finding stands (see the report above)")
}

// dryRunFailed is the error of a dry run that could not run, returned AFTER
// the result — the compile verdict and the reason — was printed: in JSON
// mode it is marked ErrReported and the CLI prints nothing more.
func dryRunFailed(p *Printer, why string) error {
	if p.Format == OutputJSON {
		return fmt.Errorf("dry run failed: %w", ErrReported)
	}
	return fmt.Errorf("dry run failed: %s", why)
}

// annotateEdits attaches to each compile diagnostic the mechanical remedy
// it carries (pkg/dsl/fix), planned on its file's own bytes. One edit fixes
// every quoted reference of a literal at once and rides every diagnostic
// it remedies — the one whose message names the edit's property and one of
// its references, never the next free one: a node's command and its
// postcondition each carry their own.
func annotateEdits(result *ValidateResult, u *unit.Unit, diags []ir.Diagnostic) {
	for _, f := range u.Files {
		var mine []ir.Diagnostic
		for _, d := range diags {
			if d.File == f.Name {
				mine = append(mine, d)
			}
		}
		if len(mine) == 0 {
			continue
		}
		edits, _ := fix.PlanFor(f.Name, f.Source, mine)
		for i := range edits {
			e := edits[i]
			for _, ref := range e.Refs {
				for j := range result.Diagnostics {
					vd := &result.Diagnostics[j]
					if vd.Source == "compile" && vd.Edit == nil && vd.Code == string(e.Code) && vd.NodeID == e.Node && vd.File == f.Name &&
						strings.Contains(vd.Message, e.Property+": "+ref+" sits inside quotes") {
						vd.Edit = &e
						break
					}
				}
			}
		}
	}
}

// loadDryRunFixtures reads the fixtures a dry run answers with: an object
// `{node: output}`, or the replay shape — a list of `{"node": …, "output":
// {…}}` (pkg/botreplay). An empty path is no fixture.
func loadDryRunFixtures(path string) (map[string]map[string]any, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixtures: %w", err)
	}
	out := map[string]map[string]any{}
	var asMap map[string]map[string]any
	if err := json.Unmarshal(raw, &asMap); err == nil {
		for k, v := range asMap {
			out[k] = v
		}
		return out, nil
	}
	var asList []struct {
		Node   string         `json:"node"`
		Output map[string]any `json:"output"`
	}
	if err := json.Unmarshal(raw, &asList); err != nil {
		return nil, fmt.Errorf("fixtures %s: neither an object of node outputs nor a list of {node, output}: %w", path, err)
	}
	for _, f := range asList {
		if f.Node == "" {
			return nil, fmt.Errorf("fixtures %s: an entry names no node", path)
		}
		out[f.Node] = f.Output
	}
	return out, nil
}

// dryRunChildren resolves a `subbot source:` for the dry run the way
// `validate` reads a child for its contract: within the collection that
// holds the bundle (bundle.ResolveChild), parsed as its own unit, compiled;
// a child that does not compile is not simulated — its own validate shows
// why. A registry child (`bot://`) is nothing here: the executor says so.
func dryRunChildren(collectionDir string) func(parent, source string) (string, *ir.Workflow, error) {
	return func(parent, source string) (string, *ir.Workflow, error) {
		path, src, ok := bundle.ResolveChild(collectionDir, parent, source)
		if !ok {
			return "", nil, fmt.Errorf("not read within the bundle's collection")
		}
		u := unit.LoadDirWithMain(path, path, src)
		if u.HasErrors() || u.Merged == nil {
			return path, nil, fmt.Errorf("does not parse")
		}
		cr := ir.Compile(u.Merged)
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				return path, nil, fmt.Errorf("does not compile: %s", d.Message)
			}
		}
		if cr.Workflow == nil {
			return path, nil, fmt.Errorf("has no workflow")
		}
		return path, cr.Workflow, nil
	}
}

// printPublicContract renders the contract the workflow keeps, as the
// compiler bound it: each port with its producer, each criterion with its
// evaluator, each effect.
func printPublicContract(p *Printer, c *ir.PublicContract) {
	title := fmt.Sprintf("%s v%d", c.Name, c.Version)
	if c.Responsibility != "" {
		title += " — " + c.Responsibility
	}
	p.KV("Contract", title)
	for _, side := range []struct {
		label string
		ports []*ir.PublicPort
	}{{"Input", c.Inputs}, {"Output", c.Outputs}} {
		for _, port := range side.ports {
			p.KV(side.label, describePublicPort(port))
		}
	}
	for _, k := range c.Criteria {
		line := k.Name + ": " + k.Kind
		if len(k.Params) > 0 {
			line += " " + string(k.Params)
		}
		line += " on " + k.Port
		if !k.Registered {
			line += " (not evaluated: no registered evaluator)"
		}
		p.KV("Criterion", line)
	}
	for _, e := range c.Effects {
		line := e.Name
		if e.Paid {
			line += " (paid)"
		}
		p.KV("Effect", line)
	}
}

// describePublicPort renders one port with everything an author checks a
// contract for: its requiredness and default, its nullability, its
// cardinality, its enum, its file shape and its producer.
func describePublicPort(port *ir.PublicPort) string {
	var notes []string
	if !port.Required {
		notes = append(notes, "optional")
	}
	if port.Default != nil {
		notes = append(notes, "default "+string(port.Default))
	}
	if port.Nullable {
		notes = append(notes, "nullable")
	}
	if port.MinItems != nil || port.MaxItems != nil {
		lo, hi := "0", "∞"
		if port.MinItems != nil {
			lo = fmt.Sprintf("%d", *port.MinItems)
		}
		if port.MaxItems != nil {
			hi = fmt.Sprintf("%d", *port.MaxItems)
		}
		notes = append(notes, lo+".."+hi+" items")
	}
	if len(port.EnumValues) > 0 {
		notes = append(notes, "one of "+strings.Join(port.EnumValues, "|"))
	}
	if port.File != nil {
		file := "file"
		if port.File.MediaType != "" {
			file += " " + port.File.MediaType
		}
		if port.File.MinBytes > 0 {
			file += fmt.Sprintf(" ≥ %d B", port.File.MinBytes)
		}
		if port.File.Schema != "" {
			file += " schema " + port.File.Schema
		}
		notes = append(notes, file)
	}
	line := port.Name + ": " + port.Type
	if len(notes) > 0 {
		line += " (" + strings.Join(notes, ", ") + ")"
	}
	if port.FromNode != "" {
		line += " ← " + port.FromNode
		if port.FromField != "" {
			line += "." + port.FromField
		}
	}
	return line
}

// validationFailed is the non-zero exit of an invalid workflow. In --json mode
// the result already carries the verdict and every finding, so the error is
// marked ErrReported and the CLI prints nothing more.
func validationFailed(p *Printer) error {
	if p.Format == OutputJSON {
		return fmt.Errorf("validation failed: %w", ErrReported)
	}
	return fmt.Errorf("validation failed")
}

// scanBundleSkills reads a bundle's skills/*.md frontmatter (name +
// description) for the routability lint (C231–C234), using the shared
// skilllib parser so it never drifts from run-time discovery. An empty or
// missing dir yields no docs (the checks then no-op). Non-.md entries and
// unreadable files are skipped silently — the lint judges routability of the
// skills that exist, not filesystem health.
func scanBundleSkills(skillsDir string) []bundlelint.SkillDoc {
	if skillsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var docs []bundlelint.SkillDoc
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		f, err := os.Open(filepath.Join(skillsDir, e.Name()))
		if err != nil {
			continue
		}
		name, desc := skilllib.ScanFrontmatter(f)
		_ = f.Close()
		docs = append(docs, bundlelint.SkillDoc{
			Path:        filepath.Join(bundle.DirSkills, e.Name()),
			Name:        name,
			Description: desc,
		})
	}
	return docs
}

// wantsDryRun says the options ask for the dry run — by its switch, or by
// anything only the dry run reads.
func (o ValidateOptions) wantsDryRun() bool {
	return o.Exec || o.Strict || o.Fixtures != "" || len(o.Vars) > 0 || o.Preset != "" || len(o.Inputs) > 0
}

// dryRunInputs is what the dry run's launch supplies: the preset, then the
// --var flags, then the typed inputs — the precedence `run` has. A bot that
// guards its entry on a var (the gallery's TAG_UNSET shape) is otherwise
// refused at the gate on every pass, and nothing behind it is walked.
func dryRunInputs(wf *ir.Workflow, opts ValidateOptions) (map[string]any, error) {
	vars, err := ParseVarFlags(opts.Vars)
	if err != nil {
		return nil, err
	}
	inputs, err := buildRunInputs(wf, opts.Preset, vars)
	if err != nil {
		return nil, err
	}
	for k, v := range opts.Inputs {
		inputs[k] = v
	}
	return inputs, nil
}
