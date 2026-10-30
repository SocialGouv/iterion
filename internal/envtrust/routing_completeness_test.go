package envtrust

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The completeness guard on the routing class. The inventory on
// routingSuffixes/routingPrefixes claims to be DERIVED from the readers in
// this tree, not from one reviewer's list — this test makes the derivation
// hold: it re-walks the sources the tree actually reads environment
// variables in (first-party pkg/, internal/, cmd/, plus the two vendored
// trees that read routing-shaped names — aws-sdk-go-v2's endpoint
// machinery and the claw runtime), and every name the walk finds that
// LOOKS like it decides a route must read as routing-shaped. A reader the
// inventory has no shape for arrives red here, the day it is written, not
// the day a planted `.env` aims it.
//
// The walk sees a literal argument, so a name read through a composed
// constant (aws-sdk's per-service spellings are built with fmt.Sprintf
// inside the SDK; iterion's PublicBaseEnv is a first-party constant) is
// visible only through its literal kin — the service endpoints.go files
// carry the per-service literals, and the floor list below pins the
// composed ones. What the walk cannot see at all, it cannot vouch for:
// that limit is stated on the model-path class guard
// (pkg/backend/model/planted_endpoint_class_test.go) and is why the
// inventory is declarations plus a walk, not a walk alone.
func TestEveryRoutingShapedNameTheTreeReadsIsInTheClass(t *testing.T) {
	root := moduleRoot(t)
	dirs := []string{
		filepath.Join(root, "pkg"),
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
		filepath.Join(root, "vendor", "github.com", "SocialGouv", "claw-code-go"),
		filepath.Join(root, "vendor", "github.com", "aws", "aws-sdk-go-v2", "config"),
		filepath.Join(root, "vendor", "github.com", "getsentry", "sentry-go"),
	}
	var files []string
	for _, dir := range dirs {
		files = append(files, goFilesIn(t, dir)...)
	}
	// The per-service endpoint files: one per vendored AWS service, each
	// reading AWS_ENDPOINT_URL and its own AWS_ENDPOINT_URL_<SERVICE>.
	serviceEndpoints, err := filepath.Glob(filepath.Join(root, "vendor", "github.com", "aws", "aws-sdk-go-v2", "service", "*", "endpoints.go"))
	if err != nil {
		t.Fatalf("glob the service endpoint files: %v", err)
	}
	if len(serviceEndpoints) == 0 {
		t.Fatal("no vendored AWS service endpoints.go found: the walk no longer matches the tree layout")
	}
	files = append(files, serviceEndpoints...)

	found := map[string]bool{}
	for _, path := range files {
		for _, name := range envNamesReadIn(t, path) {
			found[name] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("the walk found no env read at all: it has stopped matching the source, so a green verdict proves nothing")
	}

	// The floor: names the walk must keep seeing, one per family the class
	// was widened for and read through every spelling the walk reaches — a
	// walk that stops finding them has stopped looking, not stopped needing.
	for _, want := range []string{
		"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", // vendored claw providers
		"ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", // the refresh-token wire
		"ITERION_REMOTE_URL",                        // pkg/cli
		"ITERION_MONGO_URI",                         // the _URI spelling
		"ITERION_REDIS_SENTINEL_ADDRS",              // the _ADDRS spelling
		"ITERION_SANDBOX_MODEL_HOSTS",               // the _HOSTS spelling
		"ITERION_WEBHOOK_FORGE_HOSTS",               // the trusted-host list
		"SENTRY_DSN",                                // the _DSN wire (vendored sentry-go)
		"SEARXNG_URL",                               // read FIRST, before SEARXNG_ENDPOINT
		"CLAW_WEB_SEARCH_BRAVE_URL",                 // vendored claw web search
		"AWS_ENDPOINT_URL",                          // the global override
		"AWS_ENDPOINT_URL_BEDROCK_RUNTIME",          // the per-service spelling
		"OTEL_EXPORTER_OTLP_ENDPOINT",               // the vendored exporters
	} {
		if !found[want] {
			t.Fatalf("the walk no longer finds %s (found %d names): either the reader is gone, or the walk stopped matching the read forms the tree uses", want, len(found))
		}
	}

	// The class decision: every name that looks like it decides a route is
	// covered. routeLikeShape is this test's own spelling of "looks like it
	// decides a route" — deliberately a literal here, not the production
	// list, so narrowing the production class cannot shrink the walk to
	// match it.
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if !routeLikeShape(name) {
			continue
		}
		if !RoutingShapedName(name) {
			t.Errorf("%s is read in this tree and looks like it decides a route, but RoutingShapedName does not cover it: a project `.env` would plant it silently — widen the class (routing.go) with the shape and the reader that proves it", name)
		}
	}
}

// routeLikeShape guesses "this name decides where a connection goes, what is
// presented there, or which hosts are trusted" from its spelling alone. The
// shapes are the ones the tree's readers carry; a new spelling belongs here
// the day a reader carries it, where the case above fails and asks for the
// widening with its proof.
func routeLikeShape(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "AWS_ENDPOINT_URL") && len(upper) > len("AWS_ENDPOINT_URL") {
		return true
	}
	for _, suffix := range []string{
		"_URL", "_URI", "_ADDRS", "_HOSTS", "_ENDPOINT", "_HOST", "_BASE_URL", "_API_BASE", "_PROXY", "_DSN",
	} {
		if len(upper) > len(suffix) && strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// envNamesReadIn parses one Go file and returns the environment variable
// names it reads with a LITERAL argument, through the read forms the tree
// and its vendored dependencies use: os.Getenv / os.LookupEnv, envtrust's
// own Inherited / Planted, and the plain-name lookup helpers (pkg/config's
// lookup and lookupString, pkg/secrets' envOr, the desktop's readEnv).
// Files whose bytes name no read form are skipped unparsed.
func envNamesReadIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytesNameAReadForm(raw) {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, raw, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var names []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && len(call.Args) > 0 {
			lit, ok := call.Args[0].(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				if envReadForm(call.Fun) {
					if name, err := strconv.Unquote(lit.Value); err == nil && name != "" {
						names = append(names, name)
					}
				}
			}
		}
		return true
	})
	return names
}

// bytesNameAReadForm is the cheap prefilter that keeps the walk off the
// files that cannot contain a read.
func bytesNameAReadForm(raw []byte) bool {
	for _, form := range []string{"Getenv(", "LookupEnv(", ".Inherited(", ".Planted(", "lookupString(", "lookup(", "envOr(", "readEnv("} {
		if strings.Contains(string(raw), form) {
			return true
		}
	}
	return false
}

// envReadForm reports whether a call expression is one of the read forms.
func envReadForm(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch {
		case pkg.Name == "os" && (e.Sel.Name == "Getenv" || e.Sel.Name == "LookupEnv"):
			return true
		case pkg.Name == "envtrust" && (e.Sel.Name == "Inherited" || e.Sel.Name == "Planted"):
			return true
		}
		return false
	case *ast.Ident:
		switch e.Name {
		case "lookup", "lookupString", "envOr", "readEnv":
			return true
		}
		return false
	}
	return false
}

// moduleRoot walks up to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test: the walk does not know where the module starts")
		}
		dir = parent
	}
}

// goFilesIn lists the non-test Go files under dir, skipping vendor trees
// other than the ones the caller asked for by path.
func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}
