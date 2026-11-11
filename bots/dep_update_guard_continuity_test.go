package bots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// depUpdateGuardGreenVerdicts is the verdict set every reader treats as
// green: the gate table (blocking 0), arm_automerge's green set, and the
// verdicts continuity may carry. One list, so a reader that forgets one of
// them reddens the test that reads it.
var depUpdateGuardGreenVerdicts = []string{"clean", "committed", "carried"}

const (
	depUpdateGuardBot = "dep-update-guard/main.bot"
	identityOpen      = "# >>> vetty-change-identity/v1"
	identityClose     = "# <<< vetty-change-identity/v1"
	historyOpen       = "# >>> vetty-gate-history/v1"
	historyClose      = "# <<< vetty-gate-history/v1"
	// The App the stub forge says the run token speaks as, and its user id.
	vettyLogin = "acme-vetty[bot]"
	vettyID    = 4242
)

// identityBlock extracts the identity contract block a node carries.
func identityBlock(t *testing.T, node string) string {
	t.Helper()
	return sentinelBlock(t, node, identityOpen, identityClose)
}

// sentinelBlock extracts the text a node carries between two sentinels,
// both included, each present exactly once.
func sentinelBlock(t *testing.T, node, open, closing string) string {
	t.Helper()
	script := toolScript(t, depUpdateGuardBot, node)
	if strings.Count(script, open) != 1 || strings.Count(script, closing) != 1 {
		t.Fatalf("%s carries %d %q and %d %q sentinels, want exactly one of each",
			node, strings.Count(script, open), open, strings.Count(script, closing), closing)
	}
	i, j := strings.Index(script, open), strings.Index(script, closing)
	if j < i {
		t.Fatalf("%s: the %q sentinels are out of order", node, open)
	}
	return script[i : j+len(closing)]
}

// changeIdentity runs the contract block continuity carries — the code
// under test, not a re-implementation — and returns (id, reason).
func changeIdentity(t *testing.T, ws, baseRef, baseRev, sha string, env ...string) (string, string) {
	t.Helper()
	prog := "import json, os, sys\n" + identityBlock(t, "continuity") +
		"\nprint(json.dumps(change_identity(sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4])))\n"
	path := filepath.Join(t.TempDir(), "identity.py")
	if err := os.WriteFile(path, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", path, ws, baseRef, baseRev, sha)
	cmd.Env = append(gittest.Env(), env...)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("identity block: %v: %s", err, raw)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 2 {
		t.Fatalf("identity block output %q: %v", raw, err)
	}
	return got[0], got[1]
}

// depsYAML is a 40-line manifest, with the given 1-based lines replaced.
func depsYAML(over map[int]string) []byte {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		line, ok := over[i]
		if !ok {
			line = fmt.Sprintf("key%02d: v%02d", i, i)
		}
		b.WriteString(line + "\n")
	}
	return []byte(b.String())
}

// commitTree commits the given files on top of parent, detached, and
// returns the new commit; nil content removes the file.
func commitTree(t *testing.T, ws, parent, msg string, files map[string][]byte) string {
	t.Helper()
	gittest.Run(t, ws, "switch", "-q", "--detach", parent)
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		full := filepath.Join(ws, p)
		if files[p] == nil {
			gittest.Run(t, ws, "rm", "-q", p)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, files[p], 0o600); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, ws, "add", p)
	}
	gittest.Run(t, ws, "commit", "-q", "--allow-empty", "-m", msg)
	return gittest.Run(t, ws, "rev-parse", "HEAD")
}

