package orgsweep

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Item 14 of #2401: an org purge left seven collections of a deleted team's
// data behind. The measurement that opened this file found thirty — every
// collection added since the tables were written, because adding one to the
// tree and adding it here are two different gestures and only the first is
// required to ship.
//
// The regrowable class is the gap itself, so the repair is this gate rather
// than one longer list: every Mongo collection the tree declares is
// CLASSIFIED here — swept by tenant, swept by org, swept by a filter of its
// own, removed by the identity cascade, or deliberately left with the reason
// why. A collection that appears with none of those fails, naming itself.
//
// An under-purge is the safe direction (every filter matches on the team's or
// org's own id, so a wrong field can only leave data, never cross tenants) —
// which is exactly why nothing ever failed and the gap grew in silence.

// collectionConstant matches a Go constant or variable whose NAME says it
// holds a collection name, assigned a snake_case string. "Column" is excluded:
// the pipeline board's column constants read the same way and are not
// collections.
var collectionConstant = regexp.MustCompile(`(?m)^\s*(?:var|const)?\s*((?:[A-Za-z_][A-Za-z0-9_]*)?(?:[Cc]ol|[Cc]ollection)[A-Za-z0-9_]*)\s*=\s*"([a-z][a-z0-9_]*)"`)

// collectionCall matches a .Collection(x) call: the site that PROVES a name is
// a collection. x is a literal, a local constant, or another package's.
var collectionCall = regexp.MustCompile(`\.Collection\(\s*([A-Za-z_][A-Za-z0-9_.]*|"[a-z][a-z0-9_]*")\s*\)`)

var lineComments = regexp.MustCompile(`(?m)//.*$`)

