package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestForgeDeliveryPreflightUsesActualTokenProof(t *testing.T) {
	for _, name := range []string{"allowed", "narrow-token-wide-installation", "unverified", "expired", "different-mounted-token", "stale-proof", "secret-other-tenant", "wrong-repo", "wrong-host", "pr-url-names-no-pull-request", "wrong-team", "unknown-grant", "inactive"} {
		t.Run(name, func(t *testing.T) {
			s, review := newForgePublishTestServer(t)
			s.sealer, _ = secrets.NewAESGCMSealer(make([]byte, 32))
			s.genericSecrets = secrets.NewMemoryGenericSecretStore()
			ctx := store.WithTenant(t.Context(), "team1")
			conn, _ := s.forgeConnections.Get(ctx, "conn1")
			conn.Kind, conn.Status, conn.ManagedSecretID = forge.KindGitHubApp, forge.StatusActive, "managed"
			conn.GrantedPermissions = map[string]string{"contents": "write", "workflows": "write"}
			if name == "inactive" {
				conn.Status = forge.StatusNeedsReauth
			}
			if err := s.forgeConnections.Update(ctx, conn); err != nil {
				t.Fatal(err)
			}
			proof := secrets.NewTokenPermissionProof("ghs_runtime_secret", map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour))
			switch name {
			case "narrow-token-wide-installation":
				delete(proof.Permissions, "workflows")
			case "unverified":
				proof = nil
			case "expired":
				proof.ExpiresAt = time.Now().Add(-time.Second)
			case "stale-proof":
				proof.SHA256 = secrets.TokenSHA256("previous-token")
			}
			sealed, _ := secrets.SealGenericSecret(s.sealer, "managed", []byte("ghs_runtime_secret"))
			rec := secrets.GenericSecret{ID: "managed", TenantID: "team1", ScopeTeamID: "team1", SealedSecret: sealed, ForgeTokenProof: proof}
			createCtx := ctx
			if name == "secret-other-tenant" {
				createCtx = store.WithTenant(t.Context(), "team2")
				rec.TenantID = "team2"
			}
			if err := s.genericSecrets.Create(createCtx, rec); err != nil {
				t.Fatal(err)
			}
			grant := ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}
			if name == "wrong-team" {
				grant.TeamID = "team2"
			}
			registerPublishToken(t, s, "grant", grant)
			prURL, tokenHash := "https://github.com/o/r/pull/1", secrets.TokenSHA256("ghs_runtime_secret")
			if name == "different-mounted-token" {
				tokenHash = secrets.TokenSHA256("some-other-token")
			}
			if name == "wrong-repo" {
				prURL = "https://github.com/o/other/pull/1"
			}
			if name == "wrong-host" {
				prURL = "https://other.example/o/r/pull/1"
			}
			// A URL that names no pull request is refused here, before any
			// credential is read — which is why the probe refuses one itself
			// instead of spending a question on it.
			if name == "pr-url-names-no-pull-request" {
				prURL = "https://github.com/o/r"
			}
			body, _ := json.Marshal(map[string]string{"pr_url": prURL, "credential_sha256": tokenHash})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
			r.Header.Set("X-Iterion-Run", "grant")
			if name == "unknown-grant" {
				r.Header.Set("X-Iterion-Run", "unknown")
			}
			w := httptest.NewRecorder()
			s.handleForgeDeliveryPreflight(w, r)
			if strings.Contains(w.Body.String(), "ghs_runtime_secret") || review.calls != 0 {
				t.Fatal("preflight leaked a secret or wrote to the forge")
			}
			// The rows that leave with a STATUS and never with a verdict:
			// a scope the run grant does not cover, and this service's own
			// bookkeeping about its own credential — the connection record
			// reading anything but active, and no live mint proof of the very
			// token the run mounted, which iterion rotates under its own
			// running runs (see
			// TestForgeDeliveryPreflightRefusesOnlyFromTheProvidersAnswer).
			switch name {
			case "wrong-repo", "wrong-host", "pr-url-names-no-pull-request", "wrong-team", "unknown-grant", "secret-other-tenant",
				"unverified", "expired", "different-mounted-token", "stale-proof", "inactive":
				if w.Code < 400 {
					t.Fatalf("%s answered under %d, and a 2xx here is a verdict a caller may publish: %s", name, w.Code, w.Body)
				}
				return
			}
			var verdict forgeDeliveryVerdict
			if err := json.Unmarshal(w.Body.Bytes(), &verdict); err != nil {
				t.Fatalf("response: %d %s", w.Code, w.Body)
			}
			if verdict.OK != (name == "allowed") {
				t.Fatalf("verdict=%+v", verdict)
			}
			if !verdict.OK && (verdict.Code != "FORGE_PERMISSION_DENIED" || !strings.Contains(verdict.Reason, "workflows")) {
				t.Fatalf("untyped refusal: %+v", verdict)
			}
		})
	}
}

// forgePreflightOutageStore answers every read with the transport failure of a
// store whose backend is unreachable.
type forgePreflightOutageStore struct{ secrets.GenericSecretStore }

func (forgePreflightOutageStore) Get(context.Context, string) (secrets.GenericSecret, error) {
	return secrets.GenericSecret{}, errors.New("mongo: server selection timeout: no reachable servers")
}