// TestDepUpdateGuardChangeIdentityContract pins identity contract v1 on real
// git: what must keep the id (a pure rebase) and what must change it
// (anything that can change the meaning of the change). Each case on its own
// repository.
//
// Mutations (main.bot, both copies of the block) and the case each reddens:
// --verbatim -> --stable (the re-indent case); drop -O<devnull> (the
// diff.orderFile case); drop base_ref from the hash (the base_ref case); drop
// GIT_NO_REPLACE_OBJECTS (the replace-ref case); drop GIT_GRAFT_FILE (the
// graft case); drop GIT_ATTR_SOURCE and attr.tree (the .gitattributes case).
func TestDepUpdateGuardChangeIdentityContract(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	// base: the manifest and an unrelated file; the PR bumps line 20.
	fixture := func(t *testing.T) (ws, base string) {
		t.Helper()
		ws = t.TempDir()
		gittest.InitRepo(t, ws)
		base = commitTree(t, ws, "HEAD", "base", map[string][]byte{
			"deps.yaml": depsYAML(nil), "other.txt": []byte("one\n"),
		})
		gittest.Run(t, ws, "branch", "-f", "main", base)
		return ws, base
	}
	bump := map[int]string{20: "key20: v99"}
	id := func(t *testing.T, ws, sha string) string {
		t.Helper()
		got, why := changeIdentity(t, ws, "main", "main", sha)
		if got == "" {
			t.Fatalf("no identity for %s: %s", sha[:12], why)
		}
		return got
	}

	t.Run("a pure rebase keeps the id", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			moved map[string][]byte
		}{
			{"the base moves in another file", map[string][]byte{"other.txt": []byte("two\n")}},
			{"the base moves 15 lines away in the same file", map[string][]byte{"deps.yaml": depsYAML(map[int]string{35: "key35: moved"})}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ws, base := fixture(t)
				x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
				before := id(t, ws, x)
				moved := commitTree(t, ws, base, "base moves", tc.moved)
				gittest.Run(t, ws, "branch", "-f", "main", moved)
				after := moved
				content := depsYAML(bump)
				if _, sameFile := tc.moved["deps.yaml"]; sameFile {
					content = depsYAML(map[int]string{20: "key20: v99", 35: "key35: moved"})
				}
				y := commitTree(t, ws, after, "bump, rebased", map[string][]byte{"deps.yaml": content})
				if got := id(t, ws, y); got != before {
					t.Errorf("a pure rebase changed the id: %s before, %s after", before, got)
				}
			})
		}
	})

	t.Run("a base edit on a context line, then the change re-applied, changes the id", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		before := id(t, ws, x)
		moved := commitTree(t, ws, base, "base edits line 18", map[string][]byte{"deps.yaml": depsYAML(map[int]string{18: "key18: edited"})})
		gittest.Run(t, ws, "branch", "-f", "main", moved)
		y := commitTree(t, ws, moved, "bump, re-applied", map[string][]byte{"deps.yaml": depsYAML(map[int]string{18: "key18: edited", 20: "key20: v99"})})
		if got := id(t, ws, y); got == before {
			t.Errorf("a base change inside the hunk's context kept the id %s: a conflict-adjacent rebase would carry a verdict about another text", got)
		}
	})

	// --stable strips whitespace and reads a re-indented YAML block as the
	// same change, although the re-indent changes what the YAML means.
	t.Run("a YAML re-indent changes the id", func(t *testing.T) {
		ws, base := fixture(t)
		flat := commitTree(t, ws, base, "value bump", map[string][]byte{"chart.yaml": []byte("b:\n  c: 3\n")})
		gittest.Run(t, ws, "branch", "-f", "main", base)
		deep := commitTree(t, ws, base, "value bump, re-indented", map[string][]byte{"chart.yaml": []byte("b:\n    c: 3\n")})
		if a, b := id(t, ws, flat), id(t, ws, deep); a == b {
			t.Errorf("a re-indent kept the id %s: whitespace that changes the meaning was stripped", a)
		}
	})

	t.Run("other binary bytes on the same path change the id", func(t *testing.T) {
		ws, base := fixture(t)
		a := commitTree(t, ws, base, "blob a", map[string][]byte{"img/logo.bin": {0, 1, 2, 3, 0, 'a'}})
		b := commitTree(t, ws, base, "blob b", map[string][]byte{"img/logo.bin": {0, 1, 2, 3, 0, 'b'}})
		if x, y := id(t, ws, a), id(t, ws, b); x == y {
			t.Errorf("two different binaries share the id %s", x)
		}
	})

	// A binary section makes the patch-id sum depend on the order of the
	// files around it, so the order is pinned: a repository's own
	// diff.orderFile must not decide the id.
	t.Run("a repository diff.orderFile does not move the id", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "three files", map[string][]byte{
			"a.txt": []byte("alpha\n"), "m.bin": {0, 9, 8, 0, 7}, "z.txt": []byte("omega\n"),
		})
		before := id(t, ws, x)
		order := filepath.Join(t.TempDir(), "order")
		if err := os.WriteFile(order, []byte("z.txt\nm.bin\na.txt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, ws, "config", "diff.orderFile", order)
		if got := id(t, ws, x); got != before {
			t.Errorf("diff.orderFile moved the id from %s to %s", before, got)
		}
	})

	t.Run("another base_ref changes the id", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		onMain, _ := changeIdentity(t, ws, "main", "main", x)
		onRelease, _ := changeIdentity(t, ws, "release", "main", x)
		if onMain == "" || onMain == onRelease {
			t.Errorf("the base_ref is not folded into the id: %q on main, %q on release", onMain, onRelease)
		}
	})

	// Only the commit objects decide. What a repository layers over them —
	// a replace ref, a graft, a .gitattributes — is local state, and the id
	// is compared across repositories: a workspace and the scratch repository
	// post_feedback fills from the forge.
	t.Run("a replace ref does not move the id", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		other := commitTree(t, ws, base, "another bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v77"})})
		before := id(t, ws, other)
		gittest.Run(t, ws, "replace", other, x)
		if got := id(t, ws, other); got != before {
			t.Errorf("a replace ref made %s read as %s: id %s, want %s", other[:12], x[:12], got, before)
		}
	})
	t.Run("a graft does not move the id", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		moved := commitTree(t, ws, base, "base moves", map[string][]byte{"other.txt": []byte("two\n")})
		gittest.Run(t, ws, "branch", "-f", "main", moved)
		before := id(t, ws, x)
		// Grafted onto the moved base, x's merge-base would become that base
		// and its diff would revert the base's change.
		grafts := filepath.Join(gittest.Run(t, ws, "rev-parse", "--absolute-git-dir"), "info", "grafts")
		if err := os.MkdirAll(filepath.Dir(grafts), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grafts, []byte(x+" "+moved+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := id(t, ws, x); got != before {
			t.Errorf("a graft re-parented %s: id %s, want %s", x[:12], got, before)
		}
	})
	t.Run("a .gitattributes in the tree does not move the id across repositories", func(t *testing.T) {
		ws := t.TempDir()
		gittest.InitRepo(t, ws)
		base := commitTree(t, ws, "HEAD", "base", map[string][]byte{
			"deps.yaml": depsYAML(nil), ".gitattributes": []byte("deps.yaml -diff\n"),
		})
		gittest.Run(t, ws, "branch", "-f", "main", base)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		inWorkspace := id(t, ws, x)
		// A bare repository holding the same commits and no checkout: the
		// shape of the one post_feedback fills from the forge.
		bare := filepath.Join(t.TempDir(), "scratch.git")
		gittest.Run(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
		gittest.Run(t, bare, "fetch", "-q", ws, base+":refs/vetty/base", x+":refs/vetty/head")
		inScratch, why := changeIdentity(t, bare, "main", "refs/vetty/base", x)
		if inScratch == "" || inScratch != inWorkspace {
			t.Errorf("the same commits got %s in the workspace and %q in a bare repository (%s): the tree's attributes moved the id",
				inWorkspace, inScratch, why)
		}
	})

	t.Run("no merge-base, no identity", func(t *testing.T) {
		ws, _ := fixture(t)
		gittest.Run(t, ws, "switch", "-q", "--orphan", "unrelated")
		if err := os.WriteFile(filepath.Join(ws, "x.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, ws, "add", "x.txt")
		gittest.Run(t, ws, "commit", "-q", "-m", "unrelated root")
		lone := gittest.Run(t, ws, "rev-parse", "HEAD")
		if got, why := changeIdentity(t, ws, "main", "main", lone); got != "" || !strings.Contains(why, "no merge-base") {
			t.Errorf("an unrelated head got the identity %q (%s)", got, why)
		}
	})

	// The sandbox image's git is the one the bot runs. One without
	// --verbatim has no identity — never a fallback to the whitespace-blind
	// --stable — and says why.
	t.Run("a git without patch-id --verbatim has no identity, and says so", func(t *testing.T) {
		ws, base := fixture(t)
		x := commitTree(t, ws, base, "bump", map[string][]byte{"deps.yaml": depsYAML(bump)})
		real, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		shim := t.TempDir()
		script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = --verbatim ]; then\n" +
			"    echo 'usage: git patch-id [--stable | --unstable]' >&2\n    exit 129\n  fi\ndone\nexec " + real + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		got, why := changeIdentity(t, ws, "main", "main", x, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		if got != "" {
			t.Fatalf("a git without --verbatim produced the identity %s", got)
		}
		if !strings.Contains(why, "patch-id --verbatim") {
			t.Errorf("the reason does not name the missing capability: %q", why)
		}
	})
}

// contFixture is a workspace on the head a synchronize launch reads: X is
// the bump as first audited, the base then moved, and Y is the same bump
// rebased onto it. Local main is the moved base, as a fresh clone has it.
type contFixture struct {
	ws, x, y, base1 string
	id              string
}

func newContFixture(t *testing.T) contFixture {
	t.Helper()
	ws := t.TempDir()
	gittest.InitRepo(t, ws)
	base0 := commitTree(t, ws, "HEAD", "base", map[string][]byte{"deps.yaml": depsYAML(nil), "other.txt": []byte("one\n")})
	x := commitTree(t, ws, base0, "bump", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
	base1 := commitTree(t, ws, base0, "base moves", map[string][]byte{"other.txt": []byte("two\n")})
	gittest.Run(t, ws, "branch", "-f", "main", base1)
	y := commitTree(t, ws, base1, "bump, rebased", map[string][]byte{"deps.yaml": depsYAML(map[int]string{20: "key20: v99"})})
	gittest.Run(t, ws, "switch", "-q", "-C", "renovate/bump", y)
	idY, why := changeIdentity(t, ws, "main", "main", y)
	idX, _ := changeIdentity(t, ws, "main", "main", x)
	if idY == "" || idY != idX {
		t.Fatalf("fixture: the rebase is not identity-equal (%q vs %q): %s", idX, idY, why)
	}
	return contFixture{ws: ws, x: x, y: y, base1: base1, id: idY}
}

// contForge is the GitHub REST/GraphQL surface continuity reads.
type contForge struct {
	t        *testing.T
	mu       sync.Mutex
	head     string
	baseRef  string
	reviews  []map[string]any
	endless  bool // every reviews page says rel="next"
	statuses map[string][]map[string]any
	// viewer answers GraphQL's `viewer`; nil is the App as REST spells it.
	viewer map[string]any
	seen   []string
}

func (f *contForge) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

func (f *contForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.RequestURI())
	if got := r.Header.Get("Authorization"); got != "Bearer ghs_test" {
		f.t.Errorf("continuity read the forge with Authorization %q, want the run token", got)
	}
	if r.Method != http.MethodGet && r.URL.Path != "/graphql" {
		f.t.Errorf("continuity WROTE to the forge: %s %s — it reads only", r.Method, r.URL.Path)
	}
	const prefix = "/repos/acme/widgets/"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		viewer := f.viewer
		if viewer == nil {
			viewer = map[string]any{"login": vettyLogin, "databaseId": vettyID}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": viewer}})
	case r.URL.Path == prefix+"pulls/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": f.head}, "base": map[string]any{"ref": f.baseRef}})
	case r.URL.Path == prefix+"pulls/7/reviews":
		if f.endless {
			w.Header().Set("Link", `<https://api.github.com/x?page=99>; rel="next"`)
		}
		_ = json.NewEncoder(w).Encode(f.reviews)
	case strings.HasPrefix(r.URL.Path, prefix+"commits/") && strings.HasSuffix(r.URL.Path, "/status"):
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"commits/"), "/status")
		st := f.statuses[sha]
		if st == nil {
			st = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "statuses": st, "total_count": len(st)})
	default:
		f.t.Errorf("continuity asked the forge an unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func gateMarker(sha, id, verdict, origin, at string) string {
	return "<!-- vetty-gate/v1 sha=" + sha + " id=" + id + " verdict=" + verdict +
		" origin=" + origin + " origin_at=" + at + " -->"
}

