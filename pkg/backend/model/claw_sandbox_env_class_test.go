package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The in-container claw runner (`iterion __claw-runner`) is this same binary:
// it rebuilds its registry and its claw backend from the env that crossed —
// forwardableProviderEnv — and from the task envelope, nothing else of the
// host's. So every env var this package reads is forwarded
// (providerCredentialEnvVars), crossed by a branch of forwardableProviderEnv
// that decides its value, or read on the host only, by functions the runner
// never reaches. A knob that is none of the three is honoured by a host node
// and silently ignored by its sandboxed twin, which keeps working against the
// wrong endpoint, budget or timeout: nothing else in the suite sees that.
//
// The scan reads the code as the compiler does: go/types over the toolchain's
// own export data (`go list -export`), as the fswatch inventory does. A name
// is the constant the call spells — a literal, a constant of any scope or
// package, a constant expression — and a reader is the function the call
// resolves to, however imported, aliased or shadowed. Readers are found, not
// listed: a function that hands a parameter (or its receiver), as passed, to
// an env reader is one, in this package or in any package of this module it
// depends on (ir.LookupEnv, envtrust.Inherited, ...), and so is a variable or
// a field one is stored in. What the runner reaches is walked from
// runClawRunner through the runner binary, then through this package; the
// rest of the binary counts by its direct uses of this package, since which
// command a function serves is beyond the scan (binaryHostOnly lists the
// ones reviewed as another command's).
//
// A name can reach a reader by a road no scan follows — a slice, a struct
// field, a getter returned or installed at init, an interface, another
// module's helper. So every constant this package spells like an env name
// (FOO_BAR, FOO_BAR= heading an environ entry, FOO_BAR_ prefixing a built
// name, ${FOO_BAR} in a template) is classified too, read or not: a
// host-side one is spelled only where the runner does not reach, envWrites
// lists the ones this package only sets in an env it hands on, notEnvNames
// the ones that name no variable. A name that crosses, crosses as the host's
// process env — the forwarding loop copies os.Getenv — so it is never read
// through a reader that also answers from state that does not cross
// (envStateConsults), such a reader is never handed to another package, and
// the name is spelled only at its reads or where crossingSpellings says why:
// its sandboxed twin would otherwise see another value.
//
// What it does not see, named rather than hidden: a name built at run time
// rather than spelled as a constant here — derived from data, the ${NAME}s a
// bot author spells, the node a tool command comes from, or computed from
// constants by a call (fmt.Sprintf, strings.ToUpper) on a road to a reader
// another package declares; what a function classified in envEnumerations
// makes of the environment it enumerates; what the runner binary reaches
// through another package off runClawRunner's path (its direct uses of this
// package count); and a variable another package reads under a name it
// spells itself, which is that package's to forward (#1804).

// crossedByItsOwnBranch: names forwardableProviderEnv sets itself, because
// the value that crosses is a decision, not the host's raw value.
var crossedByItsOwnBranch = map[string]string{
	"ITERION_OPENAI_USE_OAUTH":   `crosses as the refusal "0", or as "1" for the run's own ChatGPT forfait; a host-wide "1" stays home, where the run's own key crosses as the env key`,
	"ITERION_CODEX_HOST_VERSION": "the host's `codex --version` probe, sent when no ITERION_CODEX_VERSION override is set",
}

// Why a name stays on the host.
const (
	whyTaskField = "resolved by the executor into the task, which crosses in the task envelope"
	whyExecutor  = "read by the executor while it builds or routes the node, before anything is dispatched"
	whySinkGuard = "configures the run's secret guard, built on the host over every sink; the runner materialises nothing"
	whyHostTool  = "environment of a tool node's command, which the executor runs"
	whyBudget    = "the host claw backend's retry budget, which crosses in the task envelope (IOTask.Retry): it bounds billed attempts, so the runner never reads it from its own env"
)

// hostSideRead is a name the runner never needs, with the functions that
// read it, all on the host. The claim is about those sites: a read of the
// name anywhere else is refused, and so is a listed function the runner
// reaches.
type hostSideRead struct {
	why     string
	readers []string
}

var hostSideEnvReads = map[string]hostSideRead{
	"ITERION_CLAW_COMPACT_THRESHOLD_RATIO": {whyTaskField, []string{"resolveCompaction"}},
	"ITERION_CLAW_COMPACT_PRESERVE_RECENT": {whyTaskField, []string{"resolveCompaction"}},
	"ITERION_COMPRESS":                     {whyTaskField, []string{"NewClawExecutor"}},
	"ITERION_AUTO_MEMORY":                  {whyTaskField, []string{"NewClawExecutor"}},
	"ITERION_AMBIENT_CONTEXT":              {whyTaskField, []string{"NewClawExecutor"}},
	"ITERION_CLAW_SLASH_COMMANDS":          {whyTaskField, []string{"expandWorkspaceSlashCommand"}},
	"ITERION_CLAW_SLASH_COMMAND_MAX_BYTES": {whyTaskField, []string{"slashCommandMaxBytes"}},
	"ITERION_PERMISSION":                   {"resolved by the executor into the node's policy, which crosses as the permission_policy envelope", []string{"NewClawExecutor"}},
	"ITERION_NODE_MAX_RETRIES":             {whyBudget, []string{"RetryPolicyFromEnv"}},
	"ITERION_NODE_MAX_TRANSIENT_RETRIES":   {whyBudget, []string{"RetryPolicyFromEnv"}},
	"ITERION_DEFAULT_BACKEND":              {whyExecutor, []string{"(*ClawExecutor).resolveBackendName"}},
	"ITERION_DEFAULT_SUPERVISOR_MODEL":     {whyExecutor, []string{"(*ClawExecutor).executeLLMRouterUnified"}},
	"ITERION_VERIFIED_ACTION_MODEL":        {whyExecutor, []string{"(*ClawExecutor).recoveryModel"}},
	"ITERION_ROUTE_COOLDOWN":               {whyExecutor, []string{"NewClawExecutor"}},
	"ITERION_SECRETS_REDACT":               {whySinkGuard, []string{"secretGuardConfigFromEnv"}},
	"ITERION_SECRETS_REDACT_HEURISTIC":     {whySinkGuard, []string{"secretGuardConfigFromEnv"}},
	"ITERION_SECRETS_REDACT_DECODE":        {whySinkGuard, []string{"secretGuardConfigFromEnv"}},
	"ITERION_SECRETS_REDACT_MIN_SCORE":     {whySinkGuard, []string{"secretGuardConfigFromEnv"}},
	"ITERION_SECRETS_PLACEHOLDERS":         {whySinkGuard, []string{"secretGuardConfigFromEnv"}},
	"ITERION_TREE_NOISE":                   {whyHostTool, []string{"(*ClawExecutor).treeNoiseEnvAppend"}},
}

// binaryHostOnly: functions of the runner binary that only another command
// runs, reviewed — their uses of this package do not count as the runner's.
var binaryHostOnly = map[string]string{}

// launcherOnly: functions on the runner's path that only the launcher takes.
// The walk stops at them, and each must still be reached that way.
var launcherOnly = map[string]string{
	"(*ClawBackend).executeViaSandboxRunner": "the runner's tasks carry no sandbox (runClawRunner sets task.Sandbox = nil): Execute runs them in process",
}

// dynamicEnvReads: reads whose name is data — the forwarding list itself, a
// ${NAME} a tool command spells — never a knob. The exemption holds for the
// pair it names alone: the function that reads, and the reader it calls.
var dynamicEnvReads = map[[2]string]string{
	{"forwardableProviderEnv", "os.Getenv"}:   "the forwarding loop itself, over providerCredentialEnvVars",
	{"bracedEnvWouldExpand", "lookupToolEnv"}: whyHostTool + ": the ${NAME} references a tool command spells",
	{"resolveBracedEnvBody", "lookupToolEnv"}: whyHostTool + ": the ${NAME} references a tool command spells",
}

