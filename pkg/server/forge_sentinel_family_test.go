package server

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
)

// forgeSentinelFamily pins one pkg/forge sentinel to the side of the
// ErrNotFound class it belongs to, and to what forgeUpstreamStatus therefore
// answers for it. Both facts are the SAME fact — membership is what the
// classifier reads — which is why they are pinned together: a doc line saying
// "outside the class" and a 404 coming out of the table cannot both be true.
type forgeSentinelFamily struct {
	err error
	// inNotFoundClass is what errors.Is(err, forge.ErrNotFound) must answer.
	inNotFoundClass bool
	// wantStatus is what forgeUpstreamStatus must answer. 0 = "not an answer
	// from the forge", which each handler renders with its own fault status.
	wantStatus int
	// why is what the reader of a failure needs: the consequence of the
	// family, not a restatement of it.
	why string
}

// forgeSentinelFamilies is the table documented next to forgeUpstreamStatus.
// It is keyed by the sentinel's declared name, qualified by the package
// directory it lives in, so the completeness sweep below can compare it to
// what pkg/forge actually declares.
var forgeSentinelFamilies = map[string]forgeSentinelFamily{
	"forge.ErrNotFound": {
		err: forge.ErrNotFound, inNotFoundClass: true, wantStatus: http.StatusNotFound,
		why: "the class itself",
	},
	"forge.ErrHookNotFound": {
		err: forge.ErrHookNotFound, inNotFoundClass: true, wantStatus: http.StatusNotFound,
		why: "a forge-answered 404 the orchestrator also reads as 'the hook is already gone'",
	},

	// Prepared but never sent. Deliberately 0 here: this table classifies
	// what the FORGE answered, and the forge answered nothing. The 500 is
	// writeForgeUpstreamError's, via isIterionFault — a status in this
	// column would make the classifier decide fault, which is not its job.
	"forge.ErrLocalPreflight": {
		err: forge.ErrLocalPreflight, wantStatus: 0,
		why: "iterion's own preparation failed before any byte reached the network — writeForgeUpstreamError renders it 500, not the 502 its route defaults to",
	},

	// Store misses. Every "…but could not be recorded on connection X: %w"
	// wrap in the forge layer carries one of these; in the class they would
	// all answer 404 for a write iterion itself failed.
	"forge.ErrConnectionNotFound": {
		err: forge.ErrConnectionNotFound, wantStatus: 0,
		why: "an iterion store miss — the avatar, provisioning and connection routes %w-wrap it into their own failure messages",
	},
	"forge.ErrIntegrationNotFound": {
		err: forge.ErrIntegrationNotFound, wantStatus: 0,
		why: "an iterion store miss — the provisioning routes match it themselves",
	},
	"forge.ErrOAuthAppNotFound": {
		err: forge.ErrOAuthAppNotFound, wantStatus: 0,
		why: "an iterion store miss",
	},
	"forge.ErrBoardBindingNotFound": {
		err: forge.ErrBoardBindingNotFound, wantStatus: 0,
		why: "an iterion store miss — the board routes and the sync worker match it themselves",
	},
	"forge.ErrProvisionApprovalNotFound": {
		err: forge.ErrProvisionApprovalNotFound, wantStatus: 0,
		why: "an iterion store miss",
	},

	// Forge-answered 404s that stay out of the class because their own
	// caller classifies them. Moving one in is a behaviour change to own.
	"forge.ErrProjectNotFound": {
		err: forge.ErrProjectNotFound, wantStatus: 0,
		why: "a board bind answers the operator's own board ref; a bare 404 sends them to re-check a number that was right",
	},
	"forge.ErrFileNotFound": {
		err: forge.ErrFileNotFound, wantStatus: 0,
		why: "config-share answers every read failure alike",
	},

	// Not a 404 in any reading. The first three have their own arm in
	// forgeUpstreamStatus; the rest are not answers from the forge at all.
	"forge.ErrForbidden": {
		err: forge.ErrForbidden, wantStatus: http.StatusForbidden,
		why: "refused, with no permission named",
	},
	"forge.ErrUnauthorized": {
		err: forge.ErrUnauthorized, wantStatus: http.StatusUnprocessableEntity,
		why: "the credential was rejected — reconnect, never retry",
	},
	"forge.ErrPermissionsNotGranted": {
		err: forge.ErrPermissionsNotGranted, wantStatus: http.StatusUnprocessableEntity,
		why: "an installation approved too narrowly — an org admin re-approves",
	},
	"forge.ErrOAuthAppExists": {
		err: forge.ErrOAuthAppExists, wantStatus: 0,
		why: "an iterion store collision",
	},
	"forge.ErrRepoExists": {
		err: forge.ErrRepoExists, wantStatus: 0,
		why: "a create-time name collision the connections route answers 409 itself",
	},
	"forge.ErrFileConflict": {
		err: forge.ErrFileConflict, wantStatus: 0,
		why: "a stale-sha write the config-share route answers 409 itself",
	},
	"forge.ErrBoardSyncLeaseLost": {
		err: forge.ErrBoardSyncLeaseLost, wantStatus: 0,
		why: "an iterion lease overrun",
	},
	"forge.ErrAvatarUnsupported": {
		err: forge.ErrAvatarUnsupported, wantStatus: 0,
		why: "a capability gap in the forge build, not an answer to a request",
	},
	"forge.ErrSecurityReadMalformed": {
		err: forge.ErrSecurityReadMalformed, wantStatus: 0,
		why: "an operator's hand-set secret in the wrong shape",
	},
	"forge.ErrSecurityReadNoOrgKey": {
		err: forge.ErrSecurityReadNoOrgKey, wantStatus: 0,
		why: "a connection with no org to key its token by",
	},
	"forge/github.ErrInstallationNotOwned": {
		err: forgegithub.ErrInstallationNotOwned, wantStatus: 0,
		why: "a forged installation_id in a callback — the connect route answers 403 itself",
	},
}

