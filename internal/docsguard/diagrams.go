package docsguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/internal/mdcode"
)

// A drawing nothing checks is a drawing that describes the schema of some
// past commit, and a reader cannot tell which.
//
// COLLECTIONS. Every collection a call site in pkg/'s production Go files
// resolves to, or a dynamic-site row declares, must be an entity of some ER
// block, and every ER entity must
// be one of those or a store outside Mongo (the marker prefixes). The names are read by AST, not
// by grep: a collection is whatever a `*.Collection(x)` call resolves to, and
// the spellings this tree gives the constant (`col*`, `*CollectionName`,
// `*Collection`, a bare `CollectionName`, `ColRoutingPolicies`) are a
// convention the rule must not depend on — the next spelling appears the day
// someone writes it, and a pattern over NAMES would not see it.
//
// A site whose argument is not resolvable statically — a loop variable, a
// function parameter, a struct field — reaches collections no reader of this
// file can enumerate. Those sites are named in a Spec by file and function,
// with the names they reach, so the set stays finite and auditable: a dynamic
// site in a function no row names fails until someone reads the code and
// writes down what it touches, while a further one in a function a row names
// rides that row.
//
// GATES. The gates of the Spec's table are held
// from both ends: the symbol must still be declared in the file the table
// names (a rename reddens, rather than leaving the page describing a function
// that no longer exists), and the page's source must still name it — anywhere
// in the file, an HTML comment included (a gate dropped
// from the page leaves a reader believing the ladder has one rung fewer).
//
// Both halves refuse a tree the guard can no longer read: a corpus that
// yields nothing is reported as a broken guard, never as a clean tree.

// modulePrefix maps an import path of this module onto a directory of this
// tree, which is how a `pkg.Const` argument is resolved to the package that
// declares it.
const modulePrefix = "github.com/SocialGouv/iterion/"

// diagramsDir holds the pages this guard reads. The ER pages are globbed, so
// the model may be split across as many pages as it needs.
const diagramsDir = "docs/diagrams"

const erPageGlob = "data-model-*.md"

const accessControlPage = "access-control.md"

// entityMarkers are the prefixes that declare an ER entity to be a store
// OUTSIDE the Mongo database, which is why no collection answers for it.
var entityMarkers = []string{"S3:", "FS:", "NATS KV:", "external:"}

// Resolution is how a collection site's argument was read.
type Resolution string

const (
	// ByIdent — `db.Collection(colRuns)`, a constant of the same package.
	// Inside a function declaration, a local or parameter that shadows the
	// constant's name is a Dynamic site, never the constant: the walker has no
	// scope analysis beyond that.
	ByIdent Resolution = "ident"
	// BySelector — `db.Collection(secrets.OAuthCollectionName)`, a constant
	// of an imported package of this module.
	BySelector Resolution = "selector"
	// ByLiteral — `db.Collection("runs")`.
	ByLiteral Resolution = "literal"
	// Dynamic — anything else. The names reached are not in the expression.
	Dynamic Resolution = "dynamic"
)

// Site is one `*.Collection(x)` call: where it is, and how its argument read.
type Site struct {
	File string // slash-separated, relative to the root
	Func string // the enclosing function's name — a method's without its receiver, so same-named methods of one file share a row
	How  Resolution
}

// ID identifies a site the way a Spec row names it: file and function, never
// a line number, so the row survives the prose moving around it.
func (s Site) ID() string { return s.File + ":" + s.Func }

// DynamicRow allows the dynamic sites of one function and declares what they
// reach.
//
// Evidence is the reading that produced Names — the slice, the caller, the
// field the argument comes from. It is part of the row because the next
// reader has to be able to re-verify the list without re-deriving it.
type DynamicRow struct {
	Site     string
	Names    []string
	Evidence string
}

// DynamicAllowlist is the set of dynamic sites the tree tolerates.
type DynamicAllowlist []DynamicRow

// GateRef is one authorization gate: the symbol, and the file that declares it.
type GateRef struct {
	Symbol string
	File   string
}

// SliceRef names a top-level `var X = []string{…}` whose every element must be
// a string literal naming a collection a `.Collection(` call site resolves to.
type SliceRef struct {
	File string
	Var  string
}

// Spec is what the guard holds the tree to.
type Spec struct {
	// Dynamic allows the sites whose argument cannot be resolved.
	Dynamic DynamicAllowlist
	// Gates must be declared where the table says and named in the page's
	// source.
	Gates []GateRef
	// Swept are the collection-name slices whose every element must be a string
	// literal a `.Collection(` call site resolves to.
	Swept []SliceRef
}