// envEnumerations: functions that hand the whole environment on, and read
// no knob from it.
var envEnumerations = map[string]string{
	"BuildSecretGuard":                      whySinkGuard + ": every value in the env is a secret candidate",
	"(*ClawExecutor).toolNodeCommand":       whyHostTool,
	"(*ClawExecutor).toolNodeScriptCommand": whyHostTool,
	"runCommandHook":                        "a settings hook's command inherits the env of the process that runs it — in a container, the runner's own, which is the point",
}

// getterHandoffs: calls into another package handed a reader itself, which
// read the names that package spells.
const whyGatewayEnv = "the gateway's own env, which forwardableProviderEnv's gateway branch carries for a gateway node, with the host's catalog answer"

var getterHandoffs = map[string]string{
	"pkg/backend/compatgw.FromEnv":            whyGatewayEnv,
	"pkg/backend/compatgw.ResolveCatalog":     whyGatewayEnv,
	"pkg/backend/compatgw.CheckOperatorTable": whyGatewayEnv,
}

// notEnvNames: constants this package spells like an env name, which no code
// reads from the environment, with what they are.
var notEnvNames = map[string]string{}

// envWrites: variables this package sets in an environment it hands on — the
// crossing itself, a tool node's command, a settings hook's — and never
// reads.
const (
	whyCrossingSets = "set in the crossing by forwardableProviderEnv's gateway branch, for the in-container factory"
	whyForfaitDir   = "set in the crossing to where the run's forfait lands in the container"
	whyHookEnv      = "the event a settings hook's command is told about, set in its env"
	whyToolEnv      = "set in a tool node command's env"
)

var envWrites = map[string]string{
	"OPENAI_COMPATIBLE_BASE_URL":         whyCrossingSets,
	"OPENAI_COMPATIBLE_API_KEY":          whyCrossingSets,
	"ITERION_LLM_ENDPOINT_ALLOW_PRIVATE": whyCrossingSets,
	"ITERION_OPENAI_COMPATIBLE_RESOLVED": whyCrossingSets,
	// The engine-owned sandbox proxy channel: this package only sets it in
	// the runner env it hands on; the reader is pkg/backend/compatgw.
	"ITERION_SANDBOX_PROXY_ENDPOINT": whyCrossingSets,
	"CODEX_HOME":                     whyForfaitDir,
	"CLAUDE_CONFIG_DIR":              whyForfaitDir,
	"HOOK_EVENT":                     whyHookEnv,
	"HOOK_TOOL_NAME":                 whyHookEnv,
	"HOOK_TOOL_INPUT":                whyHookEnv,
	"HOOK_USER_PROMPT":               whyHookEnv,
	"HOOK_MESSAGE_COUNT":             whyHookEnv,
	"ITERION_ARTIFACT_FILES_DIR":     whyToolEnv,
	"ITERION_ENGINE_BIN":             whyToolEnv,
}

// crossingSpellings: where this package spells a name that crosses other than
// at a read of it. A road from anywhere else to a reader that also answers
// from envStateConsults would go unseen.
var crossingSpellings = map[string]string{
	"var providerCredentialEnvVars": "the forwarding list itself",
	"forwardableProviderEnv":        "sets the crossing: a refusal, the run's own credentials, the host's resolved values",
	"var byokEnvVar":                "the variable a run's own key crosses as, which forwardableProviderEnv sets",
	"anthropicWireShadowEnv":        "the ambient keys the crossing blanks, so the run's forfait outranks them",
}

// envStateConsults answer for a name from state other than the process env,
// which the forwarding loop never copies. A reader that hands its name to one
// answers differently on the host than in the container.
var envStateConsults = map[string]string{
	"pkg/dsl/ir.lookupOverlay":      "the bot-var settings overlay (ir.SetEnvOverlay), installed by the cloud server and runner, never by the claw runner",
	"internal/envtrust.Planted":     "the names a workspace .env planted, which the host refuses and the forwarding loop copies",
	"pkg/dsl/ir.ProcessEnvReadable": "the cloud process-env policy (ir.SetProcessEnvPolicy), installed by the cloud server and runner, never by the claw runner",
}

// envNameShape is a constant spelled like an env var name, like the NAME_
// prefix a name is built on, or heading an environ entry (NAME=, whatever
// value follows).
var envNameShape = regexp.MustCompile(`^([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)(?:=|_?$)`)

// How a reader reads the variable an argument decides.
type envReadKind int

const (
	exactName     envReadKind = iota + 1 // the argument is the variable's name
	templateNames                        // the argument is a template whose $NAME and ${NAME} references are read
	opaqueName                           // the name is derived from the argument in a way the scan cannot read
)

// receiverArg is the argument index a method's receiver reads through.
const receiverArg = -1

// The env readers the scan starts from, by the argument that decides what
// they read. The functions of this module that hand them a parameter as
// passed are found from there.
var seedEnvReaders = map[string]map[int]envReadKind{
	"os.Getenv":                    {0: exactName},
	"os.LookupEnv":                 {0: exactName},
	"syscall.Getenv":               {0: exactName},
	"golang.org/x/sys/unix.Getenv": {0: exactName},
	"os.ExpandEnv":                 {0: templateNames},
}

// templateExpanders expand the template at the first index through the
// lookup at the second: handed an env reader, they read the template's
// references.
var templateExpanders = map[string][2]int{
	"os.Expand":                          {0, 1},
	"pkg/dsl/ir.ExpandWithDefault":       {0, 1},
	"pkg/dsl/ir.ExpandBracedWithDefault": {0, 1},
}

// envEnumerators hand back the whole environment.
var envEnumerators = map[string]bool{
	"os.Environ":                    true,
	"syscall.Environ":               true,
	"golang.org/x/sys/unix.Environ": true,
}

const (
	iterionModule = "github.com/SocialGouv/iterion/"
	modelPkgPath  = iterionModule + "pkg/backend/model"
	runnerPkgPath = iterionModule + "cmd/iterion"
	// runnerEntry is what `iterion __claw-runner` runs.
	runnerEntry = "cmd/iterion.runClawRunner"
	// packageLevel names the reads and the references of package-level
	// declarations: they run at init, in the runner too.
	packageLevel = "(package level)"
)