// forgeSentinelRoot is the tree the sweep walks. Everything under it, provider
// subpackages included: a sentinel anywhere in pkg/forge reaches
// forgeUpstreamStatus the moment a handler %w-wraps it, so the sweep discovers
// packages instead of listing them — a list would go quietly stale on the next
// provider added.
const forgeSentinelRoot = "../forge"

// Nothing in pkg/forge states that the store-miss sentinels do not wrap
// ErrNotFound — and the day one does, every store-failure %w-wrap in the
// forge layer starts classifying as a 404 through forgeUpstreamStatus, with
// no message changing anywhere and no other test failing. This is that
// statement, pinned BOTH ways: the members must stay members, and the
// non-members must stay out.
//
// The assertion is the relationship (what errors.Is answers) and its
// consequence (what the real classifier returns), never the presence of a
// doc line.
func TestForgeSentinelFamilies_ArePinned(t *testing.T) {
	for _, name := range sortedSentinelNames() {
		f := forgeSentinelFamilies[name]
		t.Run(name, func(t *testing.T) {
			if got := errors.Is(f.err, forge.ErrNotFound); got != f.inNotFoundClass {
				t.Fatalf("errors.Is(%s, forge.ErrNotFound) = %v, want %v — %s changed family.\n"+
					"A sentinel is in the ErrNotFound class iff it wraps it, and membership is the whole meaning: "+
					"forgeUpstreamStatus answers a member 404.\nThis one: %s.\n"+
					"If the move is deliberate, change this row and the table next to forgeUpstreamStatus, "+
					"and check every %%w-wrap that carries this sentinel to a handler.",
					name, got, f.inNotFoundClass, name, f.why)
			}
			got, _ := forgeUpstreamStatus(f.err)
			if got != f.wantStatus {
				t.Fatalf("forgeUpstreamStatus(%s) = %d, want %d — %s", name, got, f.wantStatus, f.why)
			}
		})
	}
}

