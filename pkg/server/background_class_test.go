package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// detachedSpawn is one census entry: how many goroutines a function starts,
// and what joins them — or the reason nothing has to.
type detachedSpawn struct {
	spawns int
	what   string
}

// spawnCensus is the complete census of the goroutines this package starts,
// keyed by "<file>:<function>". A goroutine that writes to a store and
// outlives the request that spawned it belongs in the shutdown registry
// (registerBackgroundWorker, reached through goUntilShutdown /
// tryGoUntilShutdown / goSafe), so the process cannot exit while its last
// write is in flight: an audit row, a last_used_at stamp or a refresh claim
// cut by the exit has no sweep behind it to reconcile it.
//
// The sequence this census serves is documented in
// docs/probes-and-graceful-shutdown.md ("The background join"), which also
// records the one documented exception below: the run-console websockets do
// not watch the shutdown signal.
//
// Everything else is listed here with what joins it instead. The predicate is
// the SPAWN — a `go` statement, a call to anything named Go (errtrack.Go, an
// errgroup's Go), a function handed to a timer — because that is what a reader
// can decide from the source; whether the body writes to a store is the
// judgement each entry below records. A new spawn fails this test until it
// either joins or says why it does not have to.
var spawnCensus = map[string]detachedSpawn{
	"admin_orgs_routes.go:(*Server).buildOrgUsageView":          {1, "a read-only per-team count fan-out, reduced after g.Wait() inside the same call: the request joins it"},
	"assistant_mission.go:(*Server).restartAssistantMissions":   {1, "the assistant-mission sweep loop; its done channel reaches the join as joinBackgroundWorkers' `server.assistantMissions` extra, because the shutdown cancels the coordinator by hand first and the handle lives on it"},
	"assistant_run_watch.go:(*Server).startAssistantRunWatches": {1, "the assistant run-watch sweep loop, joined as the `server.assistantWatch` extra, same shape as the mission coordinator above"},
	"background.go:(*Server).tryGoUntilShutdown":                {2, "the registry itself: one watcher that cancels the loop's context on the shutdown signal (it waits on two channels and writes nothing), and the registered loop, whose done channel is what the join waits for"},
	"boarddispatch.go:(*boardDispatcher).tick":                  {1, "one board card's processing, held by d.wg: the dispatcher's drain closes s.boardDispDone and Shutdown waits on it BEFORE the join, so a write one of those cards asks for on its way out is still accepted"},
	"gosafe.go:(*Server).tryGoSafe":                             {1, "the detached best-effort write helper behind goSafe — registered through registerBackgroundWorker, so the shutdown joins every audit insert, MarkUsed, invitation email and model-spec refresh its call sites spawn"},
	"health.go:(*Server).handleReadyz":                          {2, "one readiness Ping per dependency plus the bounded wait that collects them; the handler gives up on a hung driver rather than hold the kubelet, so a probe can outlive its request — it pings and writes nothing"},
	"hub.go:(*Hub).HandleWebSocket":                             {2, "the read and write pumps of one websocket client; they end with the socket, Hub.Stop releases them, and they only fan broadcast frames out"},
	"projects.go:(*Server).swapWorkDir":                         {1, "the file watcher of the newly swapped workdir; Watcher.Stop ends it and fsnotify events become hub broadcasts, never store writes"},
	"runs_browser.go:(*Server).handleBrowserCDP":                {3, "the two directions of one CDP pipe bridge, plus the standing watch of the credential it was opened with; all three end when the websocket or the pipe closes (the watch on the same stop channel), and none writes to a store — the watch only READS the account its credential names"},
	"runs_files.go:combinedFiles":                               {1, "a parallel git scan the same call reads back off its channel before returning: read-only, and joined by the request"},
	"runs_shell.go:(*shellConn).run":                            {2, "one shell session's PTY pump and idle watchdog, released by the done channel this function closes when the socket dies; no store write"},
	"runs_ws.go:(*Server).handleRunWebSocket":                   {1, "one run-console websocket connection's own goroutine. A connection lives as long as its CLIENT keeps it open — a span no shutdown budget can bound — so it is not a candidate for the join; it is listed here so a store write added to the WS command path is read against that fact"},
	"runs_ws.go:(*runConn).handleSubscribe":                     {1, "the event pump of one subscription; it forwards envelopes and ends with the connection above"},
	"runs_ws.go:(*runConn).run":                                 {3, "the write pump of one connection, its lifetime watchdog, and the standing watchdog of the passive stream (ADR-148): all three end with the connection above (the watchdogs on the closed channel, or by closing the socket itself once the cap is up), and none writes to a store"},
	"runs_ws_logs.go:(*runConn).handleSubscribeLogs":            {1, "the log pump of one subscription; it ends with the connection above"},
	"server.go:New": {2, "the hub's fan-out loop (ended by Hub.Stop) and the local file watcher (ended by Watcher.Stop); neither writes to a store"},
	"server_lifecycle.go:(*Server).ListenAndServe":               {2, "the cloud board dispatcher arm and the cancel watcher inside it: the outer goroutine closes s.boardDispDone, which Shutdown waits on before the join; the inner one cancels on the shutdown signal"},
	"trigger_cloud.go:StartCloudTriggerCoordinator":              {1, "the cloud board poll loop; cloudBoardSource.Stop cancels it and waits on its done channel, and the shutdown calls it (StopSource) before the bus cancels"},
	"trigger_coordinator.go:StartTriggerCoordinator":             {1, "the effects outbox worker ticker; TriggerCoordinator.stopEffects cancels it and the shutdown waits on effectsDone before the bus cancels"},
	"watch_coordinator.go:startWatchCoordinator":                 {1, "the watch fan-out worker; watchCoordinator.Close waits on its done channel, and the shutdown calls it before the bus cancels"},
	"watcher.go:(*Watcher).Start":                                {1, "the ignore-cache GC of the file watcher, bounded by the watcher's own done channel; it prunes an in-memory map"},
	"watcher.go:(*Watcher).handleEvent":                          {1, "the debounce timer of one file event: it broadcasts the last event for that path to the hub, and writes to no store"},
	"workspace_handoff.go:(*WorkspaceHost).startHandoffDelivery": {1, "the workspace host's handoff delivery loop; WorkspaceHost.Shutdown cancels it and waits on handoffWG — the host has its own lifecycle, separate from the API server's"},
}