func TestEveryEnvReadIsClassifiedForTheSandbox(t *testing.T) {
	scan := scanPackageEnvReads(t)
	for _, problem := range scan.problems {
		t.Error(problem)
	}

	forwarded := make(map[string]bool, len(providerCredentialEnvVars))
	for _, name := range providerCredentialEnvVars {
		forwarded[name] = true
	}
	for _, name := range sortedKeys(scan.reads) {
		_, crossed := crossedByItsOwnBranch[name]
		host, isHost := hostSideEnvReads[name]
		switch sides := btoi(forwarded[name]) + btoi(crossed) + btoi(isHost); sides {
		case 0:
			t.Errorf("%s (read at %s) is not classified for the sandbox runner: forward it (providerCredentialEnvVars), cross it in its own branch of forwardableProviderEnv (crossedByItsOwnBranch), or declare it host-side with the reason and the functions that read it (hostSideEnvReads) — until then a sandboxed claw node runs without it",
				name, scan.sites(name))
			continue
		case 1:
		default:
			t.Errorf("%s is classified on %d sides; it crosses or it stays, once", name, sides)
			continue
		}
		if !isHost {
			continue
		}
		listed := map[string]bool{}
		for _, fn := range host.readers {
			listed[fn] = true
		}
		readBy := map[string]bool{}
		for _, r := range scan.reads[name] {
			readBy[r.fn] = true
			if !listed[r.fn] {
				t.Errorf("%s is declared host-side, read by %s; %s reads it too (at %s): if %s runs on the host only, list it in hostSideEnvReads; if the runner reaches it, forward the name",
					name, strings.Join(host.readers, ", "), r.fn, r.at, r.fn)
			}
		}
		for _, fn := range host.readers {
			if path, reached := scan.runnerPath(fn); reached {
				t.Errorf("%s is declared host-side, but %s, which reads it, is reached by the sandbox runner (%s): forward the name, or keep the read off the runner's path — a function of the runner binary only another command runs can be listed in binaryHostOnly", name, fn, path)
			}
			if !readBy[fn] {
				t.Errorf("hostSideEnvReads lists %s as a reader of %s, which reads it no more: drop it from the entry's readers", fn, name)
			}
		}
	}
	for _, table := range []map[string]bool{keysOf(crossedByItsOwnBranch), keysOf(hostSideEnvReads)} {
		for _, name := range sortedKeys(table) {
			if _, read := scan.reads[name]; !read {
				t.Errorf("%s is classified but no read of it was found: if its read moved behind a reader the scan does not know, teach the scan that reader; drop the entry only once this package no longer reads %s", name, name)
			}
		}
	}
	for _, name := range sortedKeys(scan.consulted) {
		if _, crossed := crossedByItsOwnBranch[name]; forwarded[name] || crossed {
			t.Errorf("%s crosses as the host's process env, which is what the forwarding loop copies, but it is read at %s through a reader that also answers from state that does not cross (envStateConsults): its sandboxed twin would see another value — read it as forwardableProviderEnv does, with os.Getenv, or make what crosses resolve it the same way",
				name, strings.Join(scan.consulted[name], ", "))
		}
	}
	for _, name := range sortedKeys(scan.spelled) {
		_, crossed := crossedByItsOwnBranch[name]
		_, isHost := hostSideEnvReads[name]
		_, read := scan.reads[name]
		_, notEnv := notEnvNames[name]
		_, written := envWrites[name]
		readAt := map[token.Pos]bool{}
		for _, r := range scan.reads[name] {
			readAt[r.pos] = true
		}
		switch {
		case forwarded[name] || crossed:
			for _, s := range scan.spelled[name] {
				if !readAt[s.pos] && crossingSpellings[s.decl] == "" {
					t.Errorf("%s crosses, and %s spells it (at %s) other than at a read of it: a road from there to a reader that also answers from envStateConsults would go unseen — read it there, or list %s in crossingSpellings with what it does with the name",
						name, s.decl, s.at, s.decl)
				}
			}
		case isHost:
			for _, s := range scan.spelled[name] {
				if readAt[s.pos] {
					continue // its reader is checked above
				}
				if path, reached := scan.runnerPath(s.fn); reached {
					t.Errorf("%s is declared host-side, but %s spells it (at %s) and the sandbox runner reaches %s (%s): whatever road takes it to a reader there, the runner reads its own env — forward the name, or keep it off the runner's path",
						name, s.fn, s.at, s.fn, path)
				}
			}
		case read: // reported unclassified above
		case !notEnv && !written:
			t.Errorf("%s is spelled like an env name (at %s), but no read of it is seen and it is not classified: if any road takes it to the environment — a slice, a field, a getter, an interface, another module's helper — classify it as a read would be; if this package only sets it in an env it hands on, list it in envWrites; if it names no variable, in notEnvNames",
				name, scan.spelledAt(name))
		}
	}
	for _, tbl := range []struct {
		table   string
		entries map[string]string
	}{{"notEnvNames", notEnvNames}, {"envWrites", envWrites}} {
		table := tbl.table
		for _, name := range sortedKeys(tbl.entries) {
			_, crossed := crossedByItsOwnBranch[name]
			_, isHost := hostSideEnvReads[name]
			_, read := scan.reads[name]
			switch _, spelled := scan.spelled[name]; {
			case !spelled:
				t.Errorf("%s lists %s, which this package spells no more: drop the entry", table, name)
			case read || forwarded[name] || crossed || isHost:
				t.Errorf("%s lists %s, which this package reads or classifies as a read: drop the entry", table, name)
			}
		}
	}
	spelledIn := map[string]bool{}
	for _, sites := range scan.spelled {
		for _, s := range sites {
			spelledIn[s.decl] = true
		}
	}
	for _, decl := range sortedKeys(crossingSpellings) {
		if !spelledIn[decl] {
			t.Errorf("crossingSpellings lists %s, which spells no env name any more: drop the entry", decl)
		}
	}
	for _, name := range sortedKeys(notEnvNames) {
		if _, both := envWrites[name]; both {
			t.Errorf("%s is listed both in notEnvNames and in envWrites: it names a variable or it does not", name)
		}
	}
	for _, fn := range sortedKeys(launcherOnly) {
		if !scan.cut[fn] {
			t.Errorf("launcherOnly lists %s, which the runner's path no longer leads to: drop the entry", fn)
		}
	}
	pairs := make([][2]string, 0, len(dynamicEnvReads))
	for pair := range dynamicEnvReads {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0]+pairs[i][1] < pairs[j][0]+pairs[j][1] })
	for _, pair := range pairs {
		if !scan.dynamicUsed[pair] {
			t.Errorf("dynamicEnvReads lists %s reading through %s, which reads no non-constant name there: drop the entry", pair[0], pair[1])
		}
	}
	for _, fn := range sortedKeys(scan.enumerations) {
		if _, ok := envEnumerations[fn]; !ok {
			t.Errorf("%s enumerates the environment (at %s): read the variables it needs by name, or classify the function in envEnumerations with what it hands the env to", fn, strings.Join(scan.enumerations[fn], ", "))
		}
	}
	for _, fn := range sortedKeys(envEnumerations) {
		if _, ok := scan.enumerations[fn]; !ok {
			t.Errorf("envEnumerations lists %s, which enumerates the environment no more: drop the entry", fn)
		}
	}
	for _, callee := range sortedKeys(scan.handoffs) {
		if _, ok := getterHandoffs[callee]; !ok {
			t.Errorf("an env reader handed to %s (at %s): read the names at the call site, or classify the call in getterHandoffs", callee, strings.Join(scan.handoffs[callee], ", "))
		}
	}
	for _, callee := range sortedKeys(getterHandoffs) {
		if _, ok := scan.handoffs[callee]; !ok {
			t.Errorf("getterHandoffs lists %s, which this package no longer hands an env reader: drop the entry", callee)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func keysOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type envRead struct {
	at   string    // file:line of the argument that names the variable
	pos  token.Pos // and its exact position
	fn   string    // the function the read happens in, packageLevel at init
	decl string    // for a spelling: the function, or "var X" at package level
}

type envScan struct {
	reads        map[string][]envRead
	spelled      map[string][]envRead // every constant spelled like an env name, by site
	consulted    map[string][]string  // name → reads through a reader that consults envStateConsults
	dynamicUsed  map[[2]string]bool
	enumerations map[string][]string // function → positions
	handoffs     map[string][]string // callee handed a reader → positions
	reachedFrom  map[string]string   // function the runner reaches → the one it is reached from, or what enters it
	cut          map[string]bool     // launcherOnly functions the walk stopped at
	problems     []string
}

func (s envScan) sites(name string) string { return joinSites(s.reads[name]) }

func (s envScan) spelledAt(name string) string { return joinSites(s.spelled[name]) }

func joinSites(rs []envRead) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.at+" in "+r.fn)
	}
	return strings.Join(out, ", ")
}