// IterionSpec is this tree's own spec.
//
// THE DYNAMIC SITES. Three calls name a collection the expression does not
// hold. Each row's names were read from the code it sits in, and each is
// re-verifiable from the Evidence line without re-deriving it.
//
// THE GATES. Symbols of the access-control page, each with the file
// that declares it today.
//
// THE SWEPT LISTS. The purger enumerates the collections it empties for a
// departing org; a name on those lists that no store owns is swept for
// nothing.
func IterionSpec() Spec {
	return Spec{
		Dynamic: DynamicAllowlist{{
			Site: "pkg/cloud/orgsweep/sweep.go:deleteMany",
			Evidence: "the purger's one Collection() call takes `coll`, a parameter. PurgeOrg passes every element of " +
				"teamScopedCollections and orgScopedCollections, plus secrets.OAuthCollectionName, " +
				"platformcfg.ColRoutingPolicies, \"api_keys\" and \"org_usage\" directly.",
			Names: []string{
				"runs", "run_seq", "events", "interactions", "user_messages",
				"memory_docs", "memory_spaces", "memory_tenant_usage",
				"forge_connections", "forge_oauth_apps", "repo_integrations",
				"api_keys", "generic_secrets", "run_secrets", "bot_secret_bindings",
				"org_sso_providers", "org_verified_domains",
				"trigger_subscriptions",
				"webhook_configs", "webhook_deliveries", "webhook_quotas",
				"board_issues", "board_config", "board_events",
				"audit_events",
				"oauth_credentials", "llm_routing_policies", "org_usage",
			},
		}, {
			Site: "pkg/platformcfg/store.go:NewMongoScoped",
			Evidence: "the collection is a parameter. Its only non-test caller, pkg/server/server.go, passes " +
				"platformcfg.ColRoutingPolicies.",
			Names: []string{"llm_routing_policies"},
		}, {
			Site: "pkg/credpool/mongo.go:EnsureSchema",
			Evidence: "the index builder loops over a slice of {col, model} specs and calls db.Collection(s.col); " +
				"the four cols are the package's own colPools/colPledges/colLeases/colLedger constants.",
			Names: []string{"cred_pools", "cred_pledges", "cred_leases", "cred_pool_usage"},
		}},
		Gates: []GateRef{
			{"canViewTeam", "pkg/server/auth_authz.go"},
			{"canEditConfigShares", "pkg/server/auth_authz.go"},
			{"canEditBots", "pkg/server/auth_authz.go"},
			{"canManageTeam", "pkg/server/auth_authz.go"},
			{"canViewOrg", "pkg/server/auth_authz.go"},
			{"canManageOrg", "pkg/server/auth_authz.go"},
			{"requireOrgStandingToInvite", "pkg/server/auth_authz.go"},
			{"requireSuperAdmin", "pkg/server/middleware.go"},
			{"canMutateApiKey", "pkg/server/byok_routes.go"},
			{"canMutateGenericSecret", "pkg/server/generic_secrets_routes.go"},
			{"canMutateScopedRecord", "pkg/server/generic_secrets_routes.go"},
			{"requireMemoryWriteAuth", "pkg/server/memory_routes.go"},
			{"canRouteToConverseBot", "pkg/server/webhooks_gitlab.go"},
			{"scopeRunByID", "pkg/server/run_scope.go"},
			{"identityInTeam", "pkg/server/run_scope.go"},
			{"runScopeLadder", "pkg/server/run_scope.go"},
			{"orgAdminOfTeam", "pkg/server/auth_authz.go"},
			{"gateLaunch", "pkg/server/launch_gate.go"},
			{"launchExemptPrincipal", "pkg/server/launch_gate.go"},
			{"requirePlatformApiKey", "pkg/server/admin_llm_routes.go"},
			// The admission choke points and the #2363 rung: every row the
			// access page explains, not only the can* predicates.
			{"requireAuth", "pkg/server/middleware.go"},
			{"stampAuthedContext", "pkg/server/middleware.go"},
			{"isSuperAdmin", "pkg/server/middleware.go"},
			{"originGateAllows", "pkg/server/middleware.go"},
			{"ghostTeamMutatingRefusal", "pkg/server/middleware.go"},
			{"requireSafeOrigin", "pkg/server/server_files.go"},
			{"handlePutOrgMember", "pkg/server/team_lifecycle_routes.go"},
			{"marketplaceModerateGate", "pkg/server/marketplace_cloud.go"},
			{"effectiveMembership", "pkg/auth/service.go"},
			{"launchRungCleared", "pkg/server/launch_gate.go"},
			{"machineLaunchCtx", "pkg/server/launch_gate.go"},
			{"writeRungDenial", "pkg/server/launch_gate.go"},
			{"mutatingRungAuth", "pkg/server/board_cloud_routes.go"},
			{"mutatingRungDenial", "pkg/server/board_cloud_routes.go"},
			{"boardWriteAuth", "pkg/server/board_cloud_routes.go"},
			{"boardWriteDenial", "pkg/server/board_cloud_routes.go"},
			{"triggerWriteAuth", "pkg/server/triggers_routes.go"},
			{"triggerWriteDenial", "pkg/server/triggers_routes.go"},
			{"runWSMutatingTypes", "pkg/server/runs_ws.go"},
			{"HasRole", "pkg/auth/identity.go"},
		},
		Swept: []SliceRef{
			{"pkg/cloud/orgsweep/sweep.go", "teamScopedCollections"},
			{"pkg/cloud/orgsweep/sweep.go", "orgScopedCollections"},
		},
	}
}

// ProblemKind classifies a refusal, so a test can assert on the refusal it
// drives rather than on the wording of a message.
type ProblemKind string

const (
	// UnmappedCollection — the code reaches a collection no ER block draws.
	UnmappedCollection ProblemKind = "collection no ER block draws"
	// UnknownEntity — an ER block draws an entity nothing answers for.
	UnknownEntity ProblemKind = "ER entity no collection answers for"
	// UnreadERLine — an ER block line in none of the forms the guard reads.
	// Mermaid also takes word cardinalities and names these forms do not,
	// and an entity drawn only on such a line would go unchecked.
	UnreadERLine ProblemKind = "ER line in no form the guard reads"
	// UnlistedDynamicSite — a site whose argument is not resolvable and
	// which no Spec row names.
	UnlistedDynamicSite ProblemKind = "dynamic collection site no row names"
	// StaleDynamicRow — a row naming a site that is not dynamic any more.
	StaleDynamicRow ProblemKind = "row naming a site that no longer exists"
	// UndeclaredGate — a gate symbol absent from the file the table names.
	UndeclaredGate ProblemKind = "gate symbol its file no longer declares"
	// UndrawnGate — a gate symbol the access-control page's source does not name.
	UndrawnGate ProblemKind = "gate symbol the access-control page omits"
	// UnknownSweptCollection — a swept element the guard cannot vouch for: a
	// name no `.Collection(` call site resolves to (a typo, a moved
	// collection, or one a store opens only through a dynamic site), or an
	// element that is not a string literal.
	UnknownSweptCollection ProblemKind = "swept element no call site resolves, or not a literal"
	// SemicolonInStateLabel — a stateDiagram line (not a `%%` comment)
	// holding a `;`. After a label's colon — a transition's, a state
	// description's, a note's — mermaid ends the statement there and draws
	// the rest of the line as stray states, and the parse-only mermaid check
	// passes nearly every such line.
	SemicolonInStateLabel ProblemKind = "stateDiagram line holding a semicolon"
	// MissingCorpus — a page or a declaration the guard reads is not there,
	// so whatever it was meant to check was not checked.
	MissingCorpus ProblemKind = "corpus the guard reads is missing"
)