func TestEveryGoroutineThisPackageStartsIsJoinedOrSaysWhyNot(t *testing.T) {
	found, err := goroutineSpawnCensus(".")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry, listed := spawnCensus[k]
		if !listed {
			t.Errorf("%s starts %d goroutine(s) nothing in this census accounts for: a goroutine that writes to a store and outlives its request is registered through registerBackgroundWorker (goUntilShutdown / tryGoUntilShutdown / goSafe) so the shutdown joins its last write; one that must stay detached is listed in spawnCensus with what ends it and why the join is not owed",
				k, found[k])
			continue
		}
		if entry.spawns != found[k] {
			t.Errorf("%s starts %d goroutine(s), the census says %d — a spawn was added to a function whose entry covers the others (%s): re-read the new one against the join and update the count",
				k, found[k], entry.spawns, entry.what)
		}
	}
	var stale []string
	for k, entry := range spawnCensus {
		if strings.TrimSpace(entry.what) == "" {
			t.Errorf("%s is listed with no reason: an entry nobody can check is worse than no entry", k)
		}
		if _, ok := found[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("%s no longer starts a goroutine: remove its census entry", k)
	}
}

// goroutineSpawnCensus counts, per "<file>:<function>", the goroutines the
// package's non-test sources start.
func goroutineSpawnCensus(dir string) (map[string]int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	fset := token.NewFileSet()
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil, perr
		}
		for _, imp := range f.Imports {
			if imp.Name != nil && imp.Name.Name == "." {
				return nil, fmt.Errorf("%s dot-imports %s: a spawn spelled without its package hides from this census", name, imp.Path.Value)
			}
		}
		for _, decl := range f.Decls {
			key := name + ":" + spawnDeclKey(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if spawnsAGoroutine(n) {
					out[key]++
				}
				return true
			})
		}
	}
	return out, nil
}