// runnerPath reports whether the runner reaches fn, and how.
func (s envScan) runnerPath(fn string) (string, bool) {
	if _, ok := s.reachedFrom[fn]; !ok {
		return "", false
	}
	path := []string{fn}
	for from := s.reachedFrom[fn]; from != ""; from = s.reachedFrom[from] {
		path = append([]string{from}, path...)
	}
	return strings.Join(path, " → "), true
}

type typedPackage struct {
	path    string
	dir     string
	goFiles []string
	files   []*ast.File
	info    *types.Info
	types   *types.Package
}

type typedModule struct {
	model   *typedPackage
	runner  *typedPackage   // cmd/iterion, the binary the runner is
	readers []*typedPackage // this package and every package of the module it depends on
	deps    map[string]map[string]bool
}

// loadTypedModule type-checks this package, every package of the module it
// depends on, and the runner binary, from source, against the export data
// `go list -export` hands over for everything else.
func loadTypedModule(t *testing.T, fset *token.FileSet) typedModule {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Generous rather than clever: on a cold cache — under -race, where the
	// test binaries' own builds do not serve — this first builds the export
	// data of every dependency.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-export", "-deps",
		"-json=ImportPath,Dir,Export,GoFiles,CgoFiles,Deps,Incomplete,Error", "./pkg/backend/model", "./cmd/iterion")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export did not answer, so no env read was scanned: %v\n%s", err, stderr.String())
	}
	type listed struct {
		ImportPath string
		Dir        string
		Export     string
		GoFiles    []string
		CgoFiles   []string
		Deps       []string
		Incomplete bool
		Error      *struct{ Err string }
	}
	byPath := map[string]listed{}
	exports := map[string]string{}
	mod := typedModule{deps: map[string]map[string]bool{}}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("go list answered %d bytes that did not decode, so the scan stopped mid-tree: %v", len(out), err)
		}
		byPath[p.ImportPath] = p
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
		mod.deps[p.ImportPath] = map[string]bool{}
		for _, d := range p.Deps {
			mod.deps[p.ImportPath][d] = true
		}
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("go list named no export data for %s", path)
		}
		return os.Open(export) // #nosec G304 -- the build cache path go list just named
	})
	check := func(path string) *typedPackage {
		p, ok := byPath[path]
		if !ok {
			t.Fatalf("go list did not list %s, so its env reads cannot be scanned", path)
		}
		if p.Incomplete || p.Error != nil {
			t.Fatalf("go list reports %s incomplete (%v), so its env reads cannot be scanned", path, p.Error)
		}
		if len(p.CgoFiles) > 0 {
			t.Fatalf("%s has cgo files, which this scan does not type-check: teach it, or its env readers go unseen", path)
		}
		// The directory too, not only its files: the test cache then sees a
		// file added to a package go list hands over.
		if _, err := os.ReadDir(p.Dir); err != nil {
			t.Fatalf("read %s: %v", p.Dir, err)
		}
		tp := &typedPackage{path: path, dir: p.Dir, goFiles: p.GoFiles, info: &types.Info{
			Types:      map[ast.Expr]types.TypeAndValue{},
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}}
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			tp.files = append(tp.files, f)
		}
		conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", runtime.GOARCH)}
		var err error
		if tp.types, err = conf.Check(path, fset, tp.files, tp.info); err != nil {
			t.Fatalf("%s does not type-check against the toolchain's export data, so its env reads cannot be scanned: %v", path, err)
		}
		return tp
	}
	for _, path := range sortedKeys(byPath) {
		if path == modelPkgPath || (strings.HasPrefix(path, iterionModule) && mod.deps[modelPkgPath][path]) {
			tp := check(path)
			mod.readers = append(mod.readers, tp)
			if path == modelPkgPath {
				mod.model = tp
			}
		}
	}
	mod.runner = check(runnerPkgPath)
	if mod.model == nil || len(mod.model.files) == 0 {
		t.Fatal("go list named no production source for this package: the scan would pass on nothing")
	}
	return mod
}

// funcLabel names fn as the tables spell it: F or (*T).M in this package,
// the import path (less the module's prefix) and the name anywhere else.
func funcLabel(fn *types.Func) string {
	full := strings.ReplaceAll(fn.Origin().FullName(), modelPkgPath+".", "")
	return strings.ReplaceAll(full, iterionModule, "")
}

// calleeIdent is the identifier naming the function call invokes, if any.
func calleeIdent(call *ast.CallExpr) *ast.Ident {
	fun := ast.Unparen(call.Fun)
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(ix.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(ix.X)
	}
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}

// refIdent is the identifier e names a function or a variable with, if e is
// one.
func refIdent(e ast.Expr) *ast.Ident {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return x
	case *ast.SelectorExpr:
		return x.Sel
	}
	return nil
}

func funcOf(info *types.Info, id *ast.Ident) *types.Func {
	if id == nil {
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

func varOf(info *types.Info, id *ast.Ident) *types.Var {
	if id == nil {
		return nil
	}
	obj := info.Uses[id]
	if obj == nil {
		obj = info.Defs[id]
	}
	v, _ := obj.(*types.Var)
	return v
}

// paramFlow is what a function body does with its parameters and its
// receiver: the ones it writes or takes the address of — passed on after
// that, the name is no longer the caller's — and the locals it computes from
// them.
type paramFlow struct {
	info    *types.Info
	params  map[*types.Var]int
	written map[*types.Var]bool
	derived map[*types.Var]map[int]bool
	// constants: the parameters a caller can hand a constant — a basic or
	// interface type, the elements of a variadic one, a receiver of a basic
	// type. Only those can carry a name decided in code.
	constants map[int]bool
}

func newParamFlow(info *types.Info, recv *ast.FieldList, ft *ast.FuncType, body *ast.BlockStmt) paramFlow {
	f := paramFlow{info: info, params: map[*types.Var]int{}, written: map[*types.Var]bool{}, derived: map[*types.Var]map[int]bool{}, constants: map[int]bool{}}
	if recv != nil {
		for _, field := range recv.List {
			for _, n := range field.Names {
				if v, ok := info.Defs[n].(*types.Var); ok {
					f.params[v] = receiverArg
					_, basic := v.Type().Underlying().(*types.Basic)
					f.constants[receiverArg] = basic
				}
			}
		}
	}
	i := 0
	for _, field := range ft.Params.List {
		for range max(len(field.Names), 1) {
			t := info.TypeOf(field.Type)
			if ell, ok := field.Type.(*ast.Ellipsis); ok {
				t = info.TypeOf(ell.Elt)
			}
			f.constants[i] = canBeConstant(t)
			i++
		}
		for j, n := range field.Names {
			if v, ok := info.Defs[n].(*types.Var); ok {
				f.params[v] = i - len(field.Names) + j
			}
		}
	}
	if body == nil || len(f.params) == 0 {
		return f
	}
	// assigns records lhs as computed from rhs: a parameter is written, a
	// local derives from every parameter rhs is computed from.
	assigns := func(lhs ast.Expr, rhs []ast.Expr) bool {
		id, ok := ast.Unparen(lhs).(*ast.Ident)
		if !ok {
			return false
		}
		v := varOf(info, id)
		if v == nil {
			return false
		}
		if _, isParam := f.params[v]; isParam {
			f.written[v] = true
			return false
		}
		grew := false
		for _, e := range rhs {
			for _, i := range f.mentioned(e) {
				if f.derived[v] == nil {
					f.derived[v] = map[int]bool{}
				}
				if !f.derived[v][i] {
					f.derived[v][i] = true
					grew = true
				}
			}
		}
		return grew
	}
	for grew := true; grew; {
		grew = false
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					rhs := x.Rhs
					if len(x.Lhs) == len(x.Rhs) {
						rhs = x.Rhs[i : i+1]
					}
					grew = assigns(lhs, rhs) || grew
				}
			case *ast.ValueSpec:
				for i, name := range x.Names {
					rhs := x.Values
					if len(x.Names) == len(x.Values) {
						rhs = x.Values[i : i+1]
					}
					grew = assigns(name, rhs) || grew
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if e != nil {
						grew = assigns(e, []ast.Expr{x.X}) || grew
					}
				}
			case *ast.IncDecStmt:
				assigns(x.X, nil)
			case *ast.UnaryExpr:
				if x.Op == token.AND {
					assigns(x.X, nil)
				}
			}
			return true
		})
	}
	return f
}