// Problem is one refusal: what kind, about what, and what to do.
type Problem struct {
	Kind    ProblemKind
	Subject string
	Detail  string
}

func (p Problem) String() string {
	s := string(p.Kind) + ": " + p.Subject
	if p.Detail != "" {
		s += " — " + p.Detail
	}
	return s
}

// Report is what one run read, and what it refuses.
type Report struct {
	// Collections are the names resolved from the code, deduplicated.
	Collections []string
	// Resolutions counts the resolved sites by how their argument read.
	Resolutions map[Resolution]int
	// Sites maps a collection name to the sites that name it.
	Sites map[string][]Site
	// DynamicSites are the sites whose argument did not resolve.
	DynamicSites []Site
	// Allowlisted are the names contributed only by a Spec row.
	Allowlisted []string
	// Entities are the ER entity names the pages draw, deduplicated.
	Entities []string
	// ERPages are the pages whose fences were read.
	ERPages []string
	// Problems is one entry per refusal, ordered by kind then subject.
	Problems []Problem
}

// Known is every collection name the tree accounts for: resolved from the
// code, or declared by a Spec row for a site that cannot be read.
func (r Report) Known() []string {
	return sortedUnion(r.Collections, r.Allowlisted)
}

// Subjects lists, sorted, the subjects of the problems of one kind — the list
// a failing test prints, and the list the page authors work from.
func (r Report) Subjects(kind ProblemKind) []string {
	var out []string
	for _, p := range r.Problems {
		if p.Kind == kind {
			out = append(out, p.Subject)
		}
	}
	sort.Strings(out)
	return out
}