// spawnsAGoroutine reports whether n is one of the ways this package starts a
// goroutine: a `go` statement, a reference to a function named Go or TryGo
// (errtrack.Go, an errgroup's Go and its bounded sibling), or a function handed
// to something that will run it on its own goroutine (time.AfterFunc,
// context.AfterFunc).
//
// The VERB decides, not the package: AfterFunc spawns whoever exports it, and
// an aliased import (`t "time"`) would hide from a check on the qualifier.
// `Tick` is the one spelling still pinned to `time`, because a method named
// Tick that computes rather than spawns exists in this repo
// (cloudsched.Ticker.Tick) and a census that cries wolf is one nobody updates.
func spawnsAGoroutine(n ast.Node) bool {
	switch x := n.(type) {
	case *ast.GoStmt:
		return true
	case *ast.SelectorExpr:
		switch x.Sel.Name {
		case "Go", "TryGo", "AfterFunc":
			return true
		}
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "time" {
			return x.Sel.Name == "Tick"
		}
	}
	return false
}

// declKey names a declaration the way a reader greps for it: the function, or
// the first name of a package-level var whose initialiser holds the spawn.
func spawnDeclKey(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		return fmt.Sprintf("(%s).%s", receiverType(d.Recv.List[0].Type), d.Name.Name)
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return "var " + vs.Names[0].Name
			}
		}
	}
	return "<declaration>"
}

func receiverType(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return "*" + receiverType(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return receiverType(x.X)
	}
	return "?"
}

// The predicate finds a spawn however it is spelled, and does not invent one.
func TestTheSpawnPredicateFindsEverySpelling(t *testing.T) {
	spawning := map[string]string{
		"go statement":      "package x\nfunc f() { go g() }\n",
		"go func literal":   "package x\nfunc f() { go func() {}() }\n",
		"errtrack.Go":       "package x\nfunc f() { errtrack.Go(\"n\", g) }\n",
		"aliased tracker":   "package x\nfunc f() { et.Go(\"n\", g) }\n",
		"Go as a value":     "package x\nvar spawn = errtrack.Go\n",
		"errgroup member":   "package x\nfunc f() { g.Go(func() error { return nil }) }\n",
		"errgroup TryGo":    "package x\nfunc f() { g.TryGo(func() error { return nil }) }\n",
		"timer AfterFunc":   "package x\nfunc f() { time.AfterFunc(d, g) }\n",
		"aliased timer":     "package x\nimport t \"time\"\nfunc f() { t.AfterFunc(d, g) }\n",
		"context AfterFunc": "package x\nfunc f() { context.AfterFunc(ctx, g) }\n",
		"package-level var": "package x\nvar watch = func() { go g() }\n",
	}
	for name, src := range spawning {
		got, err := censusOfSource(t, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != 1 {
			t.Errorf("%s: counted %d spawns, want 1 — a spawn spelled this way escapes the census", name, got)
		}
	}
	quiet := map[string]string{
		"a plain call":            "package x\nfunc f() { g() }\n",
		"prose naming a spawn":    "package x\n// go func() { errtrack.Go(\"n\", g) }\nfunc f() { g() }\n",
		"a method merely named G": "package x\nfunc f() { x.Goal() }\n",
		"a ticker":                "package x\nfunc f() { t := time.NewTicker(d); t.Stop() }\n",
		// cloudsched.Ticker.Tick computes a tick, it does not spawn one:
		// `Tick` counts for the `time` package and nothing else.
		"a Tick that computes": "package x\nfunc f() { _, _ = tk.Tick(ctx) }\n",
	}
	for name, src := range quiet {
		got, err := censusOfSource(t, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != 0 {
			t.Errorf("%s: counted %d spawns, want 0 — a census that cries wolf is a census nobody updates", name, got)
		}
	}
	if _, err := censusOfSource(t, "package x\nimport . \"other/pkg\"\nfunc f() { Go(\"n\", g) }\n"); err == nil {
		t.Error("a dot import went unreported: a spawn spelled without its package would be invisible")
	}
}

// A refused goSafe never runs fn, so anything fn is responsible for releasing
// must be acquired INSIDE fn. The shape that breaks it is a `defer x()` on a
// name the literal did not declare: a cancel, an unlock or a semaphore slot
// taken before the call, which the refusal then strands for the remaining life
// of the process. goSafe reports the refusal to no caller and tryGoSafe reports
// only that it happened, so this is checked at the call sites.
func TestNoDetachedWriteReleasesWhatItDidNotAcquire(t *testing.T) {
	strays, err := straysInDetachedWrites(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range strays {
		t.Errorf("%s %s", s.where, s.what)
	}
}

type strayRelease struct {
	where string
	what  string
}

// detachedWriteHelpers are the spellings of "spawn a best-effort write the
// shutdown joins": a task any of them is handed may be dropped, so every one of
// their call sites owes the acquire-inside-fn rule.
var detachedWriteHelpers = map[string]bool{"goSafe": true, "tryGoSafe": true}

// straysInDetachedWrites finds, per detached-write call site, the deferred
// releases of something the task literal does not own — and the sites whose
// task is not a literal at all, whose body this guard cannot read.
func straysInDetachedWrites(dir string) ([]strayRelease, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []strayRelease
	fset := token.NewFileSet()
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil, perr
		}
		for _, decl := range f.Decls {
			scope := detachedWriteScope{}
			if fd, isFunc := decl.(*ast.FuncDecl); isFunc {
				scope = scope.extend(fd.Type.Params, fd.Body)
			}
			scanDetachedWrites(name, fset, decl, scope, &out)
		}
		out = append(out, helpersTakenAsValues(name, fset, f)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].where < out[j].where })
	return out, nil
}