// A verdict under 200 is the one answer a caller may publish as a blocking
// check on a pull request (bots/dep-update-guard/main.bot's `hold`), so this
// endpoint owes it only when something was established about the credential
// ITSELF. Every row here breaks the service instead — the store, the sealer,
// the server's own wiring — with a credential whose proof would otherwise
// pass, and none of them may answer a verdict.
func TestForgeDeliveryPreflightNeverAnswersItsOwnOutageAsAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		breakIt  func(t *testing.T, s *Server, mem secrets.GenericSecretStore)
		wantCode int
	}{
		{
			name: "the credential store cannot be read",
			breakIt: func(_ *testing.T, s *Server, mem secrets.GenericSecretStore) {
				s.genericSecrets = forgePreflightOutageStore{mem}
			},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "the sealed credential cannot be opened",
			breakIt: func(t *testing.T, s *Server, _ secrets.GenericSecretStore) {
				other, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
				if err != nil {
					t.Fatal(err)
				}
				s.sealer = other
			},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "the server has no credential store at all",
			breakIt: func(_ *testing.T, s *Server, _ secrets.GenericSecretStore) {
				s.genericSecrets = nil
			},
			wantCode: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, review := newForgePublishTestServer(t)
			s.sealer, _ = secrets.NewAESGCMSealer(make([]byte, 32))
			mem := secrets.NewMemoryGenericSecretStore()
			s.genericSecrets = mem
			ctx := store.WithTenant(t.Context(), "team1")
			conn, _ := s.forgeConnections.Get(ctx, "conn1")
			conn.Kind, conn.Status, conn.ManagedSecretID = forge.KindGitHubApp, forge.StatusActive, "managed"
			conn.GrantedPermissions = map[string]string{"contents": "write", "workflows": "write"}
			if err := s.forgeConnections.Update(ctx, conn); err != nil {
				t.Fatal(err)
			}
			proof := secrets.NewTokenPermissionProof("ghs_runtime_secret", map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour))
			sealed, _ := secrets.SealGenericSecret(s.sealer, "managed", []byte("ghs_runtime_secret"))
			if err := mem.Create(ctx, secrets.GenericSecret{ID: "managed", TenantID: "team1", ScopeTeamID: "team1", SealedSecret: sealed, ForgeTokenProof: proof}); err != nil {
				t.Fatal(err)
			}
			// Everything a verdict needs is in place before the break: the
			// App connection is active and the proof carries workflows:write.
			tc.breakIt(t, s, mem)
			registerPublishToken(t, s, "grant", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

			body, _ := json.Marshal(map[string]string{"pr_url": "https://github.com/o/r/pull/1", "credential_sha256": secrets.TokenSHA256("ghs_runtime_secret")})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
			r.Header.Set("X-Iterion-Run", "grant")
			w := httptest.NewRecorder()
			s.handleForgeDeliveryPreflight(w, r)

			if strings.Contains(w.Body.String(), "ghs_runtime_secret") || review.calls != 0 {
				t.Fatal("preflight leaked a secret or wrote to the forge")
			}
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d %s, want %d: our own outage is not an answer about a credential", w.Code, w.Body.String(), tc.wantCode)
			}
			var verdict forgeDeliveryVerdict
			_ = json.Unmarshal(w.Body.Bytes(), &verdict)
			if verdict.Code != "" || strings.Contains(w.Body.String(), "FORGE_PERMISSION_DENIED") {
				t.Fatalf("a blocking verdict was minted out of this service's own failure: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// A verdict is an answer about the GRANT, and the only term of the stored mint
// proof that carries one is the permission map the provider answered when it
// minted the token. The other three terms of TokenPermissionProof.Allows —
// that a proof exists, that it names the token this run mounted, and that it
// has not expired — are this service's own bookkeeping: iterion rewrites the
// managed credential's plaintext in place at every point of use
// (Orchestrator.EnsureManagedSecret → narrowGitHubAppSecret, best-effort and
// with no freshness guard) and five minutes before the installation token's
// hour runs out (forge.RefreshWorker), while a run re-reads its mounted file
// every runner.fileSecretRefreshInterval — so a run legitimately holds, for
// minutes at a time, the previous live token of its own App. None of those
// may be published as a blocking hold on somebody's pull request.
func TestForgeDeliveryPreflightAnswersAVerdictOnlyFromALiveProofOfTheMountedToken(t *testing.T) {
	const stored = "ghs_runtime_secret"
	for _, tc := range []struct {
		name string
		// proof rewrites the proof recorded for the stored token; mounted is
		// the token the RUN carries, when it is not the stored one.
		proof       func(*secrets.TokenPermissionProof) *secrets.TokenPermissionProof
		mounted     string
		wantCode    int
		wantVerdict bool
	}{
		{
			name:     "the managed credential was rotated after the run mounted its token",
			mounted:  "ghs_the_previous_token_of_the_same_app",
			wantCode: http.StatusConflict,
		},
		{
			name: "the run outlived the hour its mint proof was recorded for",
			proof: func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
				p.ExpiresAt = time.Now().Add(-time.Second)
				return p
			},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name:     "the mint proof was never recorded",
			proof:    func(*secrets.TokenPermissionProof) *secrets.TokenPermissionProof { return nil },
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "the recorded proof names another token",
			proof: func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
				p.SHA256 = secrets.TokenSHA256("a token this record no longer holds")
				return p
			},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			// THE FACT, and #1595 itself: a live proof OF THIS TOKEN whose
			// permission map carries no workflows:write. It is the one shape
			// that owes a blocking verdict, and the rows above must not cost
			// it.
			name: "the App's own mint answered without workflows:write",
			proof: func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
				delete(p.Permissions, "workflows")
				return p
			},
			wantCode: http.StatusOK, wantVerdict: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, review := newForgePublishTestServer(t)
			s.sealer, _ = secrets.NewAESGCMSealer(make([]byte, 32))
			mem := secrets.NewMemoryGenericSecretStore()
			s.genericSecrets = mem
			ctx := store.WithTenant(t.Context(), "team1")
			conn, _ := s.forgeConnections.Get(ctx, "conn1")
			conn.Kind, conn.Status, conn.ManagedSecretID = forge.KindGitHubApp, forge.StatusActive, "managed"
			conn.GrantedPermissions = map[string]string{"contents": "write", "workflows": "write"}
			if err := s.forgeConnections.Update(ctx, conn); err != nil {
				t.Fatal(err)
			}
			proof := secrets.NewTokenPermissionProof(stored, map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour))
			if tc.proof != nil {
				proof = tc.proof(proof)
			}
			sealed, _ := secrets.SealGenericSecret(s.sealer, "managed", []byte(stored))
			if err := mem.Create(ctx, secrets.GenericSecret{ID: "managed", TenantID: "team1", ScopeTeamID: "team1", SealedSecret: sealed, ForgeTokenProof: proof}); err != nil {
				t.Fatal(err)
			}
			registerPublishToken(t, s, "grant", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

			mounted := stored
			if tc.mounted != "" {
				mounted = tc.mounted
			}
			body, _ := json.Marshal(map[string]string{"pr_url": "https://github.com/o/r/pull/1", "credential_sha256": secrets.TokenSHA256(mounted)})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
			r.Header.Set("X-Iterion-Run", "grant")
			w := httptest.NewRecorder()
			s.handleForgeDeliveryPreflight(w, r)

			if strings.Contains(w.Body.String(), stored) || review.calls != 0 {
				t.Fatal("preflight leaked a secret or wrote to the forge")
			}
			var verdict forgeDeliveryVerdict
			_ = json.Unmarshal(w.Body.Bytes(), &verdict)
			if !tc.wantVerdict && verdict.Code != "" {
				t.Fatalf("a blocking verdict was minted out of this service's own bookkeeping about its own credential: %d %s", w.Code, w.Body.String())
			}
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d %s, want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if tc.wantVerdict && (verdict.OK || verdict.Code != "FORGE_PERMISSION_DENIED" || !strings.Contains(verdict.Reason, "workflows")) {
				t.Fatalf("the one fact about the grant no longer answers a verdict: %+v", verdict)
			}
		})
	}
}