// KindsSeen lists, sorted, the kinds this report refuses on. A test asserting
// a mutant moves ITS verdict and no other compares against this.
func (r Report) KindsSeen() []ProblemKind {
	seen := map[ProblemKind]bool{}
	for _, p := range r.Problems {
		seen[p.Kind] = true
	}
	out := make([]ProblemKind, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Lines renders the problems of one kind, one per line, for a test message.
func (r Report) Lines(kinds ...ProblemKind) string {
	want := map[ProblemKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []string
	for _, p := range r.Problems {
		if len(kinds) == 0 || want[p.Kind] {
			out = append(out, p.String())
		}
	}
	return strings.Join(out, "\n  ")
}

// CheckDiagrams reads the Go packages under root/pkg and the diagram pages
// under root/docs/diagrams, and reports the disagreements it reads between
// them.
//
// An error is returned only when the tree cannot be read at all — a missing
// pkg/ directory, a Go file that does not parse. A missing PAGE is a problem,
// not an error: the authors need the list of names the pages still owe, and
// an error would hide it.
func CheckDiagrams(root string, spec Spec) (Report, error) {
	code, err := readCollectionCode(root)
	if err != nil {
		return Report{}, err
	}
	rep := Report{
		Resolutions: map[Resolution]int{},
		Sites:       map[string][]Site{},
	}

	allowed := map[string]DynamicRow{}
	for _, row := range spec.Dynamic {
		allowed[row.Site] = row
	}
	reached := map[string]bool{}  // every site a row names, to spot a stale row
	resolved := map[string]bool{} // names read from the code

	for _, site := range code.sites {
		if site.Site.How == Dynamic {
			rep.DynamicSites = append(rep.DynamicSites, site.Site)
			row, ok := allowed[site.Site.ID()]
			if !ok {
				rep.Problems = append(rep.Problems, Problem{
					Kind:    UnlistedDynamicSite,
					Subject: site.Site.ID(),
					Detail: "its argument (" + site.Expr + ") resolves to no package constant: read the code, then add a row to the Spec " +
						"listing the collections it reaches",
				})
				continue
			}
			reached[row.Site] = true
			continue
		}
		rep.Resolutions[site.Site.How]++
		resolved[site.Name] = true
		rep.Sites[site.Name] = append(rep.Sites[site.Name], site.Site)
	}
	rep.Collections = append(rep.Collections, sortedKeys(resolved)...)
	for _, sites := range rep.Sites {
		sort.Slice(sites, func(i, j int) bool { return sites[i].ID() < sites[j].ID() })
	}
	sort.Slice(rep.DynamicSites, func(i, j int) bool { return rep.DynamicSites[i].ID() < rep.DynamicSites[j].ID() })

	// A row for a site that is not dynamic any more — refactored, renamed,
	// or made resolvable — is fiction, and fiction in this table is how a
	// collection stops being drawn without anything noticing.
	known := map[string]bool{}
	for name := range resolved {
		known[name] = true
	}
	for _, row := range spec.Dynamic {
		if !reached[row.Site] {
			rep.Problems = append(rep.Problems, Problem{
				Kind:    StaleDynamicRow,
				Subject: row.Site,
				Detail:  "no dynamic collection site sits there any more: remove the row, or point it at the site that moved",
			})
		}
		for _, name := range row.Names {
			if !known[name] {
				rep.Allowlisted = append(rep.Allowlisted, name)
			}
			known[name] = true
		}
	}
	rep.Allowlisted = sortedUnion(rep.Allowlisted, nil)

	entities, pages, problems := readEntities(root)
	rep.Problems = append(rep.Problems, readStateLabels(root)...)
	rep.Entities, rep.ERPages = entities, pages
	rep.Problems = append(rep.Problems, problems...)

	drawn := map[string]bool{}
	for _, e := range entities {
		drawn[e] = true
	}
	for _, name := range sortedKeys(known) {
		if !drawn[name] {
			rep.Problems = append(rep.Problems, Problem{
				Kind:    UnmappedCollection,
				Subject: name,
				Detail:  "no erDiagram block under " + diagramsDir + "/" + erPageGlob + " has it as an entity",
			})
		}
	}
	for _, entity := range entities {
		if known[entity] || hasMarker(entity) {
			continue
		}
		detail := "it is no collection this tree reaches: rename it, or mark the store it lives in (" +
			strings.Join(entityMarkers, " ") + ")"
		if n := nearest(entity, known); n != "" {
			detail += " — nearest known name: " + n
		}
		rep.Problems = append(rep.Problems, Problem{Kind: UnknownEntity, Subject: entity, Detail: detail})
	}

	rep.Problems = append(rep.Problems, checkGates(root, spec.Gates)...)
	rep.Problems = append(rep.Problems, checkSwept(code, spec.Swept, resolved)...)

	sort.SliceStable(rep.Problems, func(i, j int) bool {
		if rep.Problems[i].Kind != rep.Problems[j].Kind {
			return rep.Problems[i].Kind < rep.Problems[j].Kind
		}
		return rep.Problems[i].Subject < rep.Problems[j].Subject
	})
	return rep, nil
}

// checkGates holds each gate from both ends: declared where the table says,
// and named in the source of the page that explains it.
func checkGates(root string, gates []GateRef) []Problem {
	if len(gates) == 0 {
		return nil
	}
	page := filepath.Join(diagramsDir, accessControlPage)
	// The page is read RAW. Every gate symbol on it sits in a code span, and
	// masking code — what the link checker wants — would blank exactly the
	// bytes this check is looking for.
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(page)))
	var problems []Problem
	if err != nil {
		var names []string
		for _, g := range gates {
			names = append(names, g.Symbol)
		}
		sort.Strings(names)
		problems = append(problems, Problem{
			Kind:    MissingCorpus,
			Subject: page,
			Detail: "the page the gate table points at is unreadable (" + err.Error() + "), so no gate is explained anywhere: " +
				strings.Join(names, " "),
		})
	}
	text := string(body)

	// One parse per FILE, and one refusal per unreadable file rather than one
	// per row pointing at it: several rows share pkg/server/auth_authz.go, and a
	// table whose file moved should say so once.
	parsed := map[string]map[string]bool{}
	for _, g := range gates {
		if _, seen := parsed[g.File]; seen {
			continue
		}
		decls, err := declaredNames(filepath.Join(root, filepath.FromSlash(g.File)))
		parsed[g.File] = decls
		if err != nil {
			problems = append(problems, Problem{
				Kind:    MissingCorpus,
				Subject: g.File,
				Detail:  "the file the gate table names cannot be read (" + err.Error() + "): fix the table to the truth",
			})
		}
	}

	for _, g := range gates {
		decls := parsed[g.File]
		if decls == nil {
			continue // already refused, once, above
		}
		if !decls[g.Symbol] {
			problems = append(problems, Problem{
				Kind:    UndeclaredGate,
				Subject: g.Symbol,
				Detail: g.File + " declares no such symbol: it was renamed or moved, and " + page +
					" now explains a gate that does not exist",
			})
		}
		// A missing page leaves text empty, so every gate is reported undrawn
		// as well as the page reported missing: the refusal above says WHY,
		// and these say what the page owes.
		if !namesSymbol(text, g.Symbol) {
			problems = append(problems, Problem{
				Kind:    UndrawnGate,
				Subject: g.Symbol,
				Detail:  page + " never names it: a rung of the authorization ladder the page leaves out",
			})
		}
	}
	return problems
}

// checkSwept holds the purger's collection lists to the call sites: a name it
// sweeps that no `*.Collection(x)` call site resolves to is either a typo or a
// collection that has moved. A dynamic-site row vouches for no swept name —
// the purger's own row lists exactly what it sweeps — so a collection opened
// only dynamically is refused here too. An element that is not a string
// literal is refused: the guard cannot read it as a name, and skipping it
// would certify a list it never read.
func checkSwept(code *collectionCode, refs []SliceRef, resolved map[string]bool) []Problem {
	var problems []Problem
	for _, ref := range refs {
		names, ok := code.slices[ref.File][ref.Var]
		if !ok {
			problems = append(problems, Problem{
				Kind:    MissingCorpus,
				Subject: ref.File + ":" + ref.Var,
				Detail:  "no top-level `var " + ref.Var + " = []string{…}` there: the list this guard checks was renamed or moved",
			})
			continue
		}
		for _, name := range names {
			if expr, ok := strings.CutPrefix(name, nonLiteralElement); ok {
				problems = append(problems, Problem{
					Kind:    UnknownSweptCollection,
					Subject: ref.File + ":" + ref.Var,
					Detail:  "an element that is not a string literal (" + expr + "): the guard reads a swept name only as a literal",
				})
				continue
			}
			if resolved[name] {
				continue
			}
			detail := "swept by " + ref.File + ":" + ref.Var + ", and no `.Collection(` call site resolves to that collection"
			if n := nearest(name, resolved); n != "" {
				detail += " — nearest: " + n
			}
			problems = append(problems, Problem{Kind: UnknownSweptCollection, Subject: name, Detail: detail})
		}
	}
	return problems
}

// namesSymbol reports whether the page names the symbol as a WHOLE word.
//
// A plain substring would let `canViewTeamMembers` answer for `canViewTeam`:
// the page would then explain a gate nobody asked about and the one that was
// dropped would never be missed.
func namesSymbol(text, symbol string) bool {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(symbol) + `([^A-Za-z0-9_]|$)`)
	return re.MatchString(text)
}