func reviewURL(n int) string {
	return fmt.Sprintf("https://github.com/acme/widgets/pull/7#pullrequestreview-%d", n)
}

// vettyReview is a review as post_feedback publishes it: the comment, the
// footer, and the marker on the last line. The App's own reviews carry its
// user id; any other author another one.
func vettyReview(n int, author, marker string) map[string]any {
	body := "**✅ Safe bump — nothing left to align**\n\n## What Vetty did\nNothing left to align.\n\n" +
		"_🤖 [iterion](https://iterion.cloud) — Vetty, deterministic feedback._\n\n" + marker
	uid := 9000 + n
	if author == vettyLogin {
		uid = vettyID
	}
	return map[string]any{"id": n, "user": map[string]any{"login": author, "type": "Bot", "id": uid},
		"body": body, "html_url": reviewURL(n)}
}

func gateStatus(state, description, target string) map[string]any {
	return map[string]any{"context": "vetty/deps", "state": state, "description": description, "target_url": target}
}

// runContinuity drives the continuity node against a stub forge.
func runContinuity(t *testing.T, ws, head string, forge http.Handler, over map[string]string) map[string]any {
	t.Helper()
	srv := httptest.NewServer(forge)
	defer srv.Close()
	enc := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	subs := map[string]string{
		"{{vars.workspace_dir}}":       enc(ws),
		"{{vars.base_ref}}":            `"main"`,
		"{{vars.pr_url}}":              `"https://github.com/acme/widgets/pull/7"`,
		"{{vars.continuity}}":          "true",
		"{{vars.carry_max_age_h}}":     "72",
		"{{vars.gate_enabled}}":        "true",
		"{{vars.gate_context}}":        `"vetty/deps"`,
		"{{secrets.forge_token.path}}": `"ghs_test"`,
		"{{input.head_sha}}":           enc(head),
		"{{input.degraded}}":           "false",
	}
	for k, v := range over {
		subs[k] = v
	}
	body := toolScript(t, depUpdateGuardBot, "continuity")
	for ref, val := range subs {
		body = strings.ReplaceAll(body, ref, val)
	}
	if !strings.Contains(body, `"https://api.github.com"`) {
		t.Fatal("continuity no longer names the GitHub API the stub stands in for")
	}
	body = strings.ReplaceAll(body, `"https://api.github.com"`, `"`+srv.URL+`"`)
	if strings.Contains(body, "{{") {
		t.Fatalf("unsubstituted ref: %s", firstRef(body))
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "continuity.py")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", path)
	cmd.Dir = dir
	cmd.Env = gittest.Env()
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("continuity: %v: %s", err, raw)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("continuity output is not continuity_output JSON: %v (%q)", err, raw)
	}
	exclusive := 0
	for _, k := range []string{"carry", "already_gated"} {
		if res[k] == true {
			exclusive++
		}
	}
	if exclusive > 1 {
		t.Fatalf("continuity answered both carry and already_gated: %v", res)
	}
	if s, _ := res["reason"].(string); s == "" {
		t.Fatalf("continuity omitted its reason: %v", res)
	}
	return res
}