// forgePreflightVerbatimConnStore and forgePreflightVerbatimSecretStore answer
// the record the test wrote, field for field: a store that normalises a value,
// or refuses the write, would let the sweep below pass without ever varying
// what the handler reads.
type forgePreflightVerbatimConnStore struct {
	forge.ConnectionStore
	conn forge.Connection
}

func (s forgePreflightVerbatimConnStore) Get(context.Context, string) (forge.Connection, error) {
	return s.conn, nil
}

type forgePreflightVerbatimSecretStore struct {
	secrets.GenericSecretStore
	rec secrets.GenericSecret
}

func (s forgePreflightVerbatimSecretStore) Get(context.Context, string) (secrets.GenericSecret, error) {
	return s.rec, nil
}

// forgePreflightValues is the vocabulary the sweep below moves a field TO, read
// from SOURCE rather than listed in this file: the values a named string type
// declares in the package that declares it, and every string value this
// handler's own source carries — its literals and the value of every constant
// of this module it names. A list written here would hold what its author knew
// the day they wrote it, which is the drift pkg/store's
// TestReservedFailureCodesCoversEveryDeclaredConstant parses declarations to
// avoid; read from source, a status pkg/forge gains tomorrow, an untyped string
// enum (forge.AccountKind* carry no type) and the permission level a term
// compares a map entry against are all presented without this file changing.
// Residue: a value the handler names from OUTSIDE this module (a stdlib
// constant) is not read.
type forgePreflightValues struct {
	// spelled is every string value the handler's own source carries.
	spelled []string
	// packageAt reads the string constants one package of this module declares.
	packageAt func(t *testing.T, importPath string) forgePreflightPackageConsts
}

// forgePreflightPackageConsts holds the string constants one package declares:
// byName for a constant the handler names, byType for the domain of a named
// string type. unreadable names a type one of whose declared values this
// reader cannot read, so the sweep fatals rather than present a domain it knows
// to be incomplete.
type forgePreflightPackageConsts struct {
	byName     map[string]string
	byType     map[string][]string
	unreadable map[string]bool
}

// declaredBy answers the domain of a NAMED string type, read from the package
// that declares it. An empty domain is legitimate (a named string that is not
// an enum); a domain this reader cannot read, or a type declared outside this
// module, is a fatal.
func (v forgePreflightValues) declaredBy(t *testing.T, name string, typ reflect.Type) []string {
	t.Helper()
	consts := v.packageAt(t, typ.PkgPath())
	if consts.unreadable[typ.String()] {
		t.Fatalf("the sweep cannot read every value %s (%s) declares: teach forgePreflightConstValue, or the guarantee holds only for the values it can present", name, typ)
	}
	return consts.byType[typ.String()]
}