// helpersTakenAsValues reports the detached-write helpers a file takes as a
// VALUE instead of calling — `spawn := s.goSafe`. The task then reaches the
// helper through a name, so the literal that defers the releases sits at a
// call this guard does not recognise as one and every release it strands is
// invisible. Refused rather than modelled: following the value is a question
// about aliasing, and calling the helper where the literal is written costs
// nothing.
func helpersTakenAsValues(file string, fset *token.FileSet, f *ast.File) []strayRelease {
	called := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, isCall := n.(*ast.CallExpr); isCall {
			called[call.Fun] = true
		}
		return true
	})
	var out []strayRelease
	ast.Inspect(f, func(n ast.Node) bool {
		sel, isSelector := n.(*ast.SelectorExpr)
		if !isSelector || !detachedWriteHelpers[sel.Sel.Name] || called[sel] {
			return true
		}
		out = append(out, strayRelease{
			where: fmt.Sprintf("%s:%d", file, fset.Position(sel.Pos()).Line),
			what: fmt.Sprintf("takes %s as a value instead of calling it: the acquire-inside-fn rule is checked at call sites, and a helper reached through a name has none — call it where the task literal is written",
				sel.Sel.Name),
		})
		return true
	})
	return out
}

// scanDetachedWrites reports the strays of the call sites written at ONE
// function level, then recurses into the literals nested in it with their own
// parameters added to the scope — so a task forwarded from a nested closure is
// read against the signature that was handed it, not against the top-level
// declaration's.
func scanDetachedWrites(file string, fset *token.FileSet, root ast.Node, scope detachedWriteScope, out *[]strayRelease) {
	var nested []*ast.FuncLit
	ast.Inspect(root, func(n ast.Node) bool {
		if lit, isLit := n.(*ast.FuncLit); isLit && ast.Node(lit) != root {
			nested = append(nested, lit)
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !detachedWriteHelpers[sel.Sel.Name] || len(call.Args) == 0 {
			return true
		}
		where := fmt.Sprintf("%s:%d", file, fset.Position(call.Pos()).Line)
		task := call.Args[len(call.Args)-1]
		lit, isLit := task.(*ast.FuncLit)
		if !isLit {
			// A task this guard cannot read: a variable holding a closure
			// carries exactly the stranded release below. The one exception is
			// a function FORWARDING the task it was handed (goSafe →
			// tryGoSafe): it closes over nothing of its own, and the literal
			// lives at its caller's site, where this guard reads it.
			if id, isIdent := task.(*ast.Ident); isIdent && scope.forwards(id.Name) {
				return true
			}
			*out = append(*out, strayRelease{
				where: where,
				what:  fmt.Sprintf("hands %s a task that is not a function literal: this guard cannot read its defers, so pass a literal (it may call the named function) and keep every release it owns visible", sel.Sel.Name),
			})
			return true
		}
		declared := declaredNames(lit)
		for _, rel := range deferredReleases(lit) {
			// Owned means introduced EARLIER: a definition is in scope only
			// after its own statement, so a release deferred ahead of the
			// task's first definition of that name releases the binding the
			// task was handed from outside. `ctx, cancel :=` further down a
			// task that already defers a `cancel` from its caller is the
			// commonest spelling of exactly the stranding checked here, and a
			// flat set of names reads it as ownership.
			if at, own := declared[rel.name]; !own || at > rel.at {
				*out = append(*out, strayRelease{
					where: where,
					what:  fmt.Sprintf("%s, which the detached task does not own there — it declares that name later, or not at all: a refused %s never runs fn, so that release never happens — acquire it inside fn, ahead of the defer", rel.spelt, sel.Sel.Name),
				})
			}
		}
		return true
	})
	for _, lit := range nested {
		scanDetachedWrites(file, fset, lit, scope.extend(lit.Type.Params, lit.Body), out)
	}
}

// detachedWriteScope is what a task handed over as a plain NAME is read
// against: the parameters the enclosing functions were handed, and the names
// those functions define themselves.
type detachedWriteScope struct {
	params  map[string]bool
	defined map[string]bool
}

// extend adds one function's parameters and whatever its body defines.
func (s detachedWriteScope) extend(params *ast.FieldList, body *ast.BlockStmt) detachedWriteScope {
	next := detachedWriteScope{params: copyNames(s.params), defined: copyNames(s.defined)}
	for n := range fieldNames(params) {
		next.params[n] = true
	}
	for n := range definedNames(body) {
		next.defined[n] = true
	}
	// A parameter cannot be redeclared with `:=` in the function's own
	// top-level block, so `fn = ...` is the one spelling available to a
	// wrapper that swaps the task it was handed for a closure of its own —
	// and an assignment DEFINES nothing, so the definitions above do not see
	// it. Without this the exemption waves through the natural shape and
	// keeps only the rarer shadowing one.
	for n := range reboundNames(body) {
		next.defined[n] = true
	}
	return next
}

// forwards reports whether name is a task one of the enclosing functions was
// HANDED, the one shape whose literal lives at a caller this guard also reads.
// A function that also DEFINES that name loses the exemption: which binding
// reached the call is then a question this guard cannot answer from the names
// alone, and a closure built locally — whose defers nobody reads — is exactly
// what it must not wave through.
func (s detachedWriteScope) forwards(name string) bool {
	return s.params[name] && !s.defined[name]
}

// declaredNames collects every name a function literal introduces itself —
// parameters, `:=` definitions and var declarations, at any depth — each with
// the position it is in scope FROM, so a release deferred ahead of it can be
// told from one deferred after it.
func declaredNames(lit *ast.FuncLit) map[string]token.Pos {
	names := fieldNames(lit.Type.Params)
	for n, at := range definedNames(lit.Body) {
		noteFirst(names, n, at)
	}
	return names
}

// fieldNames names the entries of a parameter list, each at its own position —
// which precedes every statement of the body, as a parameter's scope does.
func fieldNames(list *ast.FieldList) map[string]token.Pos {
	names := map[string]token.Pos{}
	if list == nil {
		return names
	}
	for _, field := range list.List {
		for _, id := range field.Names {
			noteFirst(names, id.Name, id.Pos())
		}
	}
	return names
}

// definedNames collects the names a body defines itself, at any depth: a `:=`
// or a var declaration anywhere under it, each at the END of the statement
// that defines it — which is where its scope starts. The parameter is typed
// rather than an ast.Node so that a bodiless declaration's nil is seen as one.
func definedNames(body *ast.BlockStmt) map[string]token.Pos {
	names := map[string]token.Pos{}
	if body == nil {
		return names
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					noteFirst(names, id.Name, x.End())
				}
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				noteFirst(names, id.Name, x.End())
			}
		}
		return true
	})
	return names
}