// scanTree reads every non-test .go file under pkg/ and returns, per package
// directory, the collection-name constants it declares, plus every
// .Collection() argument it passes.
func scanTree(t *testing.T) (consts map[string]map[string]string, calls map[string][]string) {
	t.Helper()
	root := filepath.Join("..", "..")
	consts = map[string]map[string]string{}
	calls = map[string][]string{}
	files := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		code := lineComments.ReplaceAllString(string(raw), "")
		dir := filepath.Dir(path)
		for _, m := range collectionConstant.FindAllStringSubmatch(code, -1) {
			if strings.Contains(m[1], "olumn") {
				continue // a board COLUMN name reads the same and is not a collection
			}
			if consts[dir] == nil {
				consts[dir] = map[string]string{}
			}
			consts[dir][m[1]] = m[2]
		}
		for _, m := range collectionCall.FindAllStringSubmatch(code, -1) {
			calls[dir] = append(calls[dir], m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the package tree: %v", err)
	}
	if files < 500 {
		t.Fatalf("read %d non-test .go file(s) under %s — the scan went blind and every collection below would read as classified", files, root)
	}
	return consts, calls
}

// resolveCollectionName turns one .Collection() argument into the collection
// it names: a literal is itself, a bare identifier is resolved in the calling
// package, and a qualified one (pkg.Name) in the package of that name. It
// returns "" when the argument is a value this scan cannot read — a runtime
// name, a struct field — which the caller reports rather than skips.
func resolveCollectionName(arg, dir string, consts map[string]map[string]string) string {
	if strings.HasPrefix(arg, `"`) {
		return strings.Trim(arg, `"`)
	}
	if i := strings.LastIndex(arg, "."); i >= 0 {
		pkg, name := arg[:i], arg[i+1:]
		// A selector may itself be qualified (a.b.Name): the package is the
		// last segment before the name.
		if j := strings.LastIndex(pkg, "."); j >= 0 {
			pkg = pkg[j+1:]
		}
		var hits []string
		for d, names := range consts {
			if filepath.Base(d) != pkg {
				continue
			}
			if v, ok := names[name]; ok {
				hits = append(hits, v)
			}
		}
		sort.Strings(hits)
		if len(hits) == 1 {
			return hits[0]
		}
		return ""
	}
	if v, ok := consts[dir][arg]; ok {
		return v
	}
	return ""
}

// passThroughCollectionPackages hold a .Collection() whose name arrives as a
// PARAMETER or a struct field: the collection is decided by the caller, which
// names it with a constant this scan reads there. Declared per package so a
// NEW unreadable site elsewhere still fails.
var passThroughCollectionPackages = map[string]string{
	"platformcfg": "generic record store: the collection is a parameter of its constructor",
	"credpool":    "four stores share one implementation, each holding its collection in a field",
	"orgsweep":    "the purge loops themselves: the name comes from the tables in this file",
	"connection":  "the cloud twin takes the collection as a constructor parameter; production passes ConnectionsCollectionName",
}

// classifyCollection returns the table that governs a collection, or "" when
// nothing does — the state this gate refuses.
func classifyCollection(name string) string {
	for _, c := range teamScopedCollections {
		if c == name {
			return "team-scoped"
		}
	}
	for _, c := range orgScopedCollections {
		if c == name {
			return "org-scoped"
		}
	}
	if r, ok := purgedByBespokeFilter[name]; ok {
		return "bespoke filter: " + r
	}
	if r, ok := purgedByCascade[name]; ok {
		return "identity cascade: " + r
	}
	if r, ok := notPurged[name]; ok {
		return "left on purpose: " + r
	}
	return ""
}

func TestEveryMongoCollectionIsClassifiedByThePurge(t *testing.T) {
	consts, calls := scanTree(t)
	declared := map[string]string{} // collection → where the scan saw it
	for dir, names := range consts {
		for ident, coll := range names {
			declared[coll] = dir + ":" + ident
		}
	}
	// Every .Collection() argument must resolve: a name this scan cannot read
	// is a collection it cannot classify, and a silent skip there is the hole
	// this gate exists to close.
	resolvedCalls := 0
	for dir, args := range calls {
		for _, arg := range args {
			name := resolveCollectionName(arg, dir, consts)
			if name == "" {
				if reason, ok := passThroughCollectionPackages[filepath.Base(dir)]; ok {
					_ = reason
					continue
				}
				t.Errorf("%s: .Collection(%s) names a collection this guard cannot resolve — give the name a package-level constant whose identifier says \"collection\" (or declare the package in passThroughCollectionPackages), so the purge tables can be held to it", dir, arg)
				continue
			}
			resolvedCalls++
			if _, ok := declared[name]; !ok {
				declared[name] = dir + ":" + arg
			}
		}
	}
	if resolvedCalls < 40 {
		t.Fatalf("resolved %d .Collection() call site(s) — too few for this gate to witness anything", resolvedCalls)
	}
	if len(declared) < 60 {
		t.Fatalf("the scan declared %d collection(s) — it collapsed, so the classification below proves nothing", len(declared))
	}
	names := make([]string, 0, len(declared))
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if classifyCollection(name) == "" {
			t.Errorf("collection %q (%s) is in no purge table — an org purge leaves its rows behind. Add it to teamScopedCollections / orgScopedCollections, purge it with its own filter and name it in purgedByBespokeFilter, or declare in notPurged why a deleted org keeps it", name, declared[name])
		}
	}
}

// TestNoPurgeTableNamesACollectionTheTreeDropped reads the tables in the other
// direction: a name no .Collection() site and no constant mentions any more is
// a row that makes the sweep look wider than it is.
func TestNoPurgeTableNamesACollectionTheTreeDropped(t *testing.T) {
	consts, calls := scanTree(t)
	live := map[string]bool{}
	for _, names := range consts {
		for _, coll := range names {
			live[coll] = true
		}
	}
	for dir, args := range calls {
		for _, arg := range args {
			if name := resolveCollectionName(arg, dir, consts); name != "" {
				live[name] = true
			}
		}
	}
	tables := []struct {
		what  string
		names []string
	}{
		{"teamScopedCollections", teamScopedCollections},
		{"orgScopedCollections", orgScopedCollections},
		{"purgedByBespokeFilter", keysOf(purgedByBespokeFilter)},
		{"purgedByCascade", keysOf(purgedByCascade)},
		{"notPurged", keysOf(notPurged)},
	}
	for _, table := range tables {
		for _, name := range table.names {
			if !live[name] {
				t.Errorf("%s names %q and nothing in the tree declares that collection any more — drop the row rather than leave the sweep claiming a reach it does not have", table.what, name)
			}
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