// forgePreflightValueOracle reads those two vocabularies out of the tree: the
// handler's own package (every file of it, so a constant a term names from a
// sibling file resolves too) and, on demand, the package that declares the type
// of a field the sweep moves.
func forgePreflightValueOracle(t *testing.T, handlerFile string) forgePreflightValues {
	t.Helper()
	root, module := forgePreflightModule(t)
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, here)
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]forgePreflightPackageConsts{}
	packageAt := func(t *testing.T, importPath string) forgePreflightPackageConsts {
		t.Helper()
		if consts, ok := parsed[importPath]; ok {
			return consts
		}
		if importPath != module && !strings.HasPrefix(importPath, module+"/") {
			t.Fatalf("%s is declared outside this module, so the sweep cannot read the values it declares: teach it, or the guarantee holds only for the values it can present", importPath)
		}
		consts := forgePreflightPackageConstsAt(t, filepath.Join(root, strings.TrimPrefix(strings.TrimPrefix(importPath, module), "/")))
		parsed[importPath] = consts
		return consts
	}
	own := packageAt(t, path.Join(module, filepath.ToSlash(rel)))
	return forgePreflightValues{
		spelled:   forgePreflightSpelledValues(t, handlerFile, module, own, packageAt),
		packageAt: packageAt,
	}
}

// forgePreflightModule locates the module whose sources declare those values.
func forgePreflightModule(t *testing.T) (dir, module string) {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return dir, strings.TrimSpace(module)
				}
			}
			t.Fatalf("%s/go.mod names no module, so the sweep cannot find the sources that declare the values it must present", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package under test, so the sweep cannot find the sources that declare the values it must present")
		}
		dir = parent
	}
}

// forgePreflightPackageConstsAt reads one package's string constants from its
// source. It descends into function bodies: a constant declared inside the
// handler is spelled there and nowhere else.
func forgePreflightPackageConstsAt(t *testing.T, dir string) forgePreflightPackageConsts {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s, a package whose declared values the sweep must present: %v", dir, err)
	}
	out := forgePreflightPackageConsts{
		byName:     map[string]string{},
		byType:     map[string][]string{},
		unreadable: map[string]bool{},
	}
	fset := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		files++
		forgePreflightReadConsts(file, &out)
	}
	if files == 0 {
		t.Fatalf("%s holds no Go source, so the sweep would present nothing of what it declares", dir)
	}
	return out
}

// forgePreflightReadConsts collects every string constant of one file, and the
// domain of every named type it declares one for.
func forgePreflightReadConsts(file *ast.File, out *forgePreflightPackageConsts) {
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// reflect spells a named type with its package's own name, which
			// is how a declaration spells it too.
			typeName := ""
			switch declared := valueSpec.Type.(type) {
			case *ast.Ident:
				typeName = file.Name.Name + "." + declared.Name
			case *ast.SelectorExpr:
				if qualifier, ok := declared.X.(*ast.Ident); ok {
					typeName = qualifier.Name + "." + declared.Sel.Name
				}
			}
			for i, name := range valueSpec.Names {
				if i >= len(valueSpec.Values) {
					continue
				}
				value, ok := forgePreflightConstValue(valueSpec.Values[i])
				if !ok {
					if typeName != "" {
						out.unreadable[typeName] = true
					}
					continue
				}
				out.byName[name.Name] = value
				if typeName != "" {
					out.byType[typeName] = append(out.byType[typeName], value)
				}
			}
		}
		return true
	})
}

// forgePreflightConstValue reads a constant's string value. A constant
// expression can only convert, so a call around the literal is a conversion:
// forge.Provider("github").
func forgePreflightConstValue(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(value.Value)
		return unquoted, err == nil
	case *ast.CallExpr:
		if len(value.Args) != 1 {
			return "", false
		}
		return forgePreflightConstValue(value.Args[0])
	}
	return "", false
}

// forgePreflightSpelledValues reads every string value the handler's own source
// carries: its literals, the constants of its own package it names and the
// constants of this module's other packages it names. A term keyed on a value
// this handler spells is therefore presented whatever the spelling — a literal,
// a local constant, or one imported from the package that owns the record.
func forgePreflightSpelledValues(t *testing.T, handlerFile, module string, own forgePreflightPackageConsts, packageAt func(*testing.T, string) forgePreflightPackageConsts) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, handlerFile, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", handlerFile, err)
	}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("%s imports %s, which the sweep cannot read", handlerFile, spec.Path.Value)
		}
		if !strings.HasPrefix(imported, module+"/") {
			continue
		}
		name := path.Base(imported)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = imported
	}
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch expr := node.(type) {
		case *ast.BasicLit:
			if value, ok := forgePreflightConstValue(expr); ok {
				add(value)
			}
		case *ast.SelectorExpr:
			qualifier, ok := expr.X.(*ast.Ident)
			if !ok {
				return true
			}
			if imported, ok := imports[qualifier.Name]; ok {
				if value, ok := packageAt(t, imported).byName[expr.Sel.Name]; ok {
					add(value)
				}
			}
		case *ast.Ident:
			if value, ok := own.byName[expr.Name]; ok {
				add(value)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s spells no string value, so the sweep would present nothing of its own vocabulary", handlerFile)
	}
	return out
}