// reboundNames collects the names a body ASSIGNS to, at any depth. An
// assignment introduces nothing — so these are never names a task OWNS — but
// they are names whose binding at the call is no longer the one the signature
// shows, which is what the forwarding exemption turns on.
func reboundNames(body *ast.BlockStmt) map[string]token.Pos {
	names := map[string]token.Pos{}
	if body == nil {
		return names
	}
	ast.Inspect(body, func(node ast.Node) bool {
		assign, isAssign := node.(*ast.AssignStmt)
		if !isAssign || assign.Tok != token.ASSIGN {
			return true
		}
		for _, lhs := range assign.Lhs {
			if id, isIdent := lhs.(*ast.Ident); isIdent {
				noteFirst(names, id.Name, assign.End())
			}
		}
		return true
	})
	return names
}

// noteFirst records the EARLIEST position a name is introduced at. One name
// introduced twice in a literal — two sibling scopes each taking their own
// timer context, both calling it `cancel` — is owned from the first of them
// on, and keeping the later position instead would report the task's own
// release as a stranded one.
func noteFirst(names map[string]token.Pos, name string, at token.Pos) {
	if prev, seen := names[name]; seen && prev <= at {
		return
	}
	names[name] = at
}

func copyNames(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k := range in {
		out[k] = true
	}
	return out
}