// passedAs reports which parameter e is, when it is one as passed — through
// a type conversion too, which keeps the value.
func (f paramFlow) passedAs(e ast.Expr) (int, bool) {
	e = ast.Unparen(e)
	if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 && f.info.Types[call.Fun].IsType() {
		e = ast.Unparen(call.Args[0])
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return 0, false
	}
	v, ok := f.info.Uses[id].(*types.Var)
	if !ok || f.written[v] {
		return 0, false
	}
	i, ok := f.params[v]
	return i, ok
}

// mentioned is the parameters e is computed from, through the locals
// computed from them too.
func (f paramFlow) mentioned(e ast.Expr) []int {
	seen := map[int]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if v, ok := f.info.Uses[id].(*types.Var); ok {
				if i, isParam := f.params[v]; isParam {
					seen[i] = true
				}
				for i := range f.derived[v] {
					seen[i] = true
				}
			}
		}
		return true
	})
	out := make([]int, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

var templateRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

func templateRefs(s string) []string {
	var out []string
	for _, m := range templateRef.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// lookupShaped reports whether t is a func(string) string or a
// func(string) (string, bool): the shape of an env reader taken as a value.
func lookupShaped(t types.Type) bool {
	sig, ok := t.Underlying().(*types.Signature)
	if !ok || sig.Params().Len() != 1 || !isString(sig.Params().At(0).Type()) {
		return false
	}
	res := sig.Results()
	switch res.Len() {
	case 1:
		return isString(res.At(0).Type())
	case 2:
		b, ok := res.At(1).Type().Underlying().(*types.Basic)
		return isString(res.At(0).Type()) && ok && b.Kind() == types.Bool
	}
	return false
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func canBeConstant(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Underlying().(type) {
	case *types.Basic, *types.Interface:
		return true
	}
	return false
}

// envReaders: every function known to read the environment, by the argument
// that decides what it reads (receiverArg for a method's receiver), and the
// variables and fields an exact reader is stored in — by declaration, since
// the package that stores one and the package that calls it see two objects
// of it, one type-checked from source, one from export data. consults: the
// readers, and the holders by declaration, that also hand the name to one of
// envStateConsults.
type envReaders struct {
	fset     *token.FileSet
	args     map[string]map[int]envReadKind
	holders  map[string]bool
	consults map[string]bool
}

// consultsRef reports whether e, an env reader taken as a value, is one that
// consults envStateConsults.
func (r envReaders) consultsRef(info *types.Info, e ast.Expr) bool {
	if lit, ok := ast.Unparen(e).(*ast.FuncLit); ok {
		flow := newParamFlow(info, nil, lit.Type, lit.Body)
		found := false
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, rd := range r.readsOf(info, call) {
					if i, ok := flow.passedAs(rd.arg); ok && i == 0 && rd.consults {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	id := refIdent(e)
	if v := varOf(info, id); v != nil {
		return r.consults[r.declKey(v)]
	}
	fn := funcOf(info, id)
	return fn != nil && (r.consults[funcLabel(fn)] || envStateConsults[funcLabel(fn)] != "")
}

func (r envReaders) holds(v *types.Var) bool { return r.holders[r.declKey(v)] }

func (r envReaders) declKey(v *types.Var) string {
	pos := r.fset.Position(v.Pos())
	path := ""
	if v.Pkg() != nil {
		path = v.Pkg().Path()
	}
	return fmt.Sprintf("%s.%s@%s:%d", path, v.Name(), filepath.Base(pos.Filename), pos.Line)
}

func (r envReaders) mark(fn string, idx int, kind envReadKind) bool {
	args := r.args[fn]
	if args == nil {
		args = map[int]envReadKind{}
		r.args[fn] = args
	}
	old, had := args[idx]
	if had && (old == kind || old == opaqueName) {
		return false
	}
	if had {
		kind = opaqueName
	}
	args[idx] = kind
	return true
}

// isReaderRef reports whether e is an env reader taken as a value: a reader
// function, a variable or a field holding one, or a function literal reading
// the variable its parameter names.
func (r envReaders) isReaderRef(info *types.Info, e ast.Expr) bool {
	if lit, ok := ast.Unparen(e).(*ast.FuncLit); ok {
		flow := newParamFlow(info, nil, lit.Type, lit.Body)
		found := false
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, rd := range r.readsOf(info, call) {
					if i, ok := flow.passedAs(rd.arg); ok && i == 0 && rd.kind == exactName {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	id := refIdent(e)
	if v := varOf(info, id); v != nil {
		return r.holds(v)
	}
	fn := funcOf(info, id)
	return fn != nil && r.args[funcLabel(fn)][0] == exactName
}

type envArgRead struct {
	arg      ast.Expr
	kind     envReadKind
	callee   string
	handoff  bool // read because a reader is handed alongside it
	consults bool // the reader also answers from envStateConsults
}

// readsOf is the arguments call reads the environment through.
func (r envReaders) readsOf(info *types.Info, call *ast.CallExpr) []envArgRead {
	id := calleeIdent(call)
	if v := varOf(info, id); v != nil {
		if r.holds(v) && len(call.Args) > 0 {
			return []envArgRead{{arg: call.Args[0], kind: exactName, callee: "the reader held by " + v.Name(), consults: r.consults[r.declKey(v)]}}
		}
		return nil
	}
	fn := funcOf(info, id)
	if fn == nil {
		return nil
	}
	label := funcLabel(fn)
	var out []envArgRead
	// An expander reads what its table entry says, whatever its body makes
	// of a nil lookup.
	if te, ok := templateExpanders[label]; ok {
		if te[1] < len(call.Args) && te[0] < len(call.Args) && r.isReaderRef(info, call.Args[te[1]]) {
			out = append(out, envArgRead{arg: call.Args[te[0]], kind: templateNames, callee: label, consults: r.consultsRef(info, call.Args[te[1]])})
		}
		return out
	}
	sig, _ := fn.Type().(*types.Signature)
	idxs := make([]int, 0, len(r.args[label]))
	for idx := range r.args[label] {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		kind := r.args[label][idx]
		if idx == receiverArg {
			if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
				if s := info.Selections[sel]; s != nil && s.Kind() == types.MethodVal {
					out = append(out, envArgRead{arg: sel.X, kind: kind, callee: label, consults: r.consults[label]})
				}
			}
			continue
		}
		last := idx + 1
		if sig != nil && sig.Variadic() && idx == sig.Params().Len()-1 {
			last = len(call.Args)
		}
		for i := idx; i < last && i < len(call.Args); i++ {
			out = append(out, envArgRead{arg: call.Args[i], kind: kind, callee: label, consults: r.consults[label]})
		}
	}
	if _, classified := getterHandoffs[label]; classified {
		return out
	}
	for i, arg := range call.Args {
		if !r.isReaderRef(info, arg) {
			continue
		}
		for j, other := range call.Args {
			if j != i {
				out = append(out, envArgRead{arg: other, kind: opaqueName, callee: label, handoff: true})
			}
		}
	}
	return out
}

// discover marks the functions of p that read the environment through a
// parameter or their receiver, and the variables and fields p stores an
// exact reader in; it reports whether it marked one it had not.
func (r envReaders) discover(p *typedPackage) bool {
	changed := false
	flag := func(set map[string]bool, key string) {
		if !set[key] {
			set[key] = true
			changed = true
		}
	}
	hold := func(target, value ast.Expr) {
		v := varOf(p.info, refIdent(target))
		if v == nil {
			return
		}
		if r.isReaderRef(p.info, value) {
			flag(r.holders, r.declKey(v))
		}
		if r.consultsRef(p.info, value) {
			flag(r.consults, r.declKey(v))
		}
	}
	for _, f := range p.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if i < len(x.Values) {
						hold(name, x.Values[i])
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i := range x.Lhs {
						hold(x.Lhs[i], x.Rhs[i])
					}
				}
			case *ast.CompositeLit:
				for _, elt := range x.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							hold(key, kv.Value)
						}
					}
				}
			}
			return true
		})
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn, _ := p.info.Defs[fd.Name].(*types.Func)
			flow := newParamFlow(p.info, fd.Recv, fd.Type, fd.Body)
			if fn == nil || len(flow.params) == 0 {
				continue
			}
			label := funcLabel(fn)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// A name this function also hands to a state consult, or
				// reads through a reader that does, is answered from more
				// than the process env.
				fromParam := func(arg ast.Expr) bool {
					_, passed := flow.passedAs(arg)
					return passed || len(flow.mentioned(arg)) > 0
				}
				held := false
				if v := varOf(p.info, calleeIdent(call)); v != nil {
					held = r.consults[r.declKey(v)]
				}
				if callee := funcLabelOf(p.info, calleeIdent(call)); held || envStateConsults[callee] != "" || r.consults[callee] {
					for _, arg := range call.Args {
						if fromParam(arg) {
							flag(r.consults, label)
						}
					}
				}
				for _, rd := range r.readsOf(p.info, call) {
					if rd.consults && !rd.handoff && fromParam(rd.arg) {
						flag(r.consults, label)
					}
					if i, ok := flow.passedAs(rd.arg); ok && !rd.handoff {
						if flow.constants[i] {
							changed = r.mark(label, i, rd.kind) || changed
						}
						continue
					}
					// A template computed from a parameter still reads the
					// references its caller spells; a name computed from one
					// is a name the scan cannot read.
					kind := opaqueName
					if rd.kind == templateNames {
						kind = templateNames
					}
					for _, i := range flow.mentioned(rd.arg) {
						if flow.constants[i] {
							changed = r.mark(label, i, kind) || changed
						}
					}
				}
				return true
			})
		}
	}
	return changed
}