// forgePreflightPerturbations moves one field of one of those records away from
// the baseline in every direction a term could read it: a string to the empty
// value, to a value no term expects, to every value its own type declares and
// to every value this handler spells; a map to absent, to a junk entry and to
// its own keys at every value this handler spells; a bool to its opposite; a
// number, a slice, a pointer and a timestamp to unset, set and out of range. A
// type it cannot move, or a domain it cannot read, is a FATAL rather than a
// skip: a field — or a value of one — the sweep passes over in silence would
// make the guarantee narrower than the sentence that states it.
func forgePreflightPerturbations(t *testing.T, values forgePreflightValues, name string, base reflect.Value) []reflect.Value {
	t.Helper()
	const junk = "a value no term of this handler expects"
	typ := base.Type()
	var out []reflect.Value
	add := func(v reflect.Value) {
		if reflect.DeepEqual(v.Interface(), base.Interface()) {
			return
		}
		for _, already := range out {
			if reflect.DeepEqual(already.Interface(), v.Interface()) {
				return
			}
		}
		out = append(out, v)
	}
	of := func(set func(reflect.Value)) reflect.Value {
		v := reflect.New(typ).Elem()
		set(v)
		return v
	}
	switch typ.Kind() {
	case reflect.String:
		candidates := []string{"", junk}
		if typ.PkgPath() != "" {
			candidates = append(candidates, values.declaredBy(t, name, typ)...)
		}
		for _, candidate := range append(candidates, values.spelled...) {
			add(of(func(v reflect.Value) { v.SetString(candidate) }))
		}
	case reflect.Bool:
		add(of(func(v reflect.Value) { v.SetBool(!base.Bool()) }))
	case reflect.Int, reflect.Int32, reflect.Int64:
		if typ.PkgPath() != "" {
			t.Fatalf("%s has the named integer type %s, whose declared values this sweep does not read: teach it, or the guarantee holds only for the values it can present", name, typ)
		}
		add(of(func(v reflect.Value) { v.SetInt(0) }))
		add(of(func(v reflect.Value) { v.SetInt(7777) }))
	case reflect.Map:
		add(reflect.Zero(typ))
		if typ.Key().Kind() != reflect.String || typ.Elem().Kind() != reflect.String {
			t.Fatalf("the sweep cannot fill %s (%s): extend it, or the guarantee holds only for the fields it can move", name, typ)
		}
		filled := reflect.MakeMap(typ)
		filled.SetMapIndex(reflect.ValueOf(junk).Convert(typ.Key()), reflect.ValueOf(junk).Convert(typ.Elem()))
		add(filled)
		// A term reading one ENTRY of a permission map compares it to a level
		// no type bounds, so what bounds it is the handler's own vocabulary,
		// carried on the keys the record really holds — one at a time, and all
		// at once.
		graded := func(mutate func(reflect.Value)) reflect.Value {
			m := reflect.MakeMap(typ)
			for entries := base.MapRange(); entries.Next(); {
				m.SetMapIndex(entries.Key(), entries.Value())
			}
			mutate(m)
			return m
		}
		for _, level := range append([]string{"", junk}, values.spelled...) {
			at := reflect.ValueOf(level).Convert(typ.Elem())
			add(graded(func(m reflect.Value) {
				for _, key := range base.MapKeys() {
					m.SetMapIndex(key, at)
				}
			}))
			for _, key := range base.MapKeys() {
				add(graded(func(m reflect.Value) { m.SetMapIndex(key, at) }))
			}
		}
	case reflect.Slice:
		add(reflect.Zero(typ))
		switch typ.Elem().Kind() {
		case reflect.String:
			filled := reflect.MakeSlice(typ, 1, 1)
			filled.Index(0).SetString(junk)
			add(filled)
		case reflect.Uint8:
			add(reflect.ValueOf([]byte(junk)).Convert(typ))
		default:
			t.Fatalf("the sweep cannot fill %s (%s): extend it, or the guarantee holds only for the fields it can move", name, typ)
		}
	case reflect.Pointer:
		add(reflect.Zero(typ))
		add(reflect.New(typ.Elem()))
	case reflect.Struct:
		if typ != reflect.TypeOf(time.Time{}) {
			t.Fatalf("the sweep cannot vary %s (%s): extend it, or the guarantee holds only for the fields it can move", name, typ)
		}
		add(reflect.ValueOf(time.Time{}))
		add(reflect.ValueOf(time.Now().Add(-100 * time.Hour)))
		add(reflect.ValueOf(time.Now().Add(100 * time.Hour)))
	default:
		t.Fatalf("the sweep cannot vary %s (%s): extend it, or the guarantee holds only for the fields it can move", name, typ)
	}
	if len(out) == 0 {
		t.Fatalf("no perturbation of %s (%s) differs from the baseline, so its row would pass without varying anything", name, typ)
	}
	return out
}