// goBuiltins are the predeclared functions a deferred call reaches for to
// finish the task's own work — `recover()` at the top of a panic guard, the
// `append`/`copy` of a last reduction. `close` is the one that RELEASES, and
// its subject is the channel in its argument rather than the builtin's name.
var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// deferredRelease is what one deferred call would leave unreleased if the task
// were dropped: the name of the resource, and how the site spells releasing it
// — a finding has to name the resource, because that is what its author moves
// inside fn.
type deferredRelease struct {
	name  string
	spelt string
	// ofArgument marks `close(ch)`, whose subject is its ARGUMENT: that is the
	// one release spelled as a call on something other than itself, so it is
	// also the one a rule reading callees gets backwards in both directions.
	ofArgument bool
	// at is where the defer STATEMENT sits. Ownership is positional: the task
	// owns what it declared ahead of the defer, and nothing else.
	at token.Pos
}

// releaseSubject reads what a call releases: `close(ch)` releases ch, any other
// predeclared builtin releases nothing, and a call on a plain name releases
// that name (`cancel()`, `unlock()`). Nothing else is this shape — a selector
// (`wg.Done()`, `s.publish()`) is a method on a value whose lifetime the task
// does not own, and a channel reached through a FIELD (`close(f.done)`) is
// shared state installed before the ask, whose dropped-task answer is the
// tryGoSafe verdict (models_routes.go) and not the acquire-inside-fn rule.
func releaseSubject(call *ast.CallExpr) (deferredRelease, bool) {
	callee, ok := call.Fun.(*ast.Ident)
	if !ok {
		return deferredRelease{}, false
	}
	if callee.Name == "close" {
		if len(call.Args) != 1 {
			return deferredRelease{}, false
		}
		id, isIdent := call.Args[0].(*ast.Ident)
		if !isIdent {
			return deferredRelease{}, false
		}
		return deferredRelease{name: id.Name, spelt: fmt.Sprintf("closes %s", id.Name), ofArgument: true}, true
	}
	if goBuiltins[callee.Name] {
		return deferredRelease{}, false
	}
	return deferredRelease{name: callee.Name, spelt: fmt.Sprintf("defers %s()", callee.Name)}, true
}

// deferredReleases collects what the literal's defer statements release: the
// calls deferred directly, and — inside a deferred closure — the ARGUMENT-FREE
// ones it makes in its own body, `close` excepted. `defer func() { cancel() }()`
// strands exactly what `defer cancel()` strands, and that is the spelling Go
// reaches for as soon as the defer does anything else too; inside such a
// closure the task's own finishing work sits next to its releases, so only a
// call that takes no argument is read as one — except `close(ch)`, which takes
// its subject as its argument.
func deferredReleases(lit *ast.FuncLit) []deferredRelease {
	var out []deferredRelease
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		switch fun := d.Call.Fun.(type) {
		case *ast.Ident:
			if rel, isRelease := releaseSubject(d.Call); isRelease {
				rel.at = d.Pos()
				out = append(out, rel)
			}
		case *ast.FuncLit:
			ast.Inspect(fun.Body, func(inner ast.Node) bool {
				call, isCall := inner.(*ast.CallExpr)
				if !isCall {
					return true
				}
				rel, isRelease := releaseSubject(call)
				if !isRelease || (len(call.Args) > 0 && !rel.ofArgument) {
					return true
				}
				// The position of the CALL, not of the defer: a closure
				// that takes its own deadline declares it after the defer
				// that carries it, and reading ownership at the defer would
				// report the closure's own release as a stranded one.
				rel.at = call.Pos()
				out = append(out, rel)
				return true
			})
		}
		return true
	})
	return out
}