// graphOf is p's call graph: for each function of p (packageLevel for its
// declarations), every function it may invoke — the ones it references, and
// every method declared in pkgs that a value it builds holds.
func graphOf(p *typedPackage, pkgs map[string]bool) map[string]map[*types.Func]bool {
	graph := map[string]map[*types.Func]bool{}
	for _, f := range p.files {
		for _, decl := range f.Decls {
			from := packageLevel
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if fn, ok := p.info.Defs[fd.Name].(*types.Func); ok {
					from = funcLabel(fn)
				}
			}
			if graph[from] == nil {
				graph[from] = map[*types.Func]bool{}
			}
			// A value built here may have any method it holds invoked, by
			// whoever it is handed to, by reflection or JSON too.
			builds := func(t types.Type) {
				for _, m := range methodsOf(t, pkgs, map[types.Type]bool{}) {
					graph[from][m] = true
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if fn, ok := p.info.Uses[x].(*types.Func); ok {
						graph[from][fn] = true
					}
				case *ast.CompositeLit:
					builds(p.info.TypeOf(x))
				case *ast.ValueSpec:
					if x.Type != nil {
						if _, isPointer := p.info.TypeOf(x.Type).(*types.Pointer); !isPointer {
							builds(p.info.TypeOf(x.Type))
						}
					}
				case *ast.CallExpr:
					if tv := p.info.Types[x.Fun]; tv.IsType() && len(x.Args) == 1 && !p.info.Types[x.Args[0]].IsNil() {
						builds(tv.Type)
					} else if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && len(x.Args) == 1 {
						if b, ok := p.info.Uses[id].(*types.Builtin); ok && b.Name() == "new" {
							builds(p.info.TypeOf(x.Args[0]))
						}
					}
				}
				return true
			})
		}
	}
	return graph
}

// methodsOf is the methods declared in pkgs on t and on every type a value of
// t holds — embedded or not, through pointers, slices, arrays and maps.
func methodsOf(t types.Type, pkgs map[string]bool, seen map[types.Type]bool) []*types.Func {
	if t == nil || seen[t] {
		return nil
	}
	seen[t] = true
	switch x := t.(type) {
	case *types.Pointer:
		return methodsOf(x.Elem(), pkgs, seen)
	case *types.Slice:
		return methodsOf(x.Elem(), pkgs, seen)
	case *types.Array:
		return methodsOf(x.Elem(), pkgs, seen)
	case *types.Map:
		return append(methodsOf(x.Key(), pkgs, seen), methodsOf(x.Elem(), pkgs, seen)...)
	case *types.Named:
		var out []*types.Func
		if x.Obj().Pkg() != nil && pkgs[x.Obj().Pkg().Path()] {
			for i := 0; i < x.NumMethods(); i++ {
				out = append(out, x.Method(i))
			}
		}
		return append(out, methodsOf(x.Underlying(), pkgs, seen)...)
	case *types.Struct:
		var out []*types.Func
		for i := 0; i < x.NumFields(); i++ {
			out = append(out, methodsOf(x.Field(i).Type(), pkgs, seen)...)
		}
		return out
	}
	return nil
}

func sortedFuncs(set map[*types.Func]bool) []*types.Func {
	out := make([]*types.Func, 0, len(set))
	for fn := range set {
		out = append(out, fn)
	}
	sort.Slice(out, func(i, j int) bool { return funcLabel(out[i]) < funcLabel(out[j]) })
	return out
}