// A sentinel added to pkg/forge without a family stated here is the same
// defect one commit earlier: the table next to forgeUpstreamStatus would
// describe a package that has moved on. The sweep reads the declarations, so
// the new sentinel is red until someone says which side it is on.
func TestForgeSentinelFamilies_CoverEveryDeclaredSentinel(t *testing.T) {
	declared := declaredForgeSentinels(t)
	var missing, stale []string
	for name := range declared {
		if _, ok := forgeSentinelFamilies[name]; !ok {
			missing = append(missing, name+" ("+declared[name]+")")
		}
	}
	for name := range forgeSentinelFamilies {
		if _, ok := declared[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("pkg/forge declares %d sentinel(s) with no family stated: %s\n"+
			"Add a row to forgeSentinelFamilies saying whether it wraps forge.ErrNotFound "+
			"(→ forgeUpstreamStatus answers it 404) and mirror it in the table next to forgeUpstreamStatus.",
			len(missing), strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("forgeSentinelFamilies names %d sentinel(s) pkg/forge no longer declares: %s\n"+
			"Drop the row and the matching line in the table next to forgeUpstreamStatus.",
			len(stale), strings.Join(stale, ", "))
	}
}

// The site the invariant protects, driven from the real handler: the upload
// succeeded and the connection STORE refused the record. That failure is
// iterion's own, and it reaches the operator through a %w-wrap carrying
// forge.ErrConnectionNotFound. In the ErrNotFound class it would be answered
// 404 — "the forge has no such thing" for a write the forge never saw, on a
// request whose avatar did land.
func TestForgeConnectionAvatar_StoreFailureIsNotAForge404(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeConnections = &recordRefusingStore{ConnectionStore: s.forgeConnections, err: forge.ErrConnectionNotFound}
	gl := &mockGitLabAvatar{bot: true}
	srv := gl.server()
	defer srv.Close()
	seedAvatarConn(t, s, forge.Connection{ID: "c-store", Provider: forge.ProviderGitLab, Kind: forge.KindPAT,
		AccountLogin: "group_1_bot_x", AccountKind: forge.AccountKindBot, ForgeBaseURL: srv.URL})

	w := avatarReq(s, "c-store", "")

	if gl.count() != 1 {
		t.Fatalf("the upload did not happen (%d), so this is not the failure under test", gl.count())
	}
	if !strings.Contains(w.Body.String(), "avatar uploaded but could not be recorded") {
		t.Fatalf("reached a different failure than the record one: code=%d body=%s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusNotFound {
		t.Fatalf("a connection-store failure was answered 404: forge.ErrConnectionNotFound now satisfies "+
			"errors.Is(_, forge.ErrNotFound), so forgeUpstreamStatus reads this wrap as the forge saying "+
			"it has no such thing — for a write iterion failed, after an avatar that DID land. body=%s", w.Body.String())
	}
	// 5xx, not an exact code: what this guard proves is the CLASS — the store
	// failed, so the answer belongs to iterion's side and not to the forge.
	// The route renders that arm 502, which is arguable (a Bad Gateway for a
	// write iterion itself failed) but shared by the five sites that fall
	// through forgeUpstreamStatus, four of which are genuinely upstream.
	// Tightening it is a behaviour change of its own — see #969.
	if w.Code < 500 {
		t.Fatalf("code=%d, want a 5xx: the store failed, which is iterion's side, not an answer the forge gave. body=%s",
			w.Code, w.Body.String())
	}
}

// recordRefusingStore is the memory store with one behaviour: the write that
// records an outcome fails. Reads still work, so the handler resolves the
// connection and reaches the record step.
type recordRefusingStore struct {
	forge.ConnectionStore
	err error
}

func (r *recordRefusingStore) Update(context.Context, forge.Connection) error { return r.err }

func sortedSentinelNames() []string {
	names := make([]string, 0, len(forgeSentinelFamilies))
	for name := range forgeSentinelFamilies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// declaredForgeSentinels reads the package-level `Err*` vars declared anywhere
// under pkg/forge, keyed "<package dir>.<Name>" and valued by the file that
// declares them. Parsing the declarations — rather than listing them — is what
// makes an addition loud.
//
// It reads VARS, which is the shape a tidy-up moves between families by
// changing errors.New into fmt.Errorf("%w: …"). A typed error joins the class
// by carrying an Unwrap (*forge.NotFoundError does), which is a deliberate act
// on a method rather than an edit to a declaration; those are covered by the
// rows above naming the type, not by this sweep.
func declaredForgeSentinels(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(forgeSentinelRoot, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if file != forgeSentinelRoot && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		qualifier := strings.TrimPrefix(filepath.ToSlash(filepath.Dir(file)), "../")
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					if strings.HasPrefix(id.Name, "Err") {
						found[qualifier+"."+id.Name] = filepath.ToSlash(file)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep %s: %v — the sweep must read pkg/forge to guard it", forgeSentinelRoot, err)
	}
	if len(found) == 0 {
		t.Fatal("the sweep found no sentinel at all — it is not reading pkg/forge, so it cannot go red")
	}
	return found
}

// docRowsCoveredElsewhere are rows of the doc table with no forgeSentinelFamilies
// entry, each for a stated reason. A TYPE joins the ErrNotFound class by carrying
// an Unwrap method, not by an edit to a var declaration, so it is pinned by
// TestForgeUpstreamStatus_Table against a constructed value instead of by the
// sweep below.
var docRowsCoveredElsewhere = map[string]string{
	"*forge.NotFoundError": "a type; TestForgeUpstreamStatus_Table pins it",
}

// TestForgeSentinelDoc_MatchesTheTable pins the two hand-written copies of the
// same 20 rows against each other: the table a reader finds next to
// forgeUpstreamStatus, and forgeSentinelFamilies, which is what the guards
// above actually assert.
//
// Without this, the harmful drift is not a missing row — it is a family MOVED
// in the map and not in the prose, which leaves the comment stating the exact
// opposite of what the classifier does. That is the failure this whole file
// exists to prevent, one level up: a doc line outliving the behaviour it
// describes.
//
// It compares SETS OF NAMES, never wording — the prose beside each row is free
// to say whatever serves its reader.
func TestForgeSentinelDoc_MatchesTheTable(t *testing.T) {
	inClass, outOfClass := parseForgeSentinelDoc(t)

	doc := map[string]bool{}
	for n := range inClass {
		doc[n] = true
	}
	for n := range outOfClass {
		if _, dup := doc[n]; dup {
			t.Fatalf("%s appears under BOTH headings of the doc table next to forgeUpstreamStatus", n)
		}
		doc[n] = false
	}

	for name, f := range forgeSentinelFamilies {
		side, listed := doc[name]
		if !listed {
			t.Errorf("%s is pinned in forgeSentinelFamilies but absent from the doc table next to "+
				"forgeUpstreamStatus — a reader of the classifier cannot see it.", name)
			continue
		}
		if side != f.inNotFoundClass {
			t.Errorf("%s: the doc table next to forgeUpstreamStatus files it %s the ErrNotFound class, "+
				"the pinned table says %s. One of the two is now lying to its reader; the guards above "+
				"assert the pinned one, so the prose is what a moved family leaves behind.",
				name, docSide(side), docSide(f.inNotFoundClass))
		}
		delete(doc, name)
	}
	for name := range doc {
		if _, known := docRowsCoveredElsewhere[name]; known {
			continue
		}
		t.Errorf("%s is in the doc table next to forgeUpstreamStatus but not in forgeSentinelFamilies — "+
			"nothing asserts the side it claims. Add a row, or list it in docRowsCoveredElsewhere with "+
			"what does pin it.", name)
	}
}

func docSide(in bool) string {
	if in {
		return "IN"
	}
	return "OUTSIDE"
}

// parseForgeSentinelDoc reads the doc table out of forge_upstream_status.go's
// source. It reads the file the package is built from rather than a copy, so
// what it certifies is the comment a reader of the classifier actually finds.
func parseForgeSentinelDoc(t *testing.T) (inClass, outOfClass map[string]struct{}) {
	t.Helper()
	src, err := os.ReadFile("forge_upstream_status.go")
	if err != nil {
		t.Fatalf("read the classifier's source: %v", err)
	}
	inClass, outOfClass = map[string]struct{}{}, map[string]struct{}{}
	var current map[string]struct{}
	started := false
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(line, "//") {
			if started {
				break // the comment block ended: the table is above the func.
			}
			continue
		}
		body := strings.TrimPrefix(line, "//")
		trimmed := strings.TrimSpace(body)
		switch {
		case strings.HasPrefix(trimmed, "IN the class"):
			current, started = inClass, true
			continue
		case strings.HasPrefix(trimmed, "OUTSIDE it"):
			current, started = outOfClass, true
			continue
		}
		if current == nil || trimmed == "" {
			continue
		}
		// A row is indented under its heading; a paragraph is not.
		if !strings.HasPrefix(body, "\t  ") {
			current = nil
			continue
		}
		name := strings.Fields(trimmed)[0]
		current[name] = struct{}{}
	}
	if !started {
		t.Fatal("no sentinel table found in forge_upstream_status.go — the headings this test reads " +
			"(\"IN the class\" / \"OUTSIDE it\") were renamed or the table was removed.")
	}
	if len(inClass) == 0 || len(outOfClass) == 0 {
		t.Fatalf("parsed %d row(s) IN and %d OUTSIDE: the table's layout changed under this parser",
			len(inClass), len(outOfClass))
	}
	return inClass, outOfClass
}