// The release guard reads every spelling of a release, and does not invent one.
func TestTheStrayReleaseGuardReadsEverySpellingOfARelease(t *testing.T) {
	stranding := map[string]string{
		"a bare deferred release": `package x
func f() {
	ctx, cancel := context.WithTimeout(p, d)
	s.goSafe("l", func() {
		defer cancel()
		use(ctx)
	})
}
`,
		"a release inside a deferred closure": `package x
func f() {
	ctx, cancel := context.WithTimeout(p, d)
	s.goSafe("l", func() {
		defer func() { cancel() }()
		use(ctx)
	})
}
`,
		"a task this guard cannot read": `package x
func f() {
	_, cancel := context.WithTimeout(p, d)
	fn := func() { defer cancel() }
	s.goSafe("l", fn)
}
`,
		"the same stranding through tryGoSafe": `package x
func f() {
	ctx, cancel := context.WithTimeout(p, d)
	if !s.tryGoSafe("l", func() {
		defer func() { cancel() }()
		use(ctx)
	}) {
		cancel()
	}
}
`,
		"a closure the function built under the name of its own parameter": `package x
func (s *Server) wrap(label string, fn func()) {
	if label != "" {
		_, cancel := context.WithTimeout(p, d)
		fn := func() { defer cancel() }
		s.goSafe(label, fn)
	}
	_ = fn
}
`,
		"a channel the task did not make, closed from a deferred closure": `package x
func f() {
	done := make(chan struct{})
	s.goSafe("l", func() {
		defer func() { close(done) }()
		work()
	})
	go waitOn(done)
}
`,
		"a bare deferred release that takes the thing it releases": `package x
func f() {
	slot := acquire()
	s.goSafe("l", func() {
		defer releaseSlot(slot)
		work()
	})
}
`,
		// `fn = ...` is the ONLY rebinding Go allows at a function's own top
		// level: `fn := ...` there is "no new variables on left side of :=".
		// The wrapper that builds a closure of its own under the name of the
		// task it was handed therefore spells it this way, and an assignment
		// defines nothing.
		"a parameter the wrapper rebound with a plain assignment": `package x
func (s *Server) auditWithDeadline(label string, fn func()) {
	if s.auditStore != nil {
		_, cancel := context.WithTimeout(p, d)
		fn = func() {
			defer cancel()
			work()
		}
	}
	s.goSafe(label, fn)
}
`,
		// The forge settle's shape with its two halves named alike: the first
		// defer releases what the handler acquired BEFORE the ask, and the
		// task's own later definition of that name is not retroactive.
		"a release from outside, masked by the task's own reuse of the name": `package x
func (s *Server) settle(r *http.Request) {
	ctx, cancel := context.WithTimeout(p, d)
	s.goSafe("l", func() {
		defer cancel()
		first(ctx)
		ctx, cancel := context.WithTimeout(p, d)
		defer cancel()
		second(ctx)
	})
}
`,
		"a release from outside, masked by a nested literal's own definition": `package x
func f() {
	ctx, cancel := context.WithTimeout(p, d)
	s.goSafe("l", func() {
		defer cancel()
		go func() {
			cancel := watch()
			_ = cancel
		}()
		insert(ctx)
	})
}
`,
		"a helper taken as a value": `package x
func f() {
	_, cancel := context.WithTimeout(p, d)
	spawn := s.goSafe
	spawn("l", func() {
		defer cancel()
		insert()
	})
}
`,
	}
	for name, src := range stranding {
		got, err := straysInSource(t, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: found %d strays, want 1 — a dropped task strands a release spelled this way and this guard does not see it: %v", name, len(got), got)
		}
	}
	quiet := map[string]string{
		"a release the task acquired itself": `package x
func f() {
	s.goSafe("l", func() {
		ctx, cancel := context.WithTimeout(p, d)
		defer cancel()
		use(ctx)
	})
}
`,
		"a deferred panic guard": `package x
func f() {
	s.goSafe("l", func() {
		defer func() {
			if r := recover(); r != nil {
				report(r)
			}
		}()
		work()
	})
}
`,
		"a helper forwarding the task it was handed": `package x
func (s *Server) goSafe(label string, fn func()) {
	_ = s.tryGoSafe(label, fn)
}
`,
		"a channel the task closes and a method on a value it owns": `package x
func f() {
	s.goSafe("l", func() {
		done := make(chan struct{})
		defer func() { close(done) }()
		resp, _ := get()
		defer resp.Body.Close()
		use(done)
	})
}
`,
		"a channel the task made, closed bare": `package x
func f() {
	s.goSafe("l", func() {
		done := make(chan struct{})
		defer close(done)
		use(done)
	})
}
`,
		"a literal forwarding the task IT was handed": `package x
func (s *Server) outer() {
	run := func(fn func()) { s.goSafe("l", fn) }
	run(func() { work() })
}
`,
		"a literal forwarding the task its function was handed": `package x
func (s *Server) outer(fn func()) {
	s.each(func(i int) { s.goSafe("l", fn) })
}
`,
		// A declaration with no body at all: this guard walks whatever the
		// package declares, and must read a missing body as defining nothing
		// rather than walking it.
		"a bodiless declaration": `package x

//go:noescape
func elsewhere(label string, fn func())
`,
		// A deferred closure doing one last write of its own: it declares
		// its deadline INSIDE itself, after the defer that carries it, so
		// ownership has to be read where the release is WRITTEN rather than
		// where the closure is deferred.
		"a deferred closure that takes its own deadline": `package x
func f() {
	s.goSafe("l", func() {
		defer func() {
			bg, cancel := context.WithTimeout(p, d)
			defer cancel()
			finish(bg)
		}()
		work()
	})
}
`,
		// Rebinding costs the exemption only for the TASK's name: a wrapper
		// that defaults a label and forwards the closure it was handed is the
		// shape this exemption exists for.
		"a wrapper that rebinds something other than the task": `package x
func (s *Server) wrap(label string, fn func()) {
	if label == "" {
		label = "anon"
	}
	s.goSafe(label, fn)
}
`,
		// Both contexts are the task's own, under one name. Ownership keeps
		// the FIRST position a name is introduced at: read as the last, the
		// task's own first release would be reported as a stranded one — a
		// guard that cries wolf on the spelling the tree is most likely to
		// use next.
		"two sibling scopes of a task, each taking its own context under the same name": `package x
func f() {
	s.goSafe("l", func() {
		ctx, cancel := context.WithTimeout(p, d)
		defer cancel()
		first(ctx)
		if cond {
			ctx, cancel := context.WithTimeout(p, d)
			defer cancel()
			second(ctx)
		}
	})
}
`,
	}
	for name, src := range quiet {
		got, err := straysInSource(t, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: found %d strays, want 0 — a guard that cries wolf is a guard nobody keeps: %v", name, len(got), got)
		}
	}

	// A finding names the RESOURCE a dropped task leaves unreleased, because
	// that is what the author has to go and move inside fn. `close(done)`
	// releases the channel in its argument, not the builtin it is spelled
	// with, and a finding pointing at `close` names something no literal can
	// ever declare.
	closedFromOutside, err := straysInSource(t, `package x
func f() {
	done := make(chan struct{})
	s.goSafe("l", func() {
		defer close(done)
		work()
	})
	go waitOn(done)
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(closedFromOutside) != 1 {
		t.Fatalf("a channel made outside the task and closed inside it: found %d strays, want 1 — a dropped task parks every waiter on it: %v", len(closedFromOutside), closedFromOutside)
	}
	if what := closedFromOutside[0].what; !strings.Contains(what, "done") || strings.Contains(what, "close()") {
		t.Errorf("the finding must name the channel the dropped task leaves open, not the builtin that closes it: %q", what)
	}
}

// straysInSource runs the release guard over one source file written to a temp
// dir, through the same entry point the package-wide check uses.
func straysInSource(t *testing.T, src string) ([]strayRelease, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return straysInDetachedWrites(dir)
}

// censusOfSource runs the census over one source file written to a temp dir,
// through the same entry point the package census uses.
func censusOfSource(t *testing.T, src string) (int, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := goroutineSpawnCensus(dir)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, n := range found {
		total += n
	}
	return total, nil
}