// runnerReach is what the claw runner reaches of this package, each function
// with the one it is reached from ("" for an entry): runClawRunner and what
// it reaches of the runner binary, everything of this package they use, then
// this package's own call graph — its initialisation included, which runs in
// the runner too, and what the runner binary's own initialisation uses of it
// directly. The walk stops at launcherOnly functions; cut is the ones it
// stopped at.
func runnerReach(mod typedModule, problem func(string, ...any)) (reached map[string]string, cut map[string]bool) {
	pkgs := map[string]bool{modelPkgPath: true, runnerPkgPath: true}
	runnerGraph := graphOf(mod.runner, pkgs)
	// What enters the walk, with what enters it.
	const ownInit, binaryInit = "this package's initialisation", "the runner binary's initialisation"
	entries := map[string]string{packageLevel: ownInit, "init": ownInit}
	enter := func(fn *types.Func, from string) {
		if _, seen := entries[funcLabel(fn)]; !seen && fn.Pkg() != nil && fn.Pkg().Path() == modelPkgPath {
			entries[funcLabel(fn)] = from
		}
	}
	if _, ok := runnerGraph[runnerEntry]; !ok {
		problem("the runner binary declares no %s: the runner's path is unknown, so nothing would count as reached — point runnerEntry at what `iterion __claw-runner` runs", runnerEntry)
	}
	// runClawRunner's path through the binary: a package on it that depends
	// on this one is a problem — the walk does not follow it.
	walked := map[string]bool{runnerEntry: true}
	queue := []string{runnerEntry}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, fn := range sortedFuncs(runnerGraph[from]) {
			if fn.Pkg() == nil {
				continue
			}
			switch path, label := fn.Pkg().Path(), funcLabel(fn); {
			case path == runnerPkgPath:
				if !walked[label] {
					walked[label] = true
					queue = append(queue, label)
				}
			case path == modelPkgPath:
				enter(fn, from)
			case mod.deps[path][modelPkgPath]:
				problem("the runner reaches %s, whose package depends on this one: the walk does not follow it, so what it reaches here is unseen — teach the walk that package", label)
			}
		}
	}
	// The rest of the binary counts by its direct uses of this package: which
	// command a function serves is beyond the scan, and main, the
	// initialisation, a hook set on the runner's command or a seam in front
	// of runClawRunner all run in the container. binaryHostOnly lists the
	// functions reviewed as another command's.
	for _, from := range sortedKeys(runnerGraph) {
		if binaryHostOnly[from] != "" {
			continue
		}
		origin := from
		if from == packageLevel || from == "cmd/iterion.init" {
			origin = binaryInit
		}
		for _, fn := range sortedFuncs(runnerGraph[from]) {
			enter(fn, origin)
		}
	}
	for _, fn := range sortedKeys(binaryHostOnly) {
		switch _, declared := runnerGraph[fn]; {
		case !declared:
			problem("binaryHostOnly lists %s, which the runner binary declares no more: drop the entry", fn)
		case walked[fn]:
			problem("binaryHostOnly lists %s, which runClawRunner's path reaches: it runs in the container — drop the entry", fn)
		}
	}
	graph := graphOf(mod.model, pkgs)
	reached, cut = map[string]string{}, map[string]bool{}
	queue = queue[:0]
	for _, e := range sortedKeys(entries) {
		reached[e] = entries[e]
		queue = append(queue, e)
	}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, fn := range sortedFuncs(graph[from]) {
			if fn.Pkg() == nil || fn.Pkg().Path() != modelPkgPath {
				continue
			}
			to := funcLabel(fn)
			if _, stop := launcherOnly[to]; stop {
				cut[to] = true
				continue
			}
			if _, done := reached[to]; !done {
				reached[to] = from
				queue = append(queue, to)
			}
		}
	}
	return reached, cut
}