func utcAgo(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format("2006-01-02T15:04:05Z")
}

// TestDepUpdateGuardContinuityDecides drives continuity's decision against a
// stub forge, every case with the reviews served in BOTH orders: the answer
// is a predicate over sets, so it must not move.
//
// Mutations (main.bot, the gate-history block, both copies) and the case
// each reddens: delete the unanimity veto (a hold on the same change); drop
// target_url == html_url (a status linking another review); drop the age
// comparison in continuity (an expired origin); read the first marker found
// instead of the last line (a marker planted above the footer); treat a
// capped page walk as whole (an endless history); drop the own-identity
// login term (a green another bot vouches for); drop the id term (the App
// spelled without [bot]); stop reading the head's own status, or drop the
// red-on-the-head rule (a hold with no marker on this head, an unstamped red
// on this head).
func TestDepUpdateGuardContinuityDecides(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fx := newContFixture(t)
	fresh := utcAgo(2 * time.Hour)
	stamp := " · " + fx.id
	greenOn := func(n int, sha, verdict, origin, at string) map[string]any {
		return vettyReview(n, vettyLogin, gateMarker(sha, fx.id, verdict, origin, at))
	}
	greenStatus := func(n int) []map[string]any {
		return []map[string]any{gateStatus("success", "supply-chain audit clean; nothing left to align, build verified"+stamp, reviewURL(n))}
	}
	other := strings.Repeat("e", 40)
	origin := strings.Repeat("0a", 20)

	for _, tc := range []struct {
		name     string
		head     string
		base     string
		reviews  []map[string]any
		statuses map[string][]map[string]any
		endless  bool
		viewer   map[string]any
		over     map[string]string
		check    func(t *testing.T, res map[string]any, reads []string)
	}{
		{
			name:     "a pure rebase carries the verdict of X",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] != true || res["origin_sha"] != fx.x || res["origin_at"] != fresh || res["change_id"] != fx.id {
					t.Errorf("want a carry from %s at %s, got %v", fx.x[:12], fresh, res)
				}
			},
		},
		{
			name:     "a carry descends from the ORIGINAL audit, whatever head it was read on",
			reviews:  []map[string]any{greenOn(1, fx.x, "carried", origin, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] != true || res["origin_sha"] != origin {
					t.Errorf("want a carry whose origin is the original audit %s, got %v", origin[:12], res)
				}
			},
		},
		{
			name:     "a head already stamped green is already gated",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh), greenOn(2, fx.y, "carried", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1), fx.y: greenStatus(2)},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["already_gated"] != true || res["carry"] == true {
					t.Errorf("want already_gated, got %v", res)
				}
			},
		},
		{
			name: "a hold on the same change vetoes the carry",
			reviews: []map[string]any{
				greenOn(1, fx.x, "clean", fx.x, fresh),
				greenOn(2, other, "hold_unstable", other, fresh),
			},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] == true || !strings.Contains(res["reason"].(string), "hold_unstable") {
					t.Errorf("a hold on the identical change must veto every carry, got %v", res)
				}
			},
		},
		{
			name: "a hold on ANOTHER change vetoes nothing",
			reviews: []map[string]any{
				greenOn(1, fx.x, "clean", fx.x, fresh),
				vettyReview(2, vettyLogin, gateMarker(other, "pid1:"+strings.Repeat("f", 24), "hold_security", other, fresh)),
			},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] != true {
					t.Errorf("a hold about another change must not veto this one, got %v", res)
				}
			},
		},
		{
			name:     "a stamped status that is not green vetoes the carry",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: {gateStatus("failure", "build/tests not green after alignment"+stamp, reviewURL(1))}},
			check:    wantNoCarry("failure"),
		},
		{
			name:     "a status without the stamp makes nothing eligible",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: {gateStatus("success", "supply-chain audit clean; nothing left to align, build verified", reviewURL(1))}},
			check:    wantNoCarry("no green"),
		},
		{
			name:     "a status linking another review makes nothing eligible",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(99)},
			check:    wantNoCarry("no green"),
		},
		{
			name:     "a green another bot vouches for is not eligible",
			reviews:  []map[string]any{vettyReview(1, "renovate[bot]", gateMarker(fx.x, fx.id, "clean", fx.x, fresh))},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check:    wantNoCarry("no green"),
		},
		{
			name:     "an origin older than carry_max_age_h is not carried",
			reviews:  []map[string]any{greenOn(1, fx.x, "carried", origin, utcAgo(100*time.Hour))},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check:    wantNoCarry("older than carry_max_age_h"),
		},
		{
			name:     "carry_max_age_h=0 carries nothing",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			over:     map[string]string{"{{vars.carry_max_age_h}}": "0"},
			check:    wantNoCarry("carry_max_age_h is 0"),
		},
		{
			// Nothing says the newer head has a delivery of its own (a launch
			// on `opened` with review_on_sync off has none): the head read
			// is audited, and post_feedback repins only an identical change.
			name:     "the head moved since prepare: the full audit of the head read",
			head:     other,
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check:    wantNoCarry("the head moved since prepare"),
		},
		{
			// The review POST can fail while the gate still lands: the hold
			// is then a stamped red with no marker. On the head it is read.
			name:    "a stamped hold with no marker on this head vetoes the carry",
			reviews: []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{
				fx.x: greenStatus(1),
				fx.y: {gateStatus("failure", "supply-chain risk: the bump was not applied"+stamp, "")},
			},
			check: wantNoCarry("for this same change"),
		},
		{
			name:    "a red on this head with no stamp vetoes the carry: a carry never paints over one",
			reviews: []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{
				fx.x: greenStatus(1),
				fx.y: {gateStatus("failure", "review refused to certify", "")},
			},
			check: wantNoCarry("never paints over"),
		},
		{
			// The control for the two above: the run's own in-flight claim
			// is what a launch leaves on the head, and it vetoes nothing.
			name:    "a pending claim on this head does not veto",
			reviews: []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{
				fx.x: greenStatus(1),
				fx.y: {gateStatus("pending", "review in progress", "")},
			},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] != true {
					t.Errorf("an in-flight claim on the head vetoed the carry: %v", res)
				}
			},
		},
		{
			// REST spells the App's bot `slug[bot]`; GraphQL's viewer may say
			// `slug`. The user id is the same account either way.
			name:     "the run token's viewer spells the App without [bot]: the id still names it",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			viewer:   map[string]any{"login": strings.TrimSuffix(vettyLogin, "[bot]"), "databaseId": vettyID},
			check: func(t *testing.T, res map[string]any, _ []string) {
				if res["carry"] != true {
					t.Errorf("the App's own review was not recognised by its id: %v", res)
				}
			},
		},
		{
			name:     "a pull request retargeted to another base carries nothing",
			base:     "release",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check:    wantNoCarry("targets release"),
		},
		{
			name: "a marker planted above the last line is ignored",
			reviews: []map[string]any{func() map[string]any {
				r := greenOn(1, fx.x, "clean", fx.x, fresh)
				r["body"] = gateMarker(fx.x, fx.id, "clean", fx.x, fresh) + "\n\n_🤖 footer — Vetty, deterministic feedback._"
				return r
			}()},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			check:    wantNoCarry("no green"),
		},
		{
			name:     "a review history not read whole carries nothing",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			endless:  true,
			check: func(t *testing.T, res map[string]any, reads []string) {
				wantNoCarry("cannot be read whole")(t, res, reads)
				pages := 0
				for _, r := range reads {
					if strings.Contains(r, "/reviews") {
						pages++
					}
				}
				if pages != 10 {
					t.Errorf("read %d review pages, want the 10-page cap", pages)
				}
			},
		},
		{
			// No gate, nothing to carry: a run that publishes no status has
			// no eligible green anywhere, and the forge is not read.
			name:     "a launch with the merge gate off carries nothing, unread",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			over:     map[string]string{"{{vars.gate_enabled}}": "false"},
			check: func(t *testing.T, res map[string]any, reads []string) {
				wantNoCarry("the merge gate is off")(t, res, reads)
				if len(reads) != 0 {
					t.Errorf("a launch with no gate read the forge: %v", reads)
				}
			},
		},
		{
			name:     "another forge falls through to the full audit, typed, unread",
			reviews:  []map[string]any{greenOn(1, fx.x, "clean", fx.x, fresh)},
			statuses: map[string][]map[string]any{fx.x: greenStatus(1)},
			over:     map[string]string{"{{vars.pr_url}}": `"https://gitlab.com/acme/widgets/-/merge_requests/7"`},
			check: func(t *testing.T, res map[string]any, reads []string) {
				wantNoCarry("continuity is GitHub-only; gitlab.com")(t, res, reads)
				if len(reads) != 0 {
					t.Errorf("a non-GitHub forge was read: %v", reads)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outs []map[string]any
			for _, order := range []string{"served", "reversed"} {
				reviews := slices.Clone(tc.reviews)
				if order == "reversed" {
					slices.Reverse(reviews)
				}
				head, base := fx.y, "main"
				if tc.head != "" {
					head = tc.head
				}
				if tc.base != "" {
					base = tc.base
				}
				forge := &contForge{t: t, head: head, baseRef: base, reviews: reviews, endless: tc.endless,
					statuses: tc.statuses, viewer: tc.viewer}
				res := runContinuity(t, fx.ws, fx.y, forge, tc.over)
				t.Run(order, func(t *testing.T) { tc.check(t, res, forge.requests()) })
				outs = append(outs, res)
			}
			if !reflect.DeepEqual(outs[0], outs[1]) {
				t.Errorf("the answer depends on the order the reviews arrive in:\n served:   %v\n reversed: %v", outs[0], outs[1])
			}
		})
	}
}