func hasMarker(entity string) bool {
	for _, m := range entityMarkers {
		if strings.HasPrefix(entity, m) {
			return true
		}
	}
	return false
}

// resolvedSite is one site and, when its argument resolved, the name it reads.
type resolvedSite struct {
	Site Site
	Name string
	Expr string // the argument as written, for a message about a dynamic site
}

// collectionCode is what one pass over the Go tree produced.
type collectionCode struct {
	sites  []resolvedSite
	slices map[string]map[string][]string // file → var → string elements
}

// readCollectionCode parses every non-test Go file under root/pkg outside
// testdata, vendor and `_` directories, and resolves every `*.Collection(x)`
// call it finds.
//
// Two passes, because a constant may be declared in another file of its
// package — or in another package altogether — than the call that reads it.
func readCollectionCode(root string) (*collectionCode, error) {
	pkgRoot := filepath.Join(root, "pkg")
	info, err := os.Stat(pkgRoot)
	if err != nil {
		return nil, fmt.Errorf("docsguard: cannot read the Go packages under %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("docsguard: %s is not a directory", pkgRoot)
	}

	// importRef is one import of this module: its alias when it has one, and
	// the directory it points at.
	type importRef struct{ alias, dir string }
	type call struct {
		site    Site
		arg     ast.Expr
		bound   bool
		dir     string // the calling file's package directory
		imports []importRef
	}
	var calls []call
	consts := map[string]map[string]string{} // dir → const name → literal
	twins := map[[2]string]bool{}            // (dir, name) another file gives another literal
	pkgNames := map[string]string{}          // dir → package name
	out := &collectionCode{slices: map[string]map[string][]string{}}

	fset := token.NewFileSet()
	err = filepath.WalkDir(pkgRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("docsguard: parse %s: %w", rel, err)
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		pkgNames[dir] = file.Name.Name

		if consts[dir] == nil {
			consts[dir] = map[string]string{}
		}
		for name, lit := range stringConsts(file) {
			if prior, ok := consts[dir][name]; ok && prior != lit {
				twins[[2]string{dir, name}] = true
			}
			consts[dir][name] = lit
		}
		if slices := stringSliceVars(file); len(slices) > 0 {
			out.slices[rel] = slices
		}

		var imports []importRef
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(p, modulePrefix) {
				continue
			}
			ref := importRef{dir: strings.TrimPrefix(p, modulePrefix)}
			if imp.Name != nil {
				ref.alias = imp.Name.Name
			}
			imports = append(imports, ref)
		}
		for _, site := range collectionCalls(file, rel) {
			calls = append(calls, call{site: site.Site, arg: site.arg, bound: site.bound, dir: dir, imports: imports})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A constant two files of one directory declare with different string
	// literals — twins under exclusive build tags — names no single
	// collection: it resolves as dynamic, so a row has to say what each build
	// reaches. A twin declared any other way (a var, an expression, an
	// implicit repetition) is not seen, and every site of the name resolves
	// to the literal.
	for k := range twins {
		delete(consts[k[0]], k[1])
	}

	for _, c := range calls {
		// An un-aliased import is referred to by the imported package's
		// NAME, which is not always its directory's base — pkg/log is
		// imported as `iterlog` here and `log` there, and a package whose
		// name differs from its folder resolves to nothing at all if the
		// base is trusted. The names are known only now, once every
		// directory under pkg/ has been parsed.
		lookup := make(map[string]string, len(c.imports))
		for _, imp := range c.imports {
			local := imp.alias
			if local == "" {
				local = pkgNames[imp.dir]
			}
			if local == "" {
				local = imp.dir[strings.LastIndex(imp.dir, "/")+1:]
			}
			lookup[local] = imp.dir
		}
		name, how := "", Dynamic
		if !c.bound {
			name, how = resolveArg(c.arg, consts[c.dir], lookup, consts)
		}
		site := c.site
		site.How = how
		out.sites = append(out.sites, resolvedSite{Site: site, Name: name, Expr: types.ExprString(c.arg)})
	}
	sort.SliceStable(out.sites, func(i, j int) bool {
		if out.sites[i].Site.ID() != out.sites[j].Site.ID() {
			return out.sites[i].Site.ID() < out.sites[j].Site.ID()
		}
		return out.sites[i].Name < out.sites[j].Name
	})
	return out, nil
}

// resolveArg reads one `Collection(x)` argument.
func resolveArg(arg ast.Expr, own map[string]string, imports map[string]string, consts map[string]map[string]string) (string, Resolution) {
	switch a := arg.(type) {
	case *ast.BasicLit:
		if a.Kind == token.STRING {
			if lit, err := strconv.Unquote(a.Value); err == nil {
				return lit, ByLiteral
			}
		}
	case *ast.Ident:
		if lit, ok := own[a.Name]; ok {
			return lit, ByIdent
		}
	case *ast.SelectorExpr:
		if pkg, ok := a.X.(*ast.Ident); ok {
			if dir, ok := imports[pkg.Name]; ok {
				if lit, ok := consts[dir][a.Sel.Name]; ok {
					return lit, BySelector
				}
			}
		}
	}
	return "", Dynamic
}

// collectionCall is a call site before its argument is resolved.
type collectionCall struct {
	Site Site
	arg  ast.Expr
	// bound marks an identifier argument whose name its function binds — a
	// same-named function declared later in the file counts too: it is read
	// as Dynamic, whatever its spelling.
	bound bool
}

// collectionCalls finds every `*.Collection(x, …)` call of one file, tagged
// with the function that encloses it.
//
// The METHOD name is the whole rule: what matters is that a collection is
// being named, not what the receiver is called or what the constant holding
// the name is spelled like. The driver's Collection is variadic (name, then
// options), so only the FIRST argument is the name and a call with options
// is a site like any other — bailing on the arity would hide exactly the
// stores that pass a read concern.
func collectionCalls(file *ast.File, rel string) []collectionCall {
	var found []collectionCall
	harvest := func(node ast.Node, fn string) {
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Collection" || len(call.Args) == 0 {
				return true
			}
			found = append(found, collectionCall{Site: Site{File: rel, Func: fn}, arg: call.Args[0]})
			return true
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				bound := boundNames(d)
				harvest(d.Body, d.Name.Name)
				// A first argument that is a name the function binds itself
				// (a parameter, a receiver, a := or var, a range variable, a
				// closure's parameter) is not the package constant it may be
				// spelled like: it resolves as Dynamic, so the site must be
				// named by a row or it reddens. Over-approximate on purpose —
				// a name bound anywhere in the function counts — because a
				// wrong "constant" here records a collection that is not the
				// one the code reaches, and reports agreement. Only an identifier
				// argument is shadow-checked; a selector's base and a package-level
				// site are not.
				for i := range found {
					if found[i].Site.Func != d.Name.Name {
						continue
					}
					if id, ok := found[i].arg.(*ast.Ident); ok && bound[id.Name] {
						found[i].bound = true
					}
				}
			}
		default:
			harvest(d, "(package level)")
		}
	}
	return found
}