// The endpoint refuses from ONE fact and no other: the permission map the
// provider recorded when it minted the very token this run mounted. Every
// other input it reads — the connection record's provider, kind and status,
// which tenant owns the managed credential, whether the stored token is still
// the one the run mounted, whether a live mint proof of it exists — is this
// service's own bookkeeping, and bookkeeping leaves with a status.
//
// The rows are the CROSS-PRODUCT of those inputs and the expected answer is
// COMPUTED from the row, not written beside it: a verdict iff every
// bookkeeping term holds AND the provider's map lacks the permission. So a
// term added to the handler that refuses on anything else reddens every row
// that varies it — three rounds of this review each found one more of this
// service's own facts published as a hold on a third party's pull request
// (the store and sealer, then the mint proof's identity and expiry, then the
// connection record), and each sat on the line the previous round had split.
// The first two subtests are the complement of the cross-product: the source
// count says only one site may refuse, and the sweep proves it from the TYPES
// of the records this handler reads — so a term keyed on a field nobody
// enumerated reddens there in whatever spelling it is written.
func TestForgeDeliveryPreflightRefusesOnlyFromTheProvidersAnswer(t *testing.T) {
	const stored = "ghs_runtime_secret"

	t.Run("the handler carries one refusal site, guarded on the provider's own map", func(t *testing.T) {
		src, err := os.ReadFile("forge_delivery_preflight.go")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		var sites []int
		for i, line := range lines {
			if strings.Contains(line, "deny(") && !strings.Contains(line, "deny := func(") {
				sites = append(sites, i)
			}
		}
		if len(sites) != 1 {
			t.Fatalf("the handler refuses at %d sites (lines %v), want exactly 1: a refusal is publishable as a blocking check on a third party's pull request, so the only term that may reach one is the provider's own answer", len(sites), sites)
		}
		if n := strings.Count(string(src), `"FORGE_PERMISSION_DENIED"`); n != 1 {
			t.Fatalf("the handler mints the publishable refusal code at %d sites, want exactly 1 (inside deny): the allow answer is written writeJSON(w, forgeDeliveryVerdict{...}), so a refusal in that spelling never reaches the count above", n)
		}
		guard := strings.Join(lines[max(0, sites[0]-3):sites[0]], "\n")
		if !strings.Contains(guard, "proof.Allows(") {
			t.Fatalf("the single refusal site is not guarded on the provider's recorded permission map:\n%s", guard)
		}
	})

	// The cross-product below varies the dimensions this handler reads TODAY,
	// and the source counts above see only the two spellings a refusal is
	// written in here. The guarantee is wider than both: whatever field of its
	// OWN records a term reads, the refusal it writes is publishable as a
	// blocking check on a third party's pull request, so only the provider's
	// answer may produce one. This sweep therefore takes both axes from
	// SOURCE rather than from a list: the FIELDS from the types — every field
	// of the connection record, of the managed credential and of the mint
	// proof but the permission map, which carries the one fact — and the
	// VALUES each field is moved to from the declarations (every value a named
	// type declares) and from this handler's own vocabulary (every string
	// value its source spells, on a string field and on the keys a permission
	// map really holds), plus the absent and the unexpected. Each is perturbed
	// while the provider's map grants both writes on the very token this run
	// mounted: the handler may answer the allow, or leave with any status; it
	// may not refuse. That is what reddens for a term keyed on a field nobody
	// enumerated, at a value nobody enumerated, in whatever spelling it mints
	// the refusal.
	t.Run("no field of this service's own records answers a refusal while the provider's map grants", func(t *testing.T) {
		values := forgePreflightValueOracle(t, "forge_delivery_preflight.go")
		proofType := reflect.TypeOf(secrets.TokenPermissionProof{})
		if _, ok := proofType.FieldByName("Permissions"); !ok {
			t.Fatal("the mint proof no longer carries Permissions: this sweep holds that field fixed because it is the one fact about the grant, so it must still name it")
		}
		sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := secrets.SealGenericSecret(sealer, "managed", []byte(stored))
		if err != nil {
			t.Fatal(err)
		}
		baseProof := secrets.NewTokenPermissionProof(stored,
			map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour))
		baseConn := forge.Connection{
			ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
			Status: forge.StatusActive, ManagedSecretID: "managed",
			GrantedPermissions: map[string]string{"contents": "write", "workflows": "write"},
		}
		baseRec := secrets.GenericSecret{
			ID: "managed", TenantID: "team1", ScopeTeamID: "team1", Name: "forge-runtime",
			SealedSecret: sealed, CreatedBy: "u1", CreatedAt: time.Now(), ForgeTokenProof: baseProof,
		}
		ask := func(t *testing.T, conn forge.Connection, rec secrets.GenericSecret) (int, string) {
			t.Helper()
			s, review := newForgePublishTestServer(t)
			s.sealer = sealer
			s.forgeConnections = forgePreflightVerbatimConnStore{s.forgeConnections, conn}
			s.genericSecrets = forgePreflightVerbatimSecretStore{secrets.NewMemoryGenericSecretStore(), rec}
			registerPublishToken(t, s, "grant", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			body, _ := json.Marshal(map[string]string{
				"pr_url": "https://github.com/o/r/pull/1", "credential_sha256": secrets.TokenSHA256(stored),
			})
			r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
			r.Header.Set("X-Iterion-Run", "grant")
			w := httptest.NewRecorder()
			s.handleForgeDeliveryPreflight(w, r)
			if strings.Contains(w.Body.String(), stored) || review.calls != 0 {
				t.Fatal("preflight leaked a secret or wrote to the forge")
			}
			return w.Code, w.Body.String()
		}
		mayNotRefuse := func(t *testing.T, field string, code int, body string) {
			t.Helper()
			var verdict forgeDeliveryVerdict
			_ = json.Unmarshal([]byte(body), &verdict)
			if verdict.Code != "" || strings.Contains(body, "FORGE_PERMISSION_DENIED") {
				t.Fatalf("%s answered a refusal a caller may publish as a blocking check on a third party's pull request, while the provider's recorded map grants both writes on the token this run mounted: %d %s", field, code, body)
			}
		}
		show := func(v reflect.Value) string {
			rendered := fmt.Sprintf("%v", v.Interface())
			if len(rendered) > 48 {
				rendered = rendered[:48]
			}
			return rendered
		}
		// The unperturbed row must ANSWER: a baseline that already leaves with
		// a status would make every row below pass without the handler ever
		// reaching the provider's map.
		if code, body := ask(t, baseConn, baseRec); code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("the unperturbed row does not answer the allow: %d %s", code, body)
		}
		connType := reflect.TypeOf(baseConn)
		for i := range connType.NumField() {
			name := "the connection record's " + connType.Field(i).Name
			for _, val := range forgePreflightPerturbations(t, values, name, reflect.ValueOf(baseConn).Field(i)) {
				t.Run(name+" = "+show(val), func(t *testing.T) {
					conn := baseConn
					reflect.ValueOf(&conn).Elem().Field(i).Set(val)
					code, body := ask(t, conn, baseRec)
					mayNotRefuse(t, name, code, body)
				})
			}
		}
		recType := reflect.TypeOf(baseRec)
		for i := range recType.NumField() {
			name := "the managed credential's " + recType.Field(i).Name
			for _, val := range forgePreflightPerturbations(t, values, name, reflect.ValueOf(baseRec).Field(i)) {
				t.Run(name+" = "+show(val), func(t *testing.T) {
					rec := baseRec
					reflect.ValueOf(&rec).Elem().Field(i).Set(val)
					code, body := ask(t, baseConn, rec)
					mayNotRefuse(t, name, code, body)
				})
			}
		}
		for i := range proofType.NumField() {
			// The permission map the provider answered at mint time is the one
			// fact about the grant, and the cross-product below varies it and
			// expects a verdict; here it stays granting.
			if proofType.Field(i).Name == "Permissions" {
				continue
			}
			name := "the mint proof's " + proofType.Field(i).Name
			for _, val := range forgePreflightPerturbations(t, values, name, reflect.ValueOf(*baseProof).Field(i)) {
				t.Run(name+" = "+show(val), func(t *testing.T) {
					proof := *baseProof
					reflect.ValueOf(&proof).Elem().Field(i).Set(val)
					rec := baseRec
					rec.ForgeTokenProof = &proof
					code, body := ask(t, baseConn, rec)
					mayNotRefuse(t, name, code, body)
				})
			}
		}
	})

	type connShape struct {
		name string
		// readable is a record this endpoint can read a delivery permission
		// against; every other shape is its own bookkeeping.
		apply    func(*forge.Connection)
		readable bool
	}
	type proofShape struct {
		name string
		// live is a mint proof OF the stored token that has not expired;
		// grants is its permission map carrying both writes.
		apply  func(*secrets.TokenPermissionProof) *secrets.TokenPermissionProof
		live   bool
		grants bool
	}
	conns := []connShape{
		{"a managed GitHub App whose record reads active", func(*forge.Connection) {}, true},
		{"the record flipped to needs-reauth while the run holds its token", func(c *forge.Connection) { c.Status = forge.StatusNeedsReauth }, false},
		{"the record was degraded by a permission mismatch", func(c *forge.Connection) { c.Status = forge.StatusDegraded }, false},
		{"the record carries no status at all", func(c *forge.Connection) { c.Status = "" }, false},
		{"the record names no managed credential", func(c *forge.Connection) { c.ManagedSecretID = "" }, false},
		{"the credential is an operator-pasted token, not an App's", func(c *forge.Connection) { c.Kind = forge.KindPAT }, false},
		{"the connection is not a GitHub connection", func(c *forge.Connection) { c.Provider = forge.ProviderGitLab }, false},
	}
	proofs := []proofShape{
		{"the provider granted both writes", func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof { return p }, true, true},
		{"the provider's map carries no workflows:write", func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
			delete(p.Permissions, "workflows")
			return p
		}, true, false},
		{"the provider's map carries no contents:write", func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
			delete(p.Permissions, "contents")
			return p
		}, true, false},
		{"no mint proof was ever recorded", func(*secrets.TokenPermissionProof) *secrets.TokenPermissionProof { return nil }, false, false},
		{"the proof expired while the run was still inside its hour", func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
			p.ExpiresAt = time.Now().Add(-time.Second)
			return p
		}, false, false},
		{"the proof names a token this record no longer holds", func(p *secrets.TokenPermissionProof) *secrets.TokenPermissionProof {
			p.SHA256 = secrets.TokenSHA256("a token this record no longer holds")
			return p
		}, false, false},
	}
	owners := []struct {
		name   string
		tenant string
		ours   bool
	}{
		{"the managed credential belongs to this tenant", "team1", true},
		{"the managed credential belongs to another tenant", "team2", false},
	}
	mounts := []struct {
		name  string
		token string
		same  bool
	}{
		{"the run mounted the token the record stores", stored, true},
		{"the managed credential was rotated after the run mounted its token", "ghs_the_previous_token_of_the_same_app", false},
	}

	statuses := map[int]string{}
	for _, cs := range conns {
		for _, ps := range proofs {
			for _, ow := range owners {
				for _, ms := range mounts {
					t.Run(cs.name+", "+ps.name+", "+ow.name+", "+ms.name, func(t *testing.T) {
						s, review := newForgePublishTestServer(t)
						s.sealer, _ = secrets.NewAESGCMSealer(make([]byte, 32))
						mem := secrets.NewMemoryGenericSecretStore()
						s.genericSecrets = mem
						ctx := store.WithTenant(t.Context(), "team1")
						conn, _ := s.forgeConnections.Get(ctx, "conn1")
						conn.Provider, conn.Kind, conn.Status, conn.ManagedSecretID = forge.ProviderGitHub, forge.KindGitHubApp, forge.StatusActive, "managed"
						conn.GrantedPermissions = map[string]string{"contents": "write", "workflows": "write"}
						cs.apply(&conn)
						if err := s.forgeConnections.Update(ctx, conn); err != nil {
							t.Fatal(err)
						}
						proof := ps.apply(secrets.NewTokenPermissionProof(stored,
							map[string]string{"contents": "write", "workflows": "write"}, time.Now().Add(time.Hour)))
						sealed, _ := secrets.SealGenericSecret(s.sealer, "managed", []byte(stored))
						rec := secrets.GenericSecret{ID: "managed", TenantID: ow.tenant, ScopeTeamID: ow.tenant, SealedSecret: sealed, ForgeTokenProof: proof}
						if err := mem.Create(store.WithTenant(t.Context(), ow.tenant), rec); err != nil {
							t.Fatal(err)
						}
						registerPublishToken(t, s, "grant", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

						body, _ := json.Marshal(map[string]string{
							"pr_url": "https://github.com/o/r/pull/1", "credential_sha256": secrets.TokenSHA256(ms.token),
						})
						r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/delivery-preflight", bytes.NewReader(body))
						r.Header.Set("X-Iterion-Run", "grant")
						w := httptest.NewRecorder()
						s.handleForgeDeliveryPreflight(w, r)

						if strings.Contains(w.Body.String(), stored) || review.calls != 0 {
							t.Fatal("preflight leaked a secret or wrote to the forge")
						}
						// THE RULE, computed from the row: the provider's own
						// answer about the token this run mounted, and nothing
						// else, decides whether anything is publishable.
						answered := cs.readable && ow.ours && ms.same && ps.live
						var verdict forgeDeliveryVerdict
						_ = json.Unmarshal(w.Body.Bytes(), &verdict)
						if !answered {
							// The guarantee's own assertion first, so a false
							// hold fails on the sentence written for it rather
							// than on a status comparison.
							if verdict.Code != "" || verdict.OK || strings.Contains(w.Body.String(), "FORGE_PERMISSION_DENIED") {
								t.Fatalf("this service's own bookkeeping answered a verdict a caller may publish: %d %s", w.Code, w.Body)
							}
							if w.Code < 400 || w.Code >= 600 {
								t.Fatalf("status = %d %s, want a non-verdict status: nothing was established about the credential", w.Code, w.Body)
							}
							// The probe reports only that nobody answered, so
							// this sentence is the whole diagnosis of WHICH of
							// this service's own facts is missing: it is what
							// an operator reads in the server's log.
							var envelope struct {
								Error string `json:"error"`
							}
							_ = json.Unmarshal(w.Body.Bytes(), &envelope)
							if strings.TrimSpace(envelope.Error) == "" {
								t.Fatalf("status %d carries no reason: %s", w.Code, w.Body)
							}
							statuses[w.Code] = cs.name + " / " + ps.name + " / " + ow.name + " / " + ms.name
							return
						}
						if w.Code != http.StatusOK {
							t.Fatalf("status = %d %s, want 200: the provider answered about this very token", w.Code, w.Body)
						}
						if ps.grants {
							if !verdict.OK || verdict.Code != "" {
								t.Fatalf("the provider granted both writes and the answer refuses: %+v", verdict)
							}
							return
						}
						if verdict.OK || verdict.Code != "FORGE_PERMISSION_DENIED" || !strings.Contains(verdict.Reason, ":write is missing or unverified") {
							t.Fatalf("the one fact about the grant no longer answers a verdict: %+v", verdict)
						}
					})
				}
			}
		}
	}
	// Every non-verdict status this endpoint can produce owes an end-to-end row
	// through the real compiled probe at both carriers — the probe reads any
	// non-200 as "the service did not answer", and that is what must be proven
	// rather than assumed (bots/delivery_probe_fault_matrix_test.go).
	for _, code := range []int{http.StatusForbidden, http.StatusConflict, http.StatusServiceUnavailable} {
		if _, ok := statuses[code]; !ok {
			t.Errorf("no row of the cross-product answered %d; the matrix carries an end-to-end row for it, so either the row or the handler moved", code)
		}
		delete(statuses, code)
	}
	for code, row := range statuses {
		t.Errorf("the handler answered %d (%s), a status no end-to-end row of bots/delivery_probe_fault_matrix_test.go drives through the real probe: add the row, then list it here", code, row)
	}
}