// scanPackageEnvReads collects every env name this package reads, by site,
// and what the sandbox runner reaches of it. A read whose name is not a
// constant is a problem, unless the function reads the variable its own
// parameter names (its callers' constants are the reads) or dynamicEnvReads
// classifies the pair; a reader referenced other than by calling it is a
// problem, unless it is handed to a call getterHandoffs classifies or used
// as a template expander's lookup.
func scanPackageEnvReads(t *testing.T) envScan {
	t.Helper()
	fset := token.NewFileSet()
	mod := loadTypedModule(t, fset)
	model, info := mod.model, mod.model.info

	scan := envScan{reads: map[string][]envRead{}, spelled: map[string][]envRead{}, consulted: map[string][]string{}, dynamicUsed: map[[2]string]bool{}, enumerations: map[string][]string{}, handoffs: map[string][]string{}}
	problem := func(format string, args ...any) { scan.problems = append(scan.problems, fmt.Sprintf(format, args...)) }
	at := func(pos token.Pos) string {
		p := fset.Position(pos)
		return fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line)
	}
	read := func(name string, pos token.Pos, where string, consults bool) {
		scan.reads[name] = append(scan.reads[name], envRead{at: at(pos), pos: pos, fn: where})
		if consults {
			scan.consulted[name] = append(scan.consulted[name], at(pos)+" in "+where)
		}
	}

	// The scan sees the files this platform builds: one a build constraint
	// excludes would read the environment unseen.
	built := map[string]bool{}
	for _, name := range model.goFiles {
		built[name] = true
	}
	entries, err := os.ReadDir(model.dir)
	if err != nil {
		t.Fatalf("read %s: %v", model.dir, err)
	}
	for _, e := range entries {
		if name := e.Name(); !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !built[name] {
			problem("%s is excluded from this platform's build, so its env reads cannot be scanned: keep build constraints out of this package, or teach the scan to read every platform", name)
		}
	}

	readers := envReaders{fset: fset, args: map[string]map[int]envReadKind{}, holders: map[string]bool{}, consults: map[string]bool{}}
	for fn, args := range seedEnvReaders {
		for idx, kind := range args {
			readers.mark(fn, idx, kind)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range mod.readers {
			changed = readers.discover(p) || changed
		}
	}
	declared := map[string]bool{}
	for _, p := range mod.readers {
		for _, obj := range p.info.Defs {
			if fn, ok := obj.(*types.Func); ok {
				declared[funcLabel(fn)] = true
			}
		}
	}
	for _, fn := range sortedKeys(envStateConsults) {
		if !declared[fn] {
			problem("envStateConsults lists %s, which no package this one builds on declares: a read that consults its successor goes unseen — point the entry at what answers for a name now", fn)
		}
	}

	// What each variable of this package is assigned — a parameter of an
	// unexported function, what its calls here hand it — so a template held
	// in one is read where it is spelled, and a function value is traced.
	assigned := map[*types.Var][]ast.Expr{}
	for _, f := range model.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if v := varOf(info, name); v != nil && i < len(x.Values) {
						assigned[v] = append(assigned[v], x.Values[i])
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, lhs := range x.Lhs {
						if v := varOf(info, refIdent(lhs)); v != nil {
							assigned[v] = append(assigned[v], x.Rhs[i])
						}
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok {
					if v := varOf(info, key); v != nil {
						assigned[v] = append(assigned[v], x.Value)
					}
				}
			case *ast.CallExpr:
				fn := funcOf(info, calleeIdent(x))
				if fn == nil || fn.Exported() || fn.Pkg() == nil || fn.Pkg().Path() != modelPkgPath {
					return true
				}
				params := fn.Type().(*types.Signature).Params()
				for i, arg := range x.Args {
					if params.Len() > 0 {
						v := params.At(min(i, params.Len()-1))
						assigned[v] = append(assigned[v], arg)
					}
				}
			}
			return true
		})
	}
	// tracedNoReader reports whether every value v is assigned here is a
	// function the scan sees, none of them an env reader.
	tracedNoReader := func(v *types.Var) bool {
		if len(assigned[v]) == 0 {
			return false
		}
		for _, value := range assigned[v] {
			_, lit := ast.Unparen(value).(*ast.FuncLit)
			if (!lit && funcOf(info, refIdent(value)) == nil) || readers.isReaderRef(info, value) {
				return false
			}
		}
		return true
	}
	// templateLiterals reads the references of every string literal e is
	// built from, through the variables it names.
	var templateLiterals func(e ast.Expr, where string, consults bool, seen map[*types.Var]bool)
	templateLiterals = func(e ast.Expr, where string, consults bool, seen map[*types.Var]bool) {
		ast.Inspect(e, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if s, err := strconv.Unquote(x.Value); err == nil {
						for _, name := range templateRefs(s) {
							read(name, x.Pos(), where, consults)
						}
					}
				}
			case *ast.Ident:
				if v := varOf(info, x); v != nil && !seen[v] {
					seen[v] = true
					for _, value := range assigned[v] {
						templateLiterals(value, where, consults, seen)
					}
				}
			}
			return true
		})
	}

	for _, f := range model.files {
		for _, decl := range f.Decls {
			where, flow, exported := packageLevel, paramFlow{info: info}, false
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if fn, _ := info.Defs[fd.Name].(*types.Func); fn != nil {
					where = funcLabel(fn)
				}
			}
			// Every constant spelled like an env name, whatever road it
			// takes; a constant's declaration is spelled where it is used.
			census := func(n ast.Node, label string) {
				ast.Inspect(n, func(n ast.Node) bool {
					if gd, ok := n.(*ast.GenDecl); ok && gd.Tok == token.CONST {
						return false
					}
					e, ok := n.(ast.Expr)
					if !ok {
						return true
					}
					s, ok := constString(info, e)
					if !ok {
						return true
					}
					for _, name := range append([]string{s}, templateRefs(s)...) {
						if m := envNameShape.FindStringSubmatch(name); m != nil {
							scan.spelled[m[1]] = append(scan.spelled[m[1]], envRead{at: at(e.Pos()), pos: e.Pos(), fn: where, decl: label})
						}
					}
					return false
				})
			}
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
						census(vs, "var "+vs.Names[0].Name)
					}
				}
			} else {
				census(decl, where)
			}
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn, _ := info.Defs[fd.Name].(*types.Func)
				if fn == nil {
					t.Fatalf("%s: no type information for %s", at(fd.Pos()), fd.Name.Name)
				}
				where, flow, exported = funcLabel(fn), newParamFlow(info, fd.Recv, fd.Type, fd.Body), fn.Exported()
				if fd.Recv != nil {
					for _, kind := range readers.args[where] {
						if kind == exactName {
							problem("%s: the method %s reads the variable a parameter names, and a call to it through an interface would go unseen: make it a function", at(fd.Pos()), where)
							break
						}
					}
				}
			}
			callTargets := map[*ast.Ident]bool{}
			handedTo := map[*ast.Ident]string{}
			lookups := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id := calleeIdent(call)
				if id != nil {
					callTargets[id] = true
				}
				label := types.ExprString(call.Fun)
				if fn := funcOf(info, id); fn != nil {
					label = funcLabel(fn)
				}
				if envEnumerators[label] {
					scan.enumerations[where] = append(scan.enumerations[where], at(call.Pos()))
				}
				// A lookup this function is handed, from a caller the scan
				// cannot trace, and called with a constant name, may read
				// it: refused, never assumed.
				if v := varOf(info, id); v != nil && !readers.holds(v) && lookupShaped(v.Type()) && len(call.Args) == 1 {
					i, isParam := flow.params[v]
					name, constant := constString(info, call.Args[0])
					if isParam && i >= 0 && constant && (exported || !tracedNoReader(v)) {
						problem("%s: the constant %q goes through the function value %s, which the scan cannot trace: if it reads the environment, read the variable by its name at the call", at(call.Pos()), name, types.ExprString(call.Fun))
					}
				}
				te, expands := templateExpanders[label]
				for i, arg := range call.Args {
					ref := refIdent(arg)
					if ref == nil || (!readers.isReaderRef(info, arg) && !envEnumerators[funcLabelOf(info, ref)]) {
						continue
					}
					switch fn := funcOf(info, id); {
					case expands && i == te[1]:
						lookups[ref] = true
					case fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == modelPkgPath:
						lookups[ref] = true // its reads are refused where they happen
						problem("%s: an env reader handed to %s, a function of this package, which reads names the scan cannot see: read them at the call site", at(arg.Pos()), label)
					default:
						handedTo[ref] = label
						if readers.consultsRef(info, arg) {
							problem("%s: an env reader that also answers from envStateConsults handed to %s: what it reads there is not what crosses — hand it os.Getenv", at(arg.Pos()), label)
						}
					}
				}
				for _, rd := range readers.readsOf(info, call) {
					if rd.handoff {
						continue // the handoff itself is the finding
					}
					switch rd.kind {
					case exactName:
						if name, ok := constString(info, rd.arg); ok {
							read(name, rd.arg.Pos(), where, rd.consults)
							continue
						}
						if i, ok := flow.passedAs(rd.arg); ok && readers.args[where][i] == exactName {
							continue
						}
						if pair := [2]string{where, rd.callee}; dynamicEnvReads[pair] != "" {
							scan.dynamicUsed[pair] = true
							continue
						}
						problem("%s: the env name %s read in %s through %s is not a constant, so its side cannot be checked: spell the name at the call (a literal or a constant); if %s reads the variable its parameter names, pass that parameter on as is (dynamicEnvReads is only for names that are data, never for a knob)",
							at(rd.arg.Pos()), types.ExprString(rd.arg), where, rd.callee, where)
					case templateNames:
						if tpl, ok := constString(info, rd.arg); ok {
							for _, name := range templateRefs(tpl) {
								read(name, rd.arg.Pos(), where, rd.consults)
							}
							continue
						}
						// The literals it is built from, here or in the
						// variables it names, are spelled here. The rest is
						// data.
						templateLiterals(rd.arg, where, rd.consults, map[*types.Var]bool{})
					case opaqueName:
						// A name derived from data — a document, a node, a
						// request — is data. One derived from a constant is
						// a knob the scan cannot name.
						if name, ok := constString(info, rd.arg); info.Types[rd.arg].Value == nil || (ok && name == "") {
							continue
						}
						if pair := [2]string{where, rd.callee}; dynamicEnvReads[pair] != "" {
							scan.dynamicUsed[pair] = true
							continue
						}
						problem("%s: %s derives the env name it reads from the constant %s in a way this scan cannot read, so its side cannot be checked: read the variable by its name at the call",
							at(rd.arg.Pos()), rd.callee, types.ExprString(rd.arg))
					}
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || callTargets[id] || lookups[id] {
					return true
				}
				fn, ok := info.Uses[id].(*types.Func)
				if !ok {
					return true
				}
				label := funcLabel(fn)
				if len(readers.args[label]) == 0 && !envEnumerators[label] {
					return true
				}
				if callee, handed := handedTo[id]; handed {
					scan.handoffs[callee] = append(scan.handoffs[callee], at(id.Pos()))
					return true
				}
				problem("%s: %s referenced other than by calling it: call it at the read, so the name it reads is spelled there", at(id.Pos()), label)
				return true
			})
		}
	}
	// A declaration crossingSpellings lists holds crossing names: a reference
	// to it anywhere else carries them on, so it spells them there.
	held := map[string][]string{}
	for _, name := range sortedKeys(scan.spelled) {
		for _, s := range scan.spelled[name] {
			if strings.HasPrefix(s.decl, "var ") && crossingSpellings[s.decl] != "" {
				held[s.decl] = append(held[s.decl], name)
			}
		}
	}
	for _, f := range model.files {
		for _, decl := range f.Decls {
			where, label := packageLevel, ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				if fn, _ := info.Defs[fd.Name].(*types.Func); fn != nil {
					where, label = funcLabel(fn), funcLabel(fn)
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if vs, ok := n.(*ast.ValueSpec); ok && where == packageLevel && len(vs.Names) > 0 {
					label = "var " + vs.Names[0].Name
				}
				id, ok := n.(*ast.Ident)
				if !ok || crossingSpellings[label] != "" {
					return true
				}
				v, ok := info.Uses[id].(*types.Var)
				if !ok || v.Parent() != model.types.Scope() {
					return true
				}
				for _, name := range held["var "+v.Name()] {
					scan.spelled[name] = append(scan.spelled[name], envRead{at: at(id.Pos()), pos: id.Pos(), fn: where, decl: label})
				}
				return true
			})
		}
	}
	scan.reachedFrom, scan.cut = runnerReach(mod, problem)
	return scan
}

func funcLabelOf(info *types.Info, id *ast.Ident) string {
	if fn := funcOf(info, id); fn != nil {
		return funcLabel(fn)
	}
	return ""
}