// boundNames collects every name a function binds locally: its receiver,
// parameters and results, every `:=` and `var`, every range variable, and
// the parameters of the closures it holds.
func boundNames(d *ast.FuncDecl) map[string]bool {
	bound := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				bound[n.Name] = true
			}
		}
	}
	addFields(d.Recv)
	addFields(d.Type.Params)
	addFields(d.Type.Results)
	ast.Inspect(d.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				for _, l := range s.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						bound[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range s.Names {
				bound[id.Name] = true
			}
		case *ast.RangeStmt:
			if s.Tok == token.DEFINE {
				for _, e := range []ast.Expr{s.Key, s.Value} {
					if id, ok := e.(*ast.Ident); ok {
						bound[id.Name] = true
					}
				}
			}
		case *ast.FuncLit:
			addFields(s.Type.Params)
			addFields(s.Type.Results)
		}
		return true
	})
	return bound
}

// stringConsts reads every `const NAME = "literal"` of one file — typed or
// not, alone or in a group.
func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, s := range gen.Specs {
			spec, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if unquoted, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = unquoted
				}
			}
		}
	}
	return out
}

// stringSliceVars reads every top-level `var NAME = []string{"a", "b"}`: a
// string literal is kept as its name, and any other element as its source
// behind nonLiteralElement, so the swept check refuses it instead of reading
// it as absent.
func stringSliceVars(file *ast.File) map[string][]string {
	out := map[string][]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, s := range gen.Specs {
			spec, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				composite, ok := spec.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				arr, ok := composite.Type.(*ast.ArrayType)
				if !ok {
					continue
				}
				if elt, ok := arr.Elt.(*ast.Ident); !ok || elt.Name != "string" {
					continue
				}
				var names []string
				for _, e := range composite.Elts {
					lit, ok := e.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						names = append(names, nonLiteralElement+types.ExprString(e))
						continue
					}
					if unquoted, err := strconv.Unquote(lit.Value); err == nil {
						names = append(names, unquoted)
					}
				}
				out[name.Name] = names
			}
		}
	}
	return out
}

// nonLiteralElement prefixes a swept-list element that is not a string literal,
// so checkSwept can refuse it by its source rather than skip it.
const nonLiteralElement = "\x00"