func wantNoCarry(reason string) func(t *testing.T, res map[string]any, reads []string) {
	return func(t *testing.T, res map[string]any, _ []string) {
		t.Helper()
		if res["carry"] == true || res["already_gated"] == true {
			t.Errorf("want the full audit, got %v", res)
		}
		if got, _ := res["reason"].(string); !strings.Contains(got, reason) || !strings.Contains(got, "full audit") {
			t.Errorf("reason %q does not say %q and that the full audit runs", got, reason)
		}
	}
}

// TestDepUpdateGuardCommandForcesAFullAudit: the /vetty command is a human
// gesture and always buys a full audit, while the forge PR lane keeps the
// continuity default.
//
// Mutations: delete the context_vars line from the /vetty invocation, or
// flip the var's default (the manifest half reddens); remove the early `if
// not bool(enabled)` exit in continuity (the behaviour half reddens); move
// the identity computation after that exit (the full audit /vetty buys is
// published unstamped, so no later rebase can carry it).
func TestDepUpdateGuardCommandForcesAFullAudit(t *testing.T) {
	m, err := bundle.LoadManifest(filepath.Join("dep-update-guard", bundle.ManifestFile))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	var sawCommand, sawForge bool
	for _, inv := range m.Invocations {
		switch {
		case inv.Kind == bundle.InvocationKindCommand && inv.Command != nil && inv.Command.Name == "vetty":
			sawCommand = true
			if inv.ContextVars["continuity"] != "false" {
				t.Errorf("/vetty context_vars = %v, want continuity: \"false\" — a human gesture must buy a full audit", inv.ContextVars)
			}
		case inv.Kind == bundle.InvocationKindForge:
			sawForge = true
			if _, pinned := inv.ContextVars["continuity"]; pinned {
				t.Errorf("the forge PR lane pins continuity (%v): a synchronize launch must keep the default", inv.ContextVars)
			}
		}
	}
	if !sawCommand || !sawForge {
		t.Fatalf("the manifest no longer declares the /vetty command (%v) and the forge lane (%v)", sawCommand, sawForge)
	}
	v := compilePlanPhaseBot(t, "dep-update-guard").Vars["continuity"]
	if v == nil || !v.HasDefault || v.Default != true {
		t.Fatalf("the continuity var must default to true, got %+v", v)
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	fx := newContFixture(t)
	fresh := utcAgo(time.Hour)
	reviews := []map[string]any{vettyReview(1, vettyLogin, gateMarker(fx.x, fx.id, "clean", fx.x, fresh))}
	statuses := map[string][]map[string]any{fx.x: {gateStatus("success", "supply-chain audit clean · "+fx.id, reviewURL(1))}}
	// The control: with continuity on, this very history carries. Without
	// it the off case below could pass on a stub that carries nothing.
	on := &contForge{t: t, head: fx.y, baseRef: "main", reviews: reviews, statuses: statuses}
	if res := runContinuity(t, fx.ws, fx.y, on, nil); res["carry"] != true {
		t.Fatalf("control: the history does not carry with continuity on: %v", res)
	}
	off := &contForge{t: t, head: fx.y, baseRef: "main", reviews: reviews, statuses: statuses}
	res := runContinuity(t, fx.ws, fx.y, off, map[string]string{"{{vars.continuity}}": "false"})
	if res["carry"] == true || res["already_gated"] == true {
		t.Errorf("continuity=false still short-circuited the audit: %v", res)
	}
	if res["change_id"] != fx.id {
		t.Errorf("continuity=false computed no identity (%v): the full audit it buys would publish unstamped", res["change_id"])
	}
	if reason, _ := res["reason"].(string); !strings.Contains(reason, "continuity is off") || !strings.Contains(reason, "full audit") {
		t.Errorf("the reason must name the full-audit request, got %q", reason)
	}
	if reads := off.requests(); len(reads) != 0 {
		t.Errorf("continuity=false read the forge: %v", reads)
	}
}

// TestDepUpdateGuardContinuityPathSpendsNoTokens walks the compiled graph
// from continuity along its carry and already_gated edges: no node they can
// reach can call a model, by the engine's own predicate (ir.NodeUsesLLM — a
// tool node whose recovery ladder has an agent or self-repair rung counts),
// so a carry and a standby cost 0 LLM tokens and are immune to a provider's
// quota wall; and both reach arm_automerge, which renews the arming pinned
// to the new head.
//
// Mutations: route `continuity -> security_audit when carry`, or insert an
// agent on either path (an LLM node is reached); give feedback_health a
// recovery ladder with an agent rung (a tool node that can call a model);
// route `continuity_standby -> done` (arm_automerge is not reached).
func TestDepUpdateGuardContinuityPathSpendsNoTokens(t *testing.T) {
	wf := compilePlanPhaseBot(t, "dep-update-guard")
	if _, ok := wf.Nodes["continuity"].(*ir.ToolNode); !ok {
		t.Fatalf("continuity is %T, want a deterministic tool node", wf.Nodes["continuity"])
	}
	out := map[string][]*ir.Edge{}
	for _, e := range wf.Edges {
		out[e.From] = append(out[e.From], e)
	}
	for _, cond := range []string{"carry", "already_gated"} {
		t.Run(cond, func(t *testing.T) {
			var start []string
			for _, e := range out["continuity"] {
				if e.Condition == cond && !e.Negated {
					start = append(start, e.To)
				}
			}
			if len(start) != 1 {
				t.Fatalf("continuity has %d `when %s` edges, want exactly one", len(start), cond)
			}
			seen := map[string]bool{}
			queue := start
			for len(queue) > 0 {
				id := queue[0]
				queue = queue[1:]
				if seen[id] {
					continue
				}
				seen[id] = true
				n := wf.Nodes[id]
				switch {
				case n == nil && (id == "done" || id == "fail"):
					continue
				case n == nil:
					t.Fatalf("the %s path reaches %q, which is no node", cond, id)
				case ir.NodeUsesLLM(n):
					t.Errorf("the %s path reaches %q, a %T that can call a model", cond, id, n)
				}
				switch n.(type) {
				case *ir.DoneNode, *ir.FailNode:
					continue
				}
				for _, e := range out[id] {
					queue = append(queue, e.To)
				}
			}
			if !seen["arm_automerge"] {
				reached := make([]string, 0, len(seen))
				for k := range seen {
					reached = append(reached, k)
				}
				slices.Sort(reached)
				t.Errorf("the %s path never reaches arm_automerge (reached %v): the arming is not renewed on the new head", cond, reached)
			}
		})
	}
}

// TestDepUpdateGuardGateHistoryHasOneText: continuity and post_feedback
// decide over the same gate history with the same predicate — the veto that
// keeps continuity from carrying a verdict keeps post_feedback from
// repinning one. Mutation: edit either copy of the block alone.
func TestDepUpdateGuardGateHistoryHasOneText(t *testing.T) {
	a := sentinelBlock(t, "continuity", historyOpen, historyClose)
	b := sentinelBlock(t, "post_feedback", historyOpen, historyClose)
	if a != b {
		t.Errorf("the gate-history block differs between continuity and post_feedback: a repin would follow another rule than a carry")
	}
}