// declaredNames reads the top-level names one Go file declares: functions and
// methods, variables and constants.
func declaredNames(path string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out[d.Name.Name] = true
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if spec, ok := s.(*ast.ValueSpec); ok {
					for _, n := range spec.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out, nil
}

// The ER forms this guard reads. An entity is named in three places and the
// guard has to see all three, because a collection drawn ONLY as the far end
// of one relationship is still drawn.
var (
	// `runs {` — an entity block. The body's lines are attributes, never
	// entities, which is why the harvester tracks the brace depth.
	erEntityBlock = regexp.MustCompile(`^(` + erName + `)\s*\{\s*$`)
	// `teams ||--o{ runs : "owns"` — a relationship in the symbol
	// cardinalities, identifying (`--`) or not (`..`), its label one quoted
	// string or none. A line in any other form is refused as UnreadERLine.
	erRelationship = regexp.MustCompile(`^(` + erName + `)\s+(?:\|o|\|\||\}o|\}\|)(?:--|\.\.)(?:o\||\|\||o\{|\|\{)\s+(` + erName + `)\s*(?::\s*"[^"]*")?\s*$`)
	// A quoted comment or label, which may hold a brace of its own.
	erQuoted = regexp.MustCompile(`"[^"]*"`)
	// `runs` alone — an entity with no attribute block.
	erBareEntity = regexp.MustCompile(`^(` + erName + `)\s*$`)
)

// erName is an entity name: a bare identifier, or a quoted string — which is
// how a name carrying a marker prefix ("S3: artifacts") has to be written,
// since mermaid's bare names take no colon and no space.
const erName = `"[^"]+"|[A-Za-z_][A-Za-z0-9_]*`

// diagramPages lists the files of the diagrams directory whose base name
// matches pattern. It reads the directory rather than globbing a path under
// root: a root path holding glob syntax ([, ?, *) is a path, and read as a
// pattern it would miss the tree or reach into a sibling's.
func diagramPages(root, pattern string) ([]string, error) {
	dir := filepath.Join(root, filepath.FromSlash(diagramsDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pages []string
	for _, e := range entries {
		ok, err := filepath.Match(pattern, e.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			pages = append(pages, filepath.Join(dir, e.Name()))
		}
	}
	return pages, nil
}

// readStateLabels refuses every `;` on a non-comment line of a stateDiagram
// block of a page directly under docs/diagrams — the flow pages included.
func readStateLabels(root string) []Problem {
	pages, err := diagramPages(root, "*.md")
	if err != nil {
		return []Problem{{Kind: MissingCorpus, Subject: diagramsDir, Detail: "the diagram pages cannot be listed (" + err.Error() + ")"}}
	}
	sort.Strings(pages)
	var problems []Problem
	for _, page := range pages {
		raw, err := os.ReadFile(page)
		rel := diagramsDir + "/" + filepath.Base(page)
		if err != nil {
			problems = append(problems, Problem{Kind: MissingCorpus, Subject: rel, Detail: "a diagram page cannot be read (" + err.Error() + ")"})
			continue
		}
		for _, hit := range stateLabelSemicolons(string(raw)) {
			problems = append(problems, Problem{
				Kind:    SemicolonInStateLabel,
				Subject: rel + ":" + strconv.Itoa(hit.line),
				Detail:  "`" + hit.text + "` — the pages write no `;` on a state-diagram line outside a `%%` comment: after a label's colon mermaid ends the statement there and draws the rest of the line as stray states. Use a dash or a comma",
			})
		}
	}
	return problems
}

// labelHit is one line of a stateDiagram block that holds a `;`.
type labelHit struct {
	line int
	text string
}

// stateLabelSemicolons walks the mermaid fences of one page and returns the
// lines of its state diagrams that hold a `;` outside a `%%` comment line.
func stateLabelSemicolons(md string) []labelHit {
	var fence mdcode.FenceScanner
	var hits []labelHit
	var body []string
	var at []int
	mermaid, quoted := false, false
	judge := func() {
		if mermaid {
			hits = append(hits, stateBlockSemicolons(body, at)...)
		}
		body, at, mermaid = nil, nil, false
	}
	for i, line := range mdcode.Lines(md) {
		wasOpen := fence.Open()
		isCode := fence.Code(line)
		switch {
		case !wasOpen && fence.Open(): // the opening delimiter
			mermaid, quoted = fenceInfo(line) == "mermaid", inBlockquote(line)
		case wasOpen && !fence.Open(): // the closing delimiter
			judge()
		case isCode:
			body, at = append(body, unquoteLine(line, quoted)), append(at, i+1)
		}
	}
	judge() // a fence left open at the end of the page still drew something
	return hits
}

// stateBlockSemicolons judges one mermaid fence, a state diagram when its
// header names one; at holds each body line's number on the page.
func stateBlockSemicolons(body []string, at []int) []labelHit {
	start, header := blockHeader(body)
	if start < 0 || !strings.HasPrefix(header, "stateDiagram") {
		return nil
	}
	var hits []labelHit
	// What follows the keyword on the header line is a statement to mermaid.
	if strings.Contains(header, ";") {
		hits = append(hits, labelHit{line: at[start], text: strings.TrimSpace(body[start])})
	}
	for j := start + 1; j < len(body); j++ {
		// Every line but a `%%` comment is refused for a `;`, whatever its
		// form: the one place mermaid would keep one, a quoted state name,
		// is refused too — a dash or a comma serves everywhere, and one rule
		// leaves no form to miss.
		if t := strings.TrimSpace(body[j]); !isMermaidComment(t) && strings.Contains(t, ";") {
			hits = append(hits, labelHit{line: at[j], text: t})
		}
	}
	return hits
}

// readEntities collects the entity names of every erDiagram block of the
// data-model pages, through the repository's own fence scanner.
func readEntities(root string) (entities []string, pages []string, problems []Problem) {
	matches, err := diagramPages(root, erPageGlob)
	if err != nil {
		return nil, nil, []Problem{{
			Kind:    MissingCorpus,
			Subject: diagramsDir + "/" + erPageGlob,
			Detail:  "the ER pages cannot be listed: " + err.Error(),
		}}
	}
	sort.Strings(matches)
	seen := map[string]bool{}
	for _, path := range matches {
		rel := diagramsDir + "/" + filepath.Base(path)
		body, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, Problem{
				Kind: MissingCorpus, Subject: rel,
				Detail: "an ER page the glob matched cannot be read: " + err.Error(),
			})
			continue
		}
		pages = append(pages, rel)
		names, unread := erEntities(string(body))
		for _, name := range names {
			seen[name] = true
		}
		for _, line := range unread {
			problems = append(problems, Problem{
				Kind: UnreadERLine, Subject: rel,
				Detail: line + " — redraw it as an entity block, a relationship in the symbol cardinalities, or a bare entity",
			})
		}
	}
	if len(pages) == 0 {
		problems = append(problems, Problem{
			Kind:    MissingCorpus,
			Subject: diagramsDir + "/" + erPageGlob,
			Detail:  "no ER page exists, so every collection below is unmapped for want of a drawing, not for want of agreement",
		})
	}
	return sortedKeys(seen), pages, problems
}

// erEntities reads one page: the entity names of its erDiagram fences.
//
// The fence machine is internal/mdcode's — the one the link checker and the
// map generator read markdown with. A page that SHOWS an erDiagram inside a
// ```markdown block, or that opens a fence a stray backtick would confuse, is
// read the same way by all three or by none. Like them, it does not model an
// HTML block or a four-column indented code block: a fence inside one is read
// as drawn although the site draws nothing there. A fence inside a blockquote
// is read without its quote marks, as the site draws it. It reads mermaid as
// these pages write it and is no parser of it: a line it takes for one form
// that mermaid reads as another goes by unrefused.
func erEntities(md string) (names, unread []string) {
	var fence mdcode.FenceScanner
	var body []string
	mermaid, quoted := false, false
	harvest := func() {
		if mermaid {
			n, u := erBlockEntities(body)
			names, unread = append(names, n...), append(unread, u...)
		}
		body, mermaid = nil, false
	}
	for _, line := range mdcode.Lines(md) {
		wasOpen := fence.Open()
		isCode := fence.Code(line)
		switch {
		case !wasOpen && fence.Open(): // the opening delimiter
			mermaid, quoted = fenceInfo(line) == "mermaid", inBlockquote(line)
		case wasOpen && !fence.Open(): // the closing delimiter
			harvest()
		case isCode:
			body = append(body, unquoteLine(line, quoted))
		}
	}
	harvest() // a fence left open at the end of the page still drew something
	return names, unread
}

// fenceInfo is the info string of a line mdcode has just called an opening
// delimiter: everything after the delimiter run, which for a mermaid block is
// the word mermaid.
func fenceInfo(line string) string {
	trimmed := strings.TrimLeft(line, " \t>")
	i := strings.IndexAny(trimmed, "`~")
	if i < 0 {
		return ""
	}
	marker := trimmed[i]
	rest := strings.TrimLeft(trimmed[i:], string(marker))
	return strings.TrimSpace(rest)
}

// erBlockEntities reads the body of one mermaid fence, an ER diagram when its
// header names one. It returns the entities and every line after the header
// it could not read.
func erBlockEntities(body []string) (names, unread []string) {
	start, header := blockHeader(body)
	if start < 0 || !strings.HasPrefix(header, "erDiagram") {
		return nil, nil
	}
	// What follows the keyword on the header line is a statement to
	// mermaid, and one this reader does not split.
	if rest := strings.TrimSpace(strings.TrimPrefix(header, "erDiagram")); rest != "" {
		unread = append(unread, rest)
	}
	depth := 0
	for _, line := range body[start+1:] {
		trimmed := strings.TrimSpace(line)
		if depth > 0 {
			// Attributes, until the brace that closes the entity block,
			// looked for outside the quoted comments that may hold one. A
			// comment left open on its line, or a statement after that
			// brace, is refused like any line this reader does not split.
			if strings.Count(trimmed, `"`)%2 == 1 {
				unread = append(unread, trimmed)
				continue
			}
			outside := erQuoted.ReplaceAllString(trimmed, `""`)
			if i := strings.Index(outside, "}"); i >= 0 {
				depth--
				if strings.TrimSpace(outside[i+1:]) != "" {
					unread = append(unread, trimmed)
				}
			}
			continue
		}
		if trimmed == "" || isMermaidComment(trimmed) {
			continue
		}
		if m := erEntityBlock.FindStringSubmatch(trimmed); m != nil {
			names = append(names, unquoteEntity(m[1]))
			depth++
			continue
		}
		if m := erRelationship.FindStringSubmatch(trimmed); m != nil {
			names = append(names, unquoteEntity(m[1]), unquoteEntity(m[2]))
			continue
		}
		if m := erBareEntity.FindStringSubmatch(trimmed); m != nil {
			names = append(names, unquoteEntity(m[1]))
			continue
		}
		unread = append(unread, trimmed)
	}
	return names, unread
}

func unquoteEntity(name string) string { return strings.Trim(name, `"`) }

// blockHeader finds a mermaid block's header, the line naming its diagram
// kind, the way mermaid does: the first line that is not blank, YAML front
// matter, a `%%` comment, or inside a `%%{…}%%` directive. It returns the
// header's index and its text after any directive closing on that line, or
// -1 when the block has none.
func blockHeader(body []string) (int, string) {
	inDirective, inFrontMatter, first := false, false, true
	for i, line := range body {
		t := strings.TrimSpace(line)
		if inFrontMatter {
			inFrontMatter = t != "---"
			continue
		}
		if first && t == "---" {
			// Front matter opens the diagram text and runs to its own ---.
			inFrontMatter, first = true, false
			continue
		}
		if t != "" {
			first = false
		}
		if !inDirective && strings.HasPrefix(t, "%%{") {
			inDirective = true
		}
		if inDirective {
			end := strings.LastIndex(t, "}%%")
			if end < 0 {
				continue
			}
			inDirective = false
			if t = strings.TrimSpace(t[end+3:]); t == "" {
				continue
			}
			return i, t
		}
		if t == "" || isMermaidComment(t) {
			continue
		}
		return i, t
	}
	return -1, ""
}

// inBlockquote reports a fence opener inside a blockquote.
func inBlockquote(opener string) bool {
	return strings.HasPrefix(strings.TrimLeft(opener, " \t"), ">")
}

// unquoteLine drops a blockquoted fence line's quote marks, which mermaid
// never sees; a line of an unquoted fence is returned as it is.
func unquoteLine(line string, quoted bool) string {
	if !quoted {
		return line
	}
	return strings.TrimLeft(line, " \t>")
}

// isMermaidComment reports a `%%` comment line. A line that closes a
// `%%{…}%%` directive is not one: mermaid reads what follows the directive.
func isMermaidComment(t string) bool {
	return strings.HasPrefix(t, "%%") && !strings.Contains(t, "}%%")
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnion(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		set[s] = true
	}
	return sortedKeys(set)
}
